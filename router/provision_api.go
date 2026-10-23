package router

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/Loomux/server/registry"
)

// Provisioning a workspace through the API (LOOM-215): what a chat's
// provision_workspace does, asked for directly, on a target the caller
// chooses. The request returns once the row is written (status
// provisioning); the rest runs in the background, recorded under a
// conversation of its own, and ends on the row: idle with its path, or
// failed with why.

// defaultProvisionPrepareTimeout bounds the API's start-and-check of the
// target before its recipe runs, unless WithProvisionPrepareTimeout says
// otherwise: with the recipe's ProvisionTimeout it stays inside the
// reaper's staleProvisioningAfter (ProvisionTimeout + 5 min) counted
// from the row's creation, as a reaper without a preparing limit counts
// it. loomuxd sets it past a machine's ready wait with an image pull,
// and the reaper's preparing limit past that (LOOM-242).
const defaultProvisionPrepareTimeout = 4 * time.Minute

// ErrNoSuchTarget: the target to provision on doesn't exist.
var ErrNoSuchTarget = errors.New("router: no such target")

// SpecError is a provisioning spec the API caller must correct.
type SpecError struct{ Err error }

func (e *SpecError) Error() string { return e.Err.Error() }
func (e *SpecError) Unwrap() error { return e.Err }

// NameTakenError: a workspace already has the name, and isn't the one
// this request would make.
type NameTakenError struct {
	Name      string
	Workspace *registry.Workspace
}

func (e *NameTakenError) Error() string {
	ws := e.Workspace
	switch {
	case ws.Status == registry.WorkspaceStatusFailed:
		return fmt.Sprintf("workspace %q exists and failed (%s): delete it to provision it again", e.Name, ws.StatusReason)
	case ws.Status == registry.WorkspaceStatusArchived:
		return fmt.Sprintf("workspace %q exists, archived: set it idle, or delete it to provision it again", e.Name)
	}
	return fmt.Sprintf("workspace %q exists on another target or from another source", e.Name)
}

// Provisioning is what ProvisionWorkspace started: the workspace, the
// conversation its provisioning is recorded under, and Run, which does
// it (nil when Existing: the same workspace was asked for before, and
// is provisioning or provisioned already).
type Provisioning struct {
	Workspace      *registry.Workspace
	ConversationID string
	Existing       bool
	Run            func(ctx context.Context)
}

// ProvisionWorkspace validates spec, resolves its target and writes the
// workspace row (status provisioning), returning Run for the caller to
// run in the background: it starts the target if it is a stopped
// machine, checks it, and runs the recipe, as a chat's
// provision_workspace does. The same name on the same target, from the
// same source, isn't made twice: the workspace already there is
// returned (Existing), unless it failed or was archived. Errors:
// ErrNoSuchTarget, *SpecError, *NameTakenError, or the store's.
func (r *Router) ProvisionWorkspace(ctx context.Context, spec ProvisionSpec) (*Provisioning, error) {
	conversationID := uuid.NewString()
	log := r.logger.With("conversation_id", conversationID, "target_id", spec.TargetID, "workspace_name", spec.Name, "source", "api")
	if err := spec.Validate(); err != nil {
		return nil, &SpecError{Err: err}
	}
	if spec.TargetID == "" {
		return nil, &SpecError{Err: errors.New("target_id is required")}
	}
	target, err := r.store.GetTarget(ctx, spec.TargetID)
	if errors.Is(err, registry.ErrNotFound) {
		return nil, ErrNoSuchTarget
	}
	if err != nil {
		return nil, err
	}
	if existing, err := r.sameWorkspace(ctx, spec); existing != nil || err != nil {
		return existing, err
	}
	ws, err := r.createProvisioningRow(ctx, log, spec)
	if errors.Is(err, registry.ErrConflict) {
		// Another request made it meanwhile: answer as if it came first.
		if existing, err := r.sameWorkspace(ctx, spec); existing != nil || err != nil {
			return existing, err
		}
	}
	if err != nil {
		return nil, err
	}
	log = log.With("workspace_id", ws.ID)
	log.Info("workspace provisioning requested", "kind", string(spec.Kind))
	return &Provisioning{Workspace: ws, ConversationID: conversationID, Run: func(ctx context.Context) {
		_ = r.runProvisioning(ctx, log, conversationID, target, ws, spec, true)
	}}, nil
}

// sameWorkspace is the workspace already named spec.Name, as Existing
// when it is the one spec asks for, or a *NameTakenError when it isn't;
// nil, nil when the name is free.
func (r *Router) sameWorkspace(ctx context.Context, spec ProvisionSpec) (*Provisioning, error) {
	all, err := r.store.ListWorkspaces(ctx)
	if err != nil {
		return nil, err
	}
	for _, ws := range all {
		if ws.Name != spec.Name {
			continue
		}
		same := ws.TargetID == spec.TargetID && ws.GitRemote == spec.GitRemote &&
			ws.Status != registry.WorkspaceStatusFailed && ws.Status != registry.WorkspaceStatusArchived
		if !same {
			return nil, &NameTakenError{Name: spec.Name, Workspace: ws}
		}
		return &Provisioning{Workspace: ws, Existing: true}, nil
	}
	return nil, nil
}
