package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/targets"
)

// Migrating a config-mode target to a managed one (LOOM-138, docs/design/
// target-onboarding.md "Migration"): POST /targets/{id}/migrate-ssh reads
// what the mounted SSH config does for it, imports the key it uses and
// pins its known host key, so the machine needs no change. A dry run
// only reports the plan; applying it tests the target, managed, and puts
// it back as it was if that fails.

// SSHConfigResolver plans a config-mode target as a managed one —
// targets.ResolveSSHConfig.
type SSHConfigResolver func(ctx context.Context, t *registry.Target) (*targets.SSHConfigPlan, error)

// WithSSHMigration enables POST /targets/{id}/migrate-ssh.
func WithSSHMigration(resolve SSHConfigResolver) Option {
	return func(s *Server) { s.resolveSSHConfig = resolve }
}

type migrateSSHRequest struct {
	// DryRun reports the plan and changes nothing.
	DryRun bool `json:"dry_run"`
}

type migrateSSHKeyResponse struct {
	Type        string `json:"type"`
	Fingerprint string `json:"fingerprint"`
	// SourceFile is the key file the SSH config uses.
	SourceFile string `json:"source_file"`
	// ExistingKeyID is the managed key already holding it, if one does
	// (another target migrated before); "" if it would be imported.
	ExistingKeyID string `json:"existing_key_id"`
}

type migrateSSHPlanResponse struct {
	Host     string                 `json:"host"`
	SSHPort  int                    `json:"ssh_port"`
	User     string                 `json:"user"`
	SSHProxy string                 `json:"ssh_proxy"`
	Key      *migrateSSHKeyResponse `json:"key"`
	HostKeys []hostKeyResponse      `json:"host_keys"`
}

type migrateSSHResponse struct {
	TargetID string `json:"target_id"`
	DryRun   bool   `json:"dry_run"`
	// CanApply is false while Problems lists anything.
	CanApply bool                   `json:"can_apply"`
	Problems []string               `json:"problems"`
	Plan     migrateSSHPlanResponse `json:"plan"`
	// Applied: the target is now managed. RolledBack: it was, failed its
	// test, and is back on the SSH config as before.
	Applied    bool                `json:"applied"`
	RolledBack bool                `json:"rolled_back"`
	Test       *testTargetResponse `json:"test"`
	Target     *targetResponse     `json:"target"`
}

// handleMigrateSSH: POST /api/v1/targets/{id}/migrate-ssh {dry_run}.
func (s *Server) handleMigrateSSH(w http.ResponseWriter, r *http.Request) {
	if s.resolveSSHConfig == nil || s.sshKeys == nil {
		writeError(w, http.StatusNotImplemented, "migrating targets to Loomux SSH keys is not configured on this server")
		return
	}
	var req migrateSSHRequest
	if !readJSON(w, r, &req) {
		return
	}
	ctx := r.Context()
	existing, err := s.targets.GetTarget(ctx, r.PathValue("id"))
	if errors.Is(err, registry.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such target")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not fetch target")
		return
	}
	if existing.Kind != registry.TargetKindRemote || existing.Managed() {
		writeError(w, http.StatusBadRequest, "only a remote target reached through the SSH config can be migrated")
		return
	}
	plan, err := s.resolveSSHConfig(ctx, existing)
	if err != nil {
		writeError(w, http.StatusBadGateway, "could not read the SSH config for this target: "+err.Error())
		return
	}
	resp := migrateSSHResponse{TargetID: existing.ID, DryRun: req.DryRun, Problems: append([]string{}, plan.Problems...)}
	resp.CanApply = len(resp.Problems) == 0
	resp.Plan = migrateSSHPlanResponse{Host: plan.Host, SSHPort: plan.Port, User: plan.User, SSHProxy: apiSSHProxy(plan.SSHProxy)}
	keys, _ := targets.ParseHostKeys(plan.HostKeys)
	resp.Plan.HostKeys = hostKeyResponses(keys)
	existingKeyID := ""
	if plan.Key != nil {
		if existingKeyID, err = s.keyWithFingerprint(ctx, plan.Key.Fingerprint); err != nil {
			writeError(w, http.StatusInternalServerError, "could not list SSH keys")
			return
		}
		resp.Plan.Key = &migrateSSHKeyResponse{Type: plan.Key.Type, Fingerprint: plan.Key.Fingerprint, SourceFile: plan.KeyFile, ExistingKeyID: existingKeyID}
	}
	if req.DryRun {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	if !resp.CanApply || plan.Key == nil {
		writeJSON(w, http.StatusConflict, resp)
		return
	}
	if s.targetProber == nil {
		writeError(w, http.StatusNotImplemented, "migrating needs target probing, which is not configured on this server")
		return
	}
	hk := s.hostKeys
	if hk == nil {
		hk, _ = s.targets.(HostKeyStore)
	}
	if hk == nil {
		writeError(w, http.StatusNotImplemented, "migrating needs host key pinning, which is not configured on this server")
		return
	}

	// Import the key, unless a managed key already holds it.
	keyID, imported := existingKeyID, (*registry.SSHKey)(nil)
	if keyID == "" {
		imported = plan.Key
		imported.ID = uuid.NewString()
		if err := s.sshKeys.CreateSSHKey(ctx, imported); err != nil {
			if errors.Is(err, registry.ErrConflict) { // the name is taken
				imported.Name = imported.Name + "-" + imported.ID[:8]
				err = s.sshKeys.CreateSSHKey(ctx, imported)
			}
			if err != nil {
				if errors.Is(err, registry.ErrNoMasterKey) {
					writeError(w, http.StatusServiceUnavailable, "storing SSH keys needs LOOMUX_MASTER_KEY, which this server doesn't have")
					return
				}
				writeError(w, http.StatusInternalServerError, "could not store the imported SSH key")
				return
			}
		}
		keyID = imported.ID
	}

	migrated := *existing
	migrated.Host, migrated.SSHPort, migrated.User = plan.Host, plan.Port, plan.User
	migrated.SSHKeyRef, migrated.SSHProxy = keyID, plan.SSHProxy
	rollback := func() {
		bg := context.Background()
		_ = s.targets.UpdateTarget(bg, existing)
		_ = hk.SetTargetHostKeys(bg, existing.ID, existing.HostKeys)
		if imported != nil {
			_ = s.sshKeys.DeleteSSHKey(bg, imported.ID)
			if s.dropSSHKey != nil {
				s.dropSSHKey(imported.ID)
			}
		}
	}
	if err := migrated.Validate(); err != nil {
		rollback()
		writeError(w, http.StatusConflict, "the planned target isn't valid: "+err.Error())
		return
	}
	if err := s.targets.UpdateTarget(ctx, &migrated); err != nil {
		rollback()
		writeTargetWriteError(w, err, "update")
		return
	}
	if err := hk.SetTargetHostKeys(ctx, existing.ID, plan.HostKeys); err != nil {
		rollback()
		writeError(w, http.StatusInternalServerError, "could not pin the target's host key")
		return
	}
	s.forgetScan(existing.ID)

	h, _, err := s.targetProber.ProbeTarget(ctx, existing.ID)
	if err != nil || h == nil {
		rollback()
		writeError(w, http.StatusInternalServerError, "could not test the migrated target; it is back on the SSH config")
		return
	}
	test := newTestTargetResponse(h)
	resp.Test = &test
	if !test.Reachable || h.Error != "" {
		rollback()
		resp.RolledBack = true
		writeJSON(w, http.StatusBadGateway, resp)
		return
	}
	resp.Applied = true
	if stored, err := s.targets.GetTarget(ctx, existing.ID); err == nil {
		if tr, err := s.targetResponseFor(ctx, stored); err == nil {
			resp.Target = &tr
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// keyWithFingerprint is the id of the managed key with fingerprint, or "".
func (s *Server) keyWithFingerprint(ctx context.Context, fingerprint string) (string, error) {
	keys, err := s.sshKeys.ListSSHKeys(ctx)
	if err != nil {
		return "", err
	}
	for _, k := range keys {
		if k.Fingerprint == fingerprint {
			return k.ID, nil
		}
	}
	return "", nil
}
