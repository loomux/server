package router_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/router"
	"github.com/Loomux/server/router/routertest"
)

// createWorkspaceRecorder wraps a registry.Store and records every
// CreateWorkspace attempt. The SQLite backend's target_id foreign key
// would reject an unknown target on its own — but registry.Store is
// pluggable and not every backend (or every pooled SQLite connection)
// enforces it, so the Router must never *attempt* the write for an
// unresolvable target; that attempt is what this observes.
type createWorkspaceRecorder struct {
	registry.Store
	attempts []*registry.Workspace
}

func (s *createWorkspaceRecorder) CreateWorkspace(ctx context.Context, ws *registry.Workspace) error {
	s.attempts = append(s.attempts, ws)
	return s.Store.CreateWorkspace(ctx, ws)
}

// setupRecording is setup with the Router's store wrapped in a
// createWorkspaceRecorder.
func setupRecording(t *testing.T) (*createWorkspaceRecorder, *fakeExecutor, *router.Router, *routertest.StubRoutingModel) {
	t.Helper()
	store := &createWorkspaceRecorder{Store: newTestStore(t)}
	exec, r, model := newRouter(t, store)
	return store, exec, r, model
}

// TestDispatch_ProvisionUnknownTarget_WritesNoWorkspaceRow is the LOOM-64
// regression: a provision_workspace decision naming a target_id that
// isn't registered used to create the workspace row first and only fail
// later at GetTarget, orphaning the row permanently. Router must not
// trust the model's target_id — it resolves it before writing anything.
// (Contrast TestDispatch_ProvisioningFailure_LeavesWorkspaceRowInPlace:
// a *valid* target whose provisioning task fails still keeps its row for
// inspection.)
func TestDispatch_ProvisionUnknownTarget_WritesNoWorkspaceRow(t *testing.T) {
	for _, tc := range []struct {
		name     string
		targetID string
	}{
		{name: "unknown", targetID: "sc1"},
		{name: "empty", targetID: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, exec, r, model := setupRecording(t)
			createFixtureTarget(t, store) // some target exists, just not the one named

			model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
				return router.Decision{
					Action: router.ActionProvisionWorkspace,
					NewWorkspace: router.ProvisionSpec{
						Name:     "orphan-ws",
						TargetID: tc.targetID, Kind: router.ProvisionEmpty,
					},
					AgentType: "claude-code",
				}, nil
			}

			if _, err := r.Dispatch(context.Background(), "conv-1", "start a new workspace"); err == nil {
				t.Fatalf("Dispatch with target_id %q: got nil error", tc.targetID)
			}

			if len(store.attempts) != 0 {
				t.Fatalf("CreateWorkspace attempted %d time(s) for an unresolvable target, want 0", len(store.attempts))
			}
			list, err := store.ListWorkspaces(context.Background())
			if err != nil {
				t.Fatalf("ListWorkspaces: %v", err)
			}
			if len(list) != 0 {
				t.Fatalf("ListWorkspaces = %+v, want no workspace row written for an unresolvable target", list)
			}
			if len(exec.sessions) != 0 {
				t.Fatalf("a session was launched despite an unresolvable target: %+v", exec.sessions)
			}
		})
	}
}

// TestDispatch_PassesRegisteredTargetsToModel proves the routing model is
// told which targets exist (LOOM-64) — and only their id, name and kind.
func TestDispatch_PassesRegisteredTargetsToModel(t *testing.T) {
	store, _, r, model := setup(t)
	remote := &registry.Target{
		ID:        uuid.NewString(),
		Name:      "bigbox",
		Kind:      registry.TargetKindRemote,
		Host:      "bigbox.example.invalid",
		User:      "loomux",
		SSHKeyRef: "vault:bigbox-key",
	}
	if err := store.CreateTarget(context.Background(), remote); err != nil {
		t.Fatalf("CreateTarget: %v", err)
	}

	model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{Action: router.ActionAnswerDirectly, DirectAnswer: "ok"}, nil
	}
	if _, err := r.Dispatch(context.Background(), "conv-1", "hi"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	got := model.LastDecideTargets
	if len(got) != 1 || got[0].ID != remote.ID || got[0].Name != "bigbox" || got[0].Kind != "remote" {
		t.Fatalf("LastDecideTargets = %+v, want one snapshot of %s (bigbox, remote)", got, remote.ID)
	}
	// Formatted with %+v so this keeps checking whatever fields
	// TargetSnapshot grows: the snapshot is what reaches the third-party
	// router vendor, so host, user and key ref must not be in it.
	rendered := fmt.Sprintf("%+v", got)
	for _, unwanted := range []string{"bigbox.example.invalid", "loomux", "vault:bigbox-key"} {
		if strings.Contains(rendered, unwanted) {
			t.Errorf("target snapshot carries %q, want only id/name/kind: %s", unwanted, rendered)
		}
	}
}
