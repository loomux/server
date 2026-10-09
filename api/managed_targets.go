package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/targets"
)

// Managed targets through the API (LOOM-138, docs/design/
// target-onboarding.md): a target with a Loomux SSH key is reached with
// that key and no ssh_config; its responses show the key's public parts
// and what onboarding step is next.

// WithSSHKeyDropper is told when a managed SSH key is deleted, to stop
// serving it (App.DropSSHKey).
func WithSSHKeyDropper(drop func(id string)) Option {
	return func(s *Server) { s.dropSSHKey = drop }
}

// targetSSHKeyResponse is a managed target's key: its public parts only.
type targetSSHKeyResponse struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Type        string `json:"type"`
	Fingerprint string `json:"fingerprint"`
	PublicKey   string `json:"public_key"`
}

// Onboarding steps (targetResponse.NextStep).
const (
	nextStepPinHostKey     = "pin_host_key"
	nextStepAuthorizeKey   = "authorize_key"
	nextStepTestConnection = "test_connection"
)

// targetResponses renders ts with their health, keys and onboarding
// state, reading health and keys once for all of them.
func (s *Server) targetResponses(ctx context.Context, ts []*registry.Target) ([]targetResponse, error) {
	healths, err := s.targets.ListTargetHealth(ctx)
	if err != nil {
		return nil, err
	}
	healthByTarget := make(map[string]*registry.TargetHealth, len(healths))
	for _, h := range healths {
		healthByTarget[h.TargetID] = h
	}
	keys := map[string]*registry.SSHKey{}
	if s.sshKeys != nil {
		list, err := s.sshKeys.ListSSHKeys(ctx)
		if err != nil {
			return nil, err
		}
		for _, k := range list {
			keys[k.ID] = k
		}
	}
	out := make([]targetResponse, 0, len(ts))
	for _, t := range ts {
		resp := newTargetResponse(t)
		h := healthByTarget[t.ID]
		if h != nil {
			resp.Health = newTargetHealthResponse(h)
		}
		if t.Managed() {
			resp.SSHKey = &targetSSHKeyResponse{ID: t.SSHKeyRef}
			if k := keys[t.SSHKeyRef]; k != nil {
				resp.SSHKey = &targetSSHKeyResponse{ID: k.ID, Name: k.Name, Type: k.Type, Fingerprint: k.Fingerprint, PublicKey: k.PublicKey}
			}
		}
		resp.Ready, resp.NextStep = onboardingState(t, h)
		out = append(out, resp)
	}
	s.decorateMachines(ctx, out)
	return out, nil
}

// targetResponseFor is targetResponses for one target.
func (s *Server) targetResponseFor(ctx context.Context, t *registry.Target) (targetResponse, error) {
	out, err := s.targetResponses(ctx, []*registry.Target{t})
	if err != nil {
		return targetResponse{}, err
	}
	return out[0], nil
}

// onboardingState says whether t is ready to work on and, if not, what to
// do next, from its latest probe — one older than the target's last
// change (a new pin, a new address) says nothing about it.
func onboardingState(t *registry.Target, h *registry.TargetHealth) (bool, *string) {
	step := func(s string) *string { return &s }
	fresh := h != nil && !h.ProbedAt.Before(t.UpdatedAt)
	switch {
	case t.Kind != registry.TargetKindRemote:
		if fresh && h.Error == "" {
			return true, nil
		}
		return false, step(nextStepTestConnection)
	case t.Managed() && strings.TrimSpace(t.HostKeys) == "":
		return false, step(nextStepPinHostKey)
	case !fresh:
		return false, step(nextStepTestConnection)
	case h.Reachable && h.TmuxVersion != "" && h.Error == "":
		return true, nil
	case strings.Contains(h.Error, string(targets.SSHAuthFailed)):
		return false, step(nextStepAuthorizeKey)
	case strings.Contains(h.Error, string(targets.SSHHostKeyChanged)), strings.Contains(h.Error, string(targets.SSHHostKeyUnknown)):
		return false, step(nextStepPinHostKey)
	default:
		return false, step(nextStepTestConnection)
	}
}

// sshKeyNameFor is a key name made from a target's name: what a key
// name can't hold becomes "-".
func sshKeyNameFor(targetName string) string {
	b := []byte(targetName)
	for i, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			b[i] = '-'
		}
	}
	name := strings.TrimLeft(string(b), "-._")
	if len(name) > 48 {
		name = name[:48]
	}
	if name == "" {
		name = "target"
	}
	return name
}

// generateTargetKey makes and stores a key for the target named
// targetName, writing the error response itself when it can't.
func (s *Server) generateTargetKey(w http.ResponseWriter, ctx context.Context, targetName string) (*registry.SSHKey, bool) {
	if s.sshKeys == nil {
		writeError(w, http.StatusNotImplemented, "SSH keys are not available on this server")
		return nil, false
	}
	name := sshKeyNameFor(targetName)
	for attempt := 0; ; attempt++ {
		k, err := targets.GenerateSSHKey(uuid.NewString(), name)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "could not generate SSH key")
			return nil, false
		}
		k.Origin = registry.SSHKeyOriginTarget
		err = s.sshKeys.CreateSSHKey(ctx, k)
		switch {
		case err == nil:
			return k, true
		case errors.Is(err, registry.ErrNoMasterKey):
			writeError(w, http.StatusServiceUnavailable, "storing SSH keys needs LOOMUX_MASTER_KEY, which this server doesn't have")
			return nil, false
		case errors.Is(err, registry.ErrConflict) && attempt < 3:
			name = sshKeyNameFor(targetName) + "-" + uuid.NewString()[:8]
		default:
			writeError(w, http.StatusInternalServerError, "could not store SSH key")
			return nil, false
		}
	}
}

// discardKey deletes a key generated for a target that then wasn't
// written; best effort.
func (s *Server) discardKey(k *registry.SSHKey) {
	if k != nil {
		_ = s.sshKeys.DeleteSSHKey(context.Background(), k.ID)
	}
}

// releaseTargetKey deletes the key a target no longer uses, if it was
// made for that target (generate_ssh_key): nothing else knows it. A key
// made through /ssh-keys stays, as does one another target still uses
// (the store refuses that delete). Best effort.
func (s *Server) releaseTargetKey(ctx context.Context, keyID string) {
	if keyID == "" || s.sshKeys == nil {
		return
	}
	keys, err := s.sshKeys.ListSSHKeys(ctx)
	if err != nil {
		return
	}
	for _, k := range keys {
		if k.ID == keyID && k.Origin == registry.SSHKeyOriginTarget {
			if s.sshKeys.DeleteSSHKey(ctx, keyID) == nil && s.dropSSHKey != nil {
				s.dropSSHKey(keyID)
			}
			return
		}
	}
}

// forgetScan drops a target's latest host key scan: after its address
// or the way it's reached changes, that scan says nothing about it.
func (s *Server) forgetScan(id string) {
	if s.scans == nil {
		return
	}
	s.scans.mu.Lock()
	defer s.scans.mu.Unlock()
	delete(s.scans.byTarget, id)
}

// writeTargetWriteError answers a failed CreateTarget or UpdateTarget.
func writeTargetWriteError(w http.ResponseWriter, err error, verb string) {
	var conflict *registry.ConflictError
	switch {
	case errors.Is(err, registry.ErrNotFound):
		writeError(w, http.StatusNotFound, "no such target")
	case errors.As(err, &conflict):
		writeError(w, http.StatusBadRequest, conflict.Reason)
	case errors.Is(err, registry.ErrConflict):
		writeError(w, http.StatusConflict, "a target with that name already exists")
	default:
		writeError(w, http.StatusInternalServerError, "could not "+verb+" target")
	}
}

// testStep is one step of a connection test.
type testStep struct {
	// Name is connect, host_key, auth or tmux.
	Name string `json:"name"`
	// Status is ok, failed or skipped (after a failed step).
	Status string `json:"status"`
	// Error is what went wrong, with a hint what to do, on the failed
	// step.
	Error string `json:"error,omitempty"`
}

// testSteps breaks a probe down into the steps of reaching a target, so
// a person sees which one failed.
func testSteps(h *registry.TargetHealth) []testStep {
	names := []string{"connect", "host_key", "auth", "tmux"}
	failed := -1
	switch {
	case h.Reachable && h.TmuxVersion != "" && h.Error == "":
	case strings.Contains(h.Error, string(targets.SSHAuthFailed)):
		failed = 2
	case strings.Contains(h.Error, string(targets.SSHHostKeyChanged)), strings.Contains(h.Error, string(targets.SSHHostKeyUnknown)):
		failed = 1
	case !h.Reachable:
		failed = 0
	default:
		failed = 3
	}
	steps := make([]testStep, len(names))
	for i, n := range names {
		steps[i] = testStep{Name: n, Status: "ok"}
		switch {
		case failed < 0 || i < failed:
		case i == failed:
			steps[i].Status = "failed"
			steps[i].Error = h.Error
			if steps[i].Error == "" {
				steps[i].Error = "unreachable"
			}
		default:
			steps[i].Status = "skipped"
		}
	}
	return steps
}
