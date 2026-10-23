package router_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/router"
	"github.com/Loomux/server/targets"
)

// LOOM-215: a workspace provisioned through the API, on a target the
// caller names.

func apiSpec(h *availabilityHarness, name string) router.ProvisionSpec {
	return router.ProvisionSpec{Name: name, TargetID: h.target.ID, Kind: router.ProvisionEmpty, Description: "from the API"}
}

func TestProvisionWorkspaceAPI_Provisions(t *testing.T) {
	h := newAvailabilityHarness(t)
	ctx := context.Background()
	p, err := h.r.ProvisionWorkspace(ctx, apiSpec(h, "api-ws"))
	if err != nil {
		t.Fatalf("ProvisionWorkspace: %v", err)
	}
	if p.Existing || p.Run == nil || p.ConversationID == "" || p.Workspace.Status != registry.WorkspaceStatusProvisioning {
		t.Fatalf("provisioning = %+v", p)
	}
	ws := h.workspaces(t)
	if len(ws) != 1 || ws[0].Status != registry.WorkspaceStatusProvisioning || ws[0].Description != "from the API" {
		t.Fatalf("before the run: %+v", ws)
	}
	p.Run(ctx)
	ws = h.workspaces(t)
	if len(ws) != 1 || ws[0].Status != registry.WorkspaceStatusIdle || ws[0].Path == "" {
		t.Fatalf("after the run: %+v, want idle with its path", ws[0])
	}
	// The run is recorded under its own conversation.
	tasks, _ := h.store.ListTasks(ctx)
	var mine int
	for _, task := range tasks {
		if task.ConversationID == p.ConversationID && task.WorkspaceID == ws[0].ID {
			mine++
		}
	}
	if mine != 1 {
		t.Errorf("provisioning tasks under the conversation = %d, want 1", mine)
	}
}

// The same request again is the same workspace, not a second one; the
// name elsewhere, or a workspace that failed, is a conflict.
func TestProvisionWorkspaceAPI_Idempotent(t *testing.T) {
	h := newAvailabilityHarness(t)
	ctx := context.Background()
	first, err := h.r.ProvisionWorkspace(ctx, apiSpec(h, "api-ws"))
	if err != nil {
		t.Fatal(err)
	}
	again, err := h.r.ProvisionWorkspace(ctx, apiSpec(h, "api-ws"))
	if err != nil || !again.Existing || again.Run != nil || again.Workspace.ID != first.Workspace.ID {
		t.Fatalf("the same request again = %+v, %v; want the same workspace, Existing", again, err)
	}
	if n := len(h.workspaces(t)); n != 1 {
		t.Errorf("workspaces = %d, want 1", n)
	}

	other := &registry.Target{ID: uuid.NewString(), Name: "jet02", Kind: registry.TargetKindLocal}
	if err := h.store.CreateTarget(ctx, other); err != nil {
		t.Fatal(err)
	}
	elsewhere := apiSpec(h, "api-ws")
	elsewhere.TargetID = other.ID
	var taken *router.NameTakenError
	if _, err := h.r.ProvisionWorkspace(ctx, elsewhere); !errors.As(err, &taken) {
		t.Errorf("the name on another target: %v, want NameTakenError", err)
	}
	clone := apiSpec(h, "api-ws")
	clone.Kind, clone.GitRemote = router.ProvisionGitClone, "https://github.com/acme/x.git"
	if _, err := h.r.ProvisionWorkspace(ctx, clone); !errors.As(err, &taken) {
		t.Errorf("the name from another source: %v, want NameTakenError", err)
	}

	cur, _ := h.store.GetWorkspace(ctx, first.Workspace.ID)
	cur.Status, cur.StatusReason = registry.WorkspaceStatusFailed, "provisioning failed at run: exit 1"
	if err := h.store.UpdateWorkspace(ctx, cur); err != nil {
		t.Fatal(err)
	}
	_, err = h.r.ProvisionWorkspace(ctx, apiSpec(h, "api-ws"))
	if !errors.As(err, &taken) || !strings.Contains(err.Error(), "delete it") {
		t.Errorf("over a failed workspace: %v, want NameTakenError saying to delete it", err)
	}
}

func TestProvisionWorkspaceAPI_Refuses(t *testing.T) {
	h := newAvailabilityHarness(t)
	ctx := context.Background()
	unknown := apiSpec(h, "api-ws")
	unknown.TargetID = uuid.NewString()
	if _, err := h.r.ProvisionWorkspace(ctx, unknown); !errors.Is(err, router.ErrNoSuchTarget) {
		t.Errorf("an unknown target: %v", err)
	}
	var specErr *router.SpecError
	for name, spec := range map[string]router.ProvisionSpec{
		"bad name":          {Name: "Bad Name", TargetID: h.target.ID, Kind: router.ProvisionEmpty},
		"clone, no remote":  {Name: "ok", TargetID: h.target.ID, Kind: router.ProvisionGitClone},
		"remote not a URL":  {Name: "ok", TargetID: h.target.ID, Kind: router.ProvisionGitClone, GitRemote: "ext::sh -c id"},
		"unknown kind":      {Name: "ok", TargetID: h.target.ID, Kind: "magic"},
		"no target":         {Name: "ok", Kind: router.ProvisionEmpty},
		"remote, not clone": {Name: "ok", TargetID: h.target.ID, Kind: router.ProvisionEmpty, GitRemote: "https://github.com/acme/x.git"},
	} {
		if _, err := h.r.ProvisionWorkspace(ctx, spec); !errors.As(err, &specErr) {
			t.Errorf("%s: %v, want SpecError", name, err)
		}
	}
	if n := len(h.workspaces(t)); n != 0 {
		t.Errorf("refused requests left %d workspaces", n)
	}
}

// A recipe that fails leaves the workspace failed, with why.
func TestProvisionWorkspaceAPI_FailureRecorded(t *testing.T) {
	h := newAvailabilityHarness(t)
	h.exec.paneExit = func(command string) *targets.PaneExit {
		if isProvisioning(command) {
			return &targets.PaneExit{Status: 1, Output: "mkdir: cannot create directory: Permission denied"}
		}
		return nil
	}
	p, err := h.r.ProvisionWorkspace(context.Background(), apiSpec(h, "api-ws"))
	if err != nil {
		t.Fatal(err)
	}
	p.Run(context.Background())
	ws := h.workspaces(t)[0]
	if ws.Status != registry.WorkspaceStatusFailed || !strings.Contains(ws.StatusReason, "provisioning ended with exit 1") {
		t.Errorf("workspace = %s (%q), want failed with the exit", ws.Status, ws.StatusReason)
	}
}

// A stopped machine is started before the recipe, as a chat's
// provisioning does; one that can't be fails the workspace, with why.
func TestProvisionWorkspaceAPI_StartsAStoppedMachine(t *testing.T) {
	var mu sync.Mutex
	var started []string
	var startErr error
	info := func(ctx context.Context, targetID string) (string, bool, string, bool) {
		return "kubernetes", false, "stopped", true
	}
	starter := func(ctx context.Context, targetID string) error {
		mu.Lock()
		defer mu.Unlock()
		started = append(started, targetID)
		return startErr
	}
	h := newAvailabilityHarnessOpts(t, nil, router.WithMachines(info, starter))
	ctx := context.Background()

	p, err := h.r.ProvisionWorkspace(ctx, apiSpec(h, "api-ws"))
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if len(started) != 0 {
		t.Error("the machine was started before the request returned")
	}
	mu.Unlock()
	p.Run(ctx)
	mu.Lock()
	if len(started) != 1 || started[0] != h.target.ID {
		t.Errorf("machines started = %v, want the target", started)
	}
	startErr = errors.New("providers: the machine didn't become ready within 5m0s")
	mu.Unlock()
	if ws, _ := h.store.GetWorkspace(ctx, p.Workspace.ID); ws.Status != registry.WorkspaceStatusIdle {
		t.Fatalf("after a start that worked: %s (%s)", ws.Status, ws.StatusReason)
	}

	p, err = h.r.ProvisionWorkspace(ctx, apiSpec(h, "api-ws-2"))
	if err != nil {
		t.Fatal(err)
	}
	p.Run(ctx)
	ws, _ := h.store.GetWorkspace(ctx, p.Workspace.ID)
	if ws.Status != registry.WorkspaceStatusFailed || !strings.Contains(ws.StatusReason, "target health") || !strings.Contains(ws.StatusReason, "didn't become ready") {
		t.Errorf("after a start that failed: %s (%q), want failed naming why", ws.Status, ws.StatusReason)
	}
}
