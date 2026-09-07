package orchestrator_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/Loomux/server/orchestrator"
	"github.com/Loomux/server/orchestrator/detectortest"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/registry/sqlite"
	"github.com/Loomux/server/targets"
)

func newTestStore(t *testing.T) registry.Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	store, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return store
}

func createFixtureWorkspace(t *testing.T, store registry.Store) *registry.Workspace {
	t.Helper()
	ctx := context.Background()

	target := &registry.Target{
		ID:   uuid.NewString(),
		Name: "fixture-target-" + t.Name(),
		Kind: registry.TargetKindLocal,
	}
	if err := store.CreateTarget(ctx, target); err != nil {
		t.Fatalf("fixture CreateTarget: %v", err)
	}

	ws := &registry.Workspace{
		ID:       uuid.NewString(),
		Name:     "fixture-ws-" + t.Name(),
		Path:     "/fixture/path",
		TargetID: target.ID,
		Status:   registry.WorkspaceStatusIdle,
	}
	if err := store.CreateWorkspace(ctx, ws); err != nil {
		t.Fatalf("fixture CreateWorkspace: %v", err)
	}
	return ws
}

// setup bundles the common test dependencies: a real (temp-file) store,
// a fixture workspace, a fake executor, a manual completion detector,
// and an Orchestrator wired to all three.
func setup(t *testing.T) (registry.Store, *registry.Workspace, *fakeExecutor, *detectortest.ManualDetector, *orchestrator.Orchestrator) {
	t.Helper()
	store := newTestStore(t)
	ws := createFixtureWorkspace(t, store)
	exec := newFakeExecutor()
	detector := detectortest.NewManualDetector()
	o := orchestrator.New(store, exec.factory(), detector)
	return store, ws, exec, detector, o
}

func TestLaunch(t *testing.T) {
	store, ws, exec, _, o := setup(t)
	ctx := context.Background()

	task, err := o.Launch(ctx, ws.ID, "conv-1", registry.TaskKindAgent, "claude-code", "claude")
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if task.WorkspaceID != ws.ID {
		t.Fatalf("WorkspaceID = %q, want %q", task.WorkspaceID, ws.ID)
	}
	if task.Kind != registry.TaskKindAgent {
		t.Fatalf("Kind = %q, want %q", task.Kind, registry.TaskKindAgent)
	}
	if task.AgentType != "claude-code" {
		t.Fatalf("AgentType = %q, want %q", task.AgentType, "claude-code")
	}
	if task.ConversationID != "conv-1" {
		t.Fatalf("ConversationID = %q, want %q", task.ConversationID, "conv-1")
	}
	if task.Status != registry.TaskStatusRunning {
		t.Fatalf("Status = %q, want %q", task.Status, registry.TaskStatusRunning)
	}
	if task.StartedAt == nil {
		t.Fatalf("StartedAt is nil")
	}
	if task.TmuxSession == "" {
		t.Fatalf("TmuxSession is empty")
	}

	sess := exec.sessionFor(task.TmuxSession)
	if sess == nil {
		t.Fatalf("no session created on executor for %q", task.TmuxSession)
	}
	if sess.dir != ws.Path {
		t.Fatalf("session dir = %q, want workspace path %q", sess.dir, ws.Path)
	}
	if sess.command != "claude" {
		t.Fatalf("session command = %q, want %q", sess.command, "claude")
	}

	stored, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if stored.Status != registry.TaskStatusRunning {
		t.Fatalf("stored Status = %q, want %q", stored.Status, registry.TaskStatusRunning)
	}

	updatedWS, err := store.GetWorkspace(ctx, ws.ID)
	if err != nil {
		t.Fatalf("GetWorkspace: %v", err)
	}
	if updatedWS.Status != registry.WorkspaceStatusActive {
		t.Fatalf("workspace Status = %q, want %q", updatedWS.Status, registry.WorkspaceStatusActive)
	}
	if updatedWS.LastUsedAt == nil {
		t.Fatalf("workspace LastUsedAt is nil")
	}
}

// TestLaunchWithID_UsesSuppliedTaskID proves LaunchWithID (LOOM-32) uses
// the caller-supplied ID as task.ID instead of minting its own — the
// mechanism router.launchAgent needs to know a task's ID before its
// launch command (which embeds that ID for a TierMarker hook script to
// read) is even built.
func TestLaunchWithID_UsesSuppliedTaskID(t *testing.T) {
	store, ws, exec, _, o := setup(t)
	ctx := context.Background()

	task, err := o.LaunchWithID(ctx, ws.ID, "conv-1", registry.TaskKindAgent, "claude-code", "supplied-task-id", "claude")
	if err != nil {
		t.Fatalf("LaunchWithID: %v", err)
	}
	if task.ID != "supplied-task-id" {
		t.Fatalf("task.ID = %q, want the supplied ID %q", task.ID, "supplied-task-id")
	}

	stored, err := store.GetTask(ctx, "supplied-task-id")
	if err != nil {
		t.Fatalf("GetTask(supplied-task-id): %v", err)
	}
	if stored.WorkspaceID != ws.ID {
		t.Fatalf("stored WorkspaceID = %q, want %q", stored.WorkspaceID, ws.ID)
	}

	sess := exec.sessionFor(task.TmuxSession)
	if sess == nil {
		t.Fatalf("no session created on executor for %q", task.TmuxSession)
	}
	if sess.command != "claude" {
		t.Fatalf("session command = %q, want %q", sess.command, "claude")
	}
}

func TestLaunchShell(t *testing.T) {
	_, ws, exec, _, o := setup(t)
	ctx := context.Background()

	task, err := o.Launch(ctx, ws.ID, "conv-1", registry.TaskKindShell, "", "")
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if task.Kind != registry.TaskKindShell {
		t.Fatalf("Kind = %q, want %q", task.Kind, registry.TaskKindShell)
	}
	if task.AgentType != "" {
		t.Fatalf("AgentType = %q, want empty for a shell task", task.AgentType)
	}
	sess := exec.sessionFor(task.TmuxSession)
	if sess == nil {
		t.Fatalf("no session created for shell task")
	}
	if sess.command != "" {
		t.Fatalf("session command = %q, want empty (default shell)", sess.command)
	}
}

func TestLaunchAutoFailsOnUnreachable(t *testing.T) {
	store, ws, exec, _, o := setup(t)
	exec.unreachable = true
	ctx := context.Background()

	task, err := o.Launch(ctx, ws.ID, "conv-1", registry.TaskKindShell, "", "")
	if !errors.Is(err, targets.ErrUnreachable) {
		t.Fatalf("Launch: err = %v, want wrapping ErrUnreachable", err)
	}
	if task == nil {
		t.Fatalf("Launch returned a nil task alongside the error; want the (now-Failed) task row")
	}

	stored, getErr := store.GetTask(ctx, task.ID)
	if getErr != nil {
		t.Fatalf("GetTask: %v", getErr)
	}
	if stored.Status != registry.TaskStatusFailed {
		t.Fatalf("task Status = %q, want %q", stored.Status, registry.TaskStatusFailed)
	}
	if stored.CompletedAt == nil {
		t.Fatalf("task CompletedAt is nil after auto-fail")
	}

	updatedWS, wsErr := store.GetWorkspace(ctx, ws.ID)
	if wsErr != nil {
		t.Fatalf("GetWorkspace: %v", wsErr)
	}
	if updatedWS.Status != registry.WorkspaceStatusIdle {
		t.Fatalf("workspace Status = %q, want %q", updatedWS.Status, registry.WorkspaceStatusIdle)
	}
}

func TestSendMessage(t *testing.T) {
	store, ws, exec, _, o := setup(t)
	ctx := context.Background()

	task, err := o.Launch(ctx, ws.ID, "conv-1", registry.TaskKindShell, "", "")
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if err := o.SendMessage(ctx, task.ID, "echo hi"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	sess := exec.sessionFor(task.TmuxSession)
	if len(sess.keys) != 1 || sess.keys[0] != "echo hi" {
		t.Fatalf("session.keys = %v, want [%q]", sess.keys, "echo hi")
	}

	stored, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if stored.Status != registry.TaskStatusRunning {
		t.Fatalf("Status = %q, want %q", stored.Status, registry.TaskStatusRunning)
	}
}

func TestSendMessage_RefusesDuringTakeover(t *testing.T) {
	_, ws, exec, _, o := setup(t)
	ctx := context.Background()

	task, err := o.Launch(ctx, ws.ID, "conv-1", registry.TaskKindShell, "", "")
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if err := o.Takeover(ctx, task.ID); err != nil {
		t.Fatalf("Takeover: %v", err)
	}

	if err := o.SendMessage(ctx, task.ID, "echo hi"); !errors.Is(err, orchestrator.ErrHumanTakeover) {
		t.Fatalf("SendMessage during takeover: err = %v, want ErrHumanTakeover", err)
	}

	sess := exec.sessionFor(task.TmuxSession)
	if len(sess.keys) != 0 {
		t.Fatalf("keys were sent despite takeover: %v", sess.keys)
	}
}

func TestSendMessage_RefusesWhenInactive(t *testing.T) {
	_, ws, _, _, o := setup(t)
	ctx := context.Background()

	task, err := o.Launch(ctx, ws.ID, "conv-1", registry.TaskKindShell, "", "")
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if err := o.Complete(ctx, task.ID, "done"); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if err := o.SendMessage(ctx, task.ID, "echo hi"); !errors.Is(err, orchestrator.ErrTaskInactive) {
		t.Fatalf("SendMessage after completion: err = %v, want ErrTaskInactive", err)
	}
}

func TestWaitForCompletion(t *testing.T) {
	store, ws, _, detector, o := setup(t)
	ctx := context.Background()

	task, err := o.Launch(ctx, ws.ID, "conv-1", registry.TaskKindShell, "", "")
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	detector.Signal(task.ID)
	if err := o.WaitForCompletion(ctx, task.ID); err != nil {
		t.Fatalf("WaitForCompletion: %v", err)
	}

	stored, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if stored.Status != registry.TaskStatusAwaitingInput {
		t.Fatalf("Status = %q, want %q", stored.Status, registry.TaskStatusAwaitingInput)
	}
}

func TestWaitForCompletion_ContextCancelled(t *testing.T) {
	store, ws, _, _, o := setup(t)
	ctx := context.Background()

	task, err := o.Launch(ctx, ws.ID, "conv-1", registry.TaskKindShell, "", "")
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := o.WaitForCompletion(cancelledCtx, task.ID); err == nil {
		t.Fatalf("WaitForCompletion with a cancelled context: got nil error")
	}

	stored, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if stored.Status != registry.TaskStatusRunning {
		t.Fatalf("Status = %q after cancelled wait, want unchanged %q", stored.Status, registry.TaskStatusRunning)
	}
}

func TestComplete(t *testing.T) {
	store, ws, exec, _, o := setup(t)
	ctx := context.Background()

	task, err := o.Launch(ctx, ws.ID, "conv-1", registry.TaskKindShell, "", "")
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if err := o.Complete(ctx, task.ID, "task summary text"); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	stored, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if stored.Status != registry.TaskStatusCompleted {
		t.Fatalf("Status = %q, want %q", stored.Status, registry.TaskStatusCompleted)
	}
	if stored.CompletedAt == nil {
		t.Fatalf("CompletedAt is nil")
	}

	exists, err := exec.HasSession(ctx, task.TmuxSession)
	if err != nil {
		t.Fatalf("HasSession: %v", err)
	}
	if exists {
		t.Fatalf("session still exists after Complete")
	}

	updatedWS, err := store.GetWorkspace(ctx, ws.ID)
	if err != nil {
		t.Fatalf("GetWorkspace: %v", err)
	}
	if updatedWS.RollingSummary != "task summary text" {
		t.Fatalf("RollingSummary = %q, want %q", updatedWS.RollingSummary, "task summary text")
	}
	if updatedWS.Status != registry.WorkspaceStatusIdle {
		t.Fatalf("workspace Status = %q, want %q", updatedWS.Status, registry.WorkspaceStatusIdle)
	}
}

func TestComplete_ReplacesRollingSummaryNotAppends(t *testing.T) {
	store, ws, _, _, o := setup(t)
	ctx := context.Background()

	task1, err := o.Launch(ctx, ws.ID, "conv-1", registry.TaskKindShell, "", "")
	if err != nil {
		t.Fatalf("Launch(task1): %v", err)
	}
	if err := o.Complete(ctx, task1.ID, "first summary"); err != nil {
		t.Fatalf("Complete(task1): %v", err)
	}

	task2, err := o.Launch(ctx, ws.ID, "conv-2", registry.TaskKindShell, "", "")
	if err != nil {
		t.Fatalf("Launch(task2): %v", err)
	}
	if err := o.Complete(ctx, task2.ID, "second summary"); err != nil {
		t.Fatalf("Complete(task2): %v", err)
	}

	updatedWS, err := store.GetWorkspace(ctx, ws.ID)
	if err != nil {
		t.Fatalf("GetWorkspace: %v", err)
	}
	if updatedWS.RollingSummary != "second summary" {
		t.Fatalf("RollingSummary = %q, want exactly %q (replace, not append)", updatedWS.RollingSummary, "second summary")
	}
}

func TestFail(t *testing.T) {
	store, ws, exec, _, o := setup(t)
	ctx := context.Background()

	task, err := o.Launch(ctx, ws.ID, "conv-1", registry.TaskKindShell, "", "")
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if err := o.Fail(ctx, task.ID, "agent crashed"); err != nil {
		t.Fatalf("Fail: %v", err)
	}

	stored, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if stored.Status != registry.TaskStatusFailed {
		t.Fatalf("Status = %q, want %q", stored.Status, registry.TaskStatusFailed)
	}
	if stored.CompletedAt == nil {
		t.Fatalf("CompletedAt is nil")
	}

	exists, err := exec.HasSession(ctx, task.TmuxSession)
	if err != nil {
		t.Fatalf("HasSession: %v", err)
	}
	if !exists {
		t.Fatalf("session was torn down on Fail; spec requires leaving it alive for inspection")
	}

	updatedWS, err := store.GetWorkspace(ctx, ws.ID)
	if err != nil {
		t.Fatalf("GetWorkspace: %v", err)
	}
	if updatedWS.Status != registry.WorkspaceStatusIdle {
		t.Fatalf("workspace Status = %q, want %q", updatedWS.Status, registry.WorkspaceStatusIdle)
	}
}

func TestTakeoverRelease(t *testing.T) {
	store, ws, exec, _, o := setup(t)
	ctx := context.Background()

	task, err := o.Launch(ctx, ws.ID, "conv-1", registry.TaskKindShell, "", "")
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if err := o.Takeover(ctx, task.ID); err != nil {
		t.Fatalf("Takeover: %v", err)
	}
	stored, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if stored.Status != registry.TaskStatusHumanTakeover {
		t.Fatalf("Status after Takeover = %q, want %q", stored.Status, registry.TaskStatusHumanTakeover)
	}

	if err := o.Release(ctx, task.ID); err != nil {
		t.Fatalf("Release: %v", err)
	}
	stored, err = store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if stored.Status != registry.TaskStatusAwaitingInput {
		t.Fatalf("Status after Release = %q, want %q", stored.Status, registry.TaskStatusAwaitingInput)
	}

	// Automated dispatch works again after release.
	if err := o.SendMessage(ctx, task.ID, "echo released"); err != nil {
		t.Fatalf("SendMessage after Release: %v", err)
	}
	sess := exec.sessionFor(task.TmuxSession)
	if len(sess.keys) != 1 || sess.keys[0] != "echo released" {
		t.Fatalf("session.keys = %v, want [%q]", sess.keys, "echo released")
	}
}

func TestTakeover_RefusesWhenInactive(t *testing.T) {
	_, ws, _, _, o := setup(t)
	ctx := context.Background()

	task, err := o.Launch(ctx, ws.ID, "conv-1", registry.TaskKindShell, "", "")
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if err := o.Complete(ctx, task.ID, "done"); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if err := o.Takeover(ctx, task.ID); !errors.Is(err, orchestrator.ErrTaskInactive) {
		t.Fatalf("Takeover on a completed task: err = %v, want ErrTaskInactive", err)
	}
}
