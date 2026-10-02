package router_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/router"
	"github.com/Loomux/server/router/routertest"
	"github.com/Loomux/server/targets"
)

// onlyTask returns the single task in workspaceID.
func onlyTask(t *testing.T, store registry.Store, workspaceID string) *registry.Task {
	t.Helper()
	tasks, err := store.ListTasksByWorkspace(context.Background(), workspaceID)
	if err != nil {
		t.Fatalf("ListTasksByWorkspace: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("tasks = %+v, want exactly one", tasks)
	}
	return tasks[0]
}

func assertFailed(t *testing.T, task *registry.Task, class registry.ErrorClass, reasonContains string) {
	t.Helper()
	if task.Status != registry.TaskStatusFailed {
		t.Errorf("task status = %q, want failed", task.Status)
	}
	if task.ErrorClass != class {
		t.Errorf("error class = %q, want %q", task.ErrorClass, class)
	}
	if !strings.Contains(task.FailureReason, reasonContains) {
		t.Errorf("failure reason = %q, want it to mention %q", task.FailureReason, reasonContains)
	}
}

// LOOM-77: every dispatch error after the task row exists leaves the task
// failed with a reason and class — never running/awaiting-input with
// nothing recorded, as a relay failure, a broken capture or an abandoned
// wait used to.
func TestDispatch_ErrorAfterLaunch_FailsTaskWithReason(t *testing.T) {
	for _, tc := range []struct {
		name    string
		breakIt func(*fakeExecutor, *routerHarness)
		class   registry.ErrorClass
		reason  string
	}{
		{name: "relay", class: registry.ErrorClassRelayFailed, reason: "relay model down",
			breakIt: func(_ *fakeExecutor, h *routerHarness) {
				h.model.RelayFunc = func(context.Context, string) (router.RelayResult, error) {
					return router.RelayResult{}, errors.New("relay model down")
				}
			}},
		{name: "wait", class: registry.ErrorClassWaitFailed, reason: "capture broke",
			breakIt: func(e *fakeExecutor, _ *routerHarness) { e.captureErr = errors.New("capture broke") }},
		{name: "wait unreachable", class: registry.ErrorClassTargetUnreachable, reason: "unreachable",
			breakIt: func(e *fakeExecutor, _ *routerHarness) { e.captureErr = targets.ErrUnreachable }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newRouterHarness(t)
			tc.breakIt(h.exec, h)
			if _, err := h.r.Dispatch(context.Background(), "conv-1", "go"); err == nil {
				t.Fatal("Dispatch: got nil error")
			}
			assertFailed(t, onlyTask(t, h.store, h.ws.ID), tc.class, tc.reason)
		})
	}
}

func TestDispatch_CancelledWait_FailsTaskWithReason(t *testing.T) {
	h := newRouterHarness(t)
	// A pane that never goes quiet keeps the idle detector waiting until
	// the request is cancelled.
	h.exec.captureFunc = func() string { return uuid.NewString() }
	ctx, cancel := context.WithCancel(context.Background())
	// Cancel once the turn is under way, i.e. during the wait.
	h.exec.onSendKeys = func() { time.AfterFunc(100*time.Millisecond, cancel) }

	if _, err := h.r.Dispatch(ctx, "conv-1", "go"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Dispatch err = %v, want context.Canceled", err)
	}
	assertFailed(t, onlyTask(t, h.store, h.ws.ID), registry.ErrorClassWaitFailed, "context canceled")
}

// An agent that exits mid-turn records agent_exited with its (redacted)
// last output on the task itself, not only in the returned error.
func TestDispatch_AgentExited_RecordsOutputTail(t *testing.T) {
	h := newRouterHarness(t)
	if err := h.store.CreateCredential(context.Background(), &registry.Credential{
		ID: uuid.NewString(), Name: "ANTHROPIC_API_KEY", AgentType: "claude-code", Value: "sk-ant-very-secret",
	}); err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}
	h.exec.paneExit = func(command string) *targets.PaneExit {
		if strings.HasSuffix(command, "claude") {
			return &targets.PaneExit{Status: 1, Output: "auth failed for sk-ant-very-secret"}
		}
		return nil
	}
	if _, err := h.r.Dispatch(context.Background(), "conv-1", "go"); err == nil {
		t.Fatal("Dispatch: got nil error")
	}
	task := onlyTask(t, h.store, h.ws.ID)
	assertFailed(t, task, registry.ErrorClassAgentExited, "status 1")
	if !strings.Contains(task.OutputTail, "auth failed") || strings.Contains(task.OutputTail, "sk-ant-very-secret") {
		t.Errorf("output tail = %q, want the output with the credential redacted", task.OutputTail)
	}
}

// A failed provisioning records why on the workspace (status_reason) and
// on its task, and the workspace is never 'active' while provisioning.
func TestDispatch_ProvisioningFailure_RecordsReasons(t *testing.T) {
	store, exec, r, model := setup(t)
	target := createFixtureTarget(t, store)
	var statusDuringProvisioning registry.WorkspaceStatus
	exec.paneExit = func(command string) *targets.PaneExit {
		if isProvisioning(command) {
			list, _ := store.ListWorkspaces(context.Background())
			if len(list) == 1 {
				statusDuringProvisioning = list[0].Status
			}
			return &targets.PaneExit{Status: 128, Output: "fatal: repository 'nowhere' does not exist"}
		}
		return nil
	}
	model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{Action: router.ActionProvisionWorkspace, AgentType: "claude-code", NewWorkspace: router.ProvisionSpec{
			Name: "ws", TargetID: target.ID, Kind: router.ProvisionGitClone, GitRemote: "https://example.invalid/repo.git",
		}}, nil
	}
	if _, err := r.Dispatch(context.Background(), "conv-1", "clone https://example.invalid/repo.git"); err == nil {
		t.Fatal("Dispatch: got nil error")
	}
	if statusDuringProvisioning != registry.WorkspaceStatusProvisioning {
		t.Errorf("workspace status while provisioning ran = %q, want provisioning", statusDuringProvisioning)
	}
	list, _ := store.ListWorkspaces(context.Background())
	if len(list) != 1 || list[0].Status != registry.WorkspaceStatusFailed || !strings.Contains(list[0].StatusReason, "status 128") {
		t.Fatalf("workspace = %+v, want failed with a status_reason naming the exit status", list)
	}
	task := onlyTask(t, store, list[0].ID)
	assertFailed(t, task, registry.ErrorClassProvisionFailed, "status 128")
	if !strings.Contains(task.OutputTail, "does not exist") {
		t.Errorf("output tail = %q, want the script's output", task.OutputTail)
	}
}

// routerHarness is setup plus a fixture workspace the stub model always
// dispatches into (use_workspace, claude-code), relaying "ok".
type routerHarness struct {
	store registry.Store
	exec  *fakeExecutor
	r     *router.Router
	model *routertest.StubRoutingModel
	ws    *registry.Workspace
}

func newRouterHarness(t *testing.T) *routerHarness {
	t.Helper()
	store, exec, r, model := setup(t)
	ws := createFixtureWorkspace(t, store)
	model.DecideFunc = func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
		return router.Decision{Action: router.ActionUseWorkspace, WorkspaceID: ws.ID, AgentType: "claude-code"}, nil
	}
	model.RelayFunc = func(ctx context.Context, captured string) (router.RelayResult, error) {
		return router.RelayResult{Reply: "ok", Done: false}, nil
	}
	return &routerHarness{store: store, exec: exec, r: r, model: model, ws: ws}
}
