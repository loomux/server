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
	if stored.ErrorClass != registry.ErrorClassTargetUnreachable || stored.FailureReason == "" {
		t.Fatalf("auto-fail recorded class %q reason %q, want target_unreachable with a reason", stored.ErrorClass, stored.FailureReason)
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

	// LOOM-91: the pane outlives the task for a grace period, so a
	// person can still attach and see what happened; SweepFinishedPanes
	// tears it down later.
	exists, err := exec.HasSession(ctx, task.TmuxSession)
	if err != nil {
		t.Fatalf("HasSession: %v", err)
	}
	if !exists {
		t.Fatalf("session killed by Complete, want it kept for the grace period")
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

	if err := o.Fail(ctx, task.ID, registry.TaskFailure{
		Class: registry.ErrorClassAgentExited, Reason: "agent crashed", OutputTail: "segfault",
	}); err != nil {
		t.Fatalf("Fail: %v", err)
	}

	stored, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if stored.Status != registry.TaskStatusFailed {
		t.Fatalf("Status = %q, want %q", stored.Status, registry.TaskStatusFailed)
	}
	// LOOM-77: the reason is persisted, not just accepted.
	if stored.FailureReason != "agent crashed" || stored.ErrorClass != registry.ErrorClassAgentExited || stored.OutputTail != "segfault" {
		t.Fatalf("failure = %q / %q / %q, want it persisted", stored.FailureReason, stored.ErrorClass, stored.OutputTail)
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

// A command task (LOOM-71) records exactly what it ran; an agent task
// never does — its launch command carries injected credentials.
func TestLaunch_RecordsCommandForCommandTasksOnly(t *testing.T) {
	store, ws, _, _, o := setup(t)
	ctx := context.Background()

	cmdTask, err := o.Launch(ctx, ws.ID, "conv-1", registry.TaskKindCommand, "", "npm install -g @openai/codex")
	if err != nil {
		t.Fatalf("Launch(command): %v", err)
	}
	got, err := store.GetTask(ctx, cmdTask.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Kind != registry.TaskKindCommand || got.Command != "npm install -g @openai/codex" {
		t.Errorf("command task = %+v, want kind command with its command recorded", got)
	}

	agentTask, err := o.Launch(ctx, ws.ID, "conv-2", registry.TaskKindAgent, "codex", "OPENAI_API_KEY='secret' codex")
	if err != nil {
		t.Fatalf("Launch(agent): %v", err)
	}
	got, err = store.GetTask(ctx, agentTask.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Command != "" {
		t.Errorf("agent task recorded its launch command %q; it must never be stored", got.Command)
	}
}

// FinishCommand records a command task's exit code — a non-zero one is
// still a finished command, not a Loomux failure — tears its (dead) pane
// down, and leaves the workspace's rolling summary alone: a command's
// output is relayed to chat, not folded into the workspace's summary.
func TestFinishCommand(t *testing.T) {
	store, ws, exec, _, o := setup(t)
	ctx := context.Background()
	if err := store.SetWorkspaceRollingSummary(ctx, ws.ID, "earlier summary"); err != nil {
		t.Fatalf("SetWorkspaceRollingSummary: %v", err)
	}

	task, err := o.Launch(ctx, ws.ID, "conv-1", registry.TaskKindCommand, "", "false")
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if err := o.FinishCommand(ctx, task.ID, 1); err != nil {
		t.Fatalf("FinishCommand: %v", err)
	}

	got, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != registry.TaskStatusCompleted || got.ExitCode == nil || *got.ExitCode != 1 || got.CompletedAt == nil {
		t.Errorf("task after FinishCommand = %+v (exit %v), want completed with exit code 1", got, got.ExitCode)
	}
	if exec.sessionFor(task.TmuxSession).alive {
		t.Error("FinishCommand left the pane alive")
	}
	gotWS, err := store.GetWorkspace(ctx, ws.ID)
	if err != nil {
		t.Fatalf("GetWorkspace: %v", err)
	}
	if gotWS.Status != registry.WorkspaceStatusIdle || gotWS.RollingSummary != "earlier summary" {
		t.Errorf("workspace after FinishCommand = status %q summary %q, want idle and summary untouched", gotWS.Status, gotWS.RollingSummary)
	}
}

func TestFinishCommand_RefusesNonCommandTask(t *testing.T) {
	_, ws, _, _, o := setup(t)
	ctx := context.Background()
	task, err := o.Launch(ctx, ws.ID, "conv-1", registry.TaskKindAgent, "codex", "codex")
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if err := o.FinishCommand(ctx, task.ID, 0); err == nil {
		t.Error("FinishCommand on an agent task: want error, got nil")
	}
}

// TestLaunch_LeavesProvisioningWorkspaceProvisioning (LOOM-77): the
// router owns a provisioning workspace's status (provisioning → idle on
// success, → failed otherwise); starting its provisioning session must
// not flip it to active, as it used to.
func TestLaunch_LeavesProvisioningWorkspaceProvisioning(t *testing.T) {
	store, ws, _, _, o := setup(t)
	ctx := context.Background()
	ws.Status = registry.WorkspaceStatusProvisioning
	if err := store.UpdateWorkspace(ctx, ws); err != nil {
		t.Fatalf("UpdateWorkspace: %v", err)
	}
	if _, err := o.Launch(ctx, ws.ID, "conv-1", registry.TaskKindShell, "", "git clone x"); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	got, err := store.GetWorkspace(ctx, ws.ID)
	if err != nil {
		t.Fatalf("GetWorkspace: %v", err)
	}
	if got.Status != registry.WorkspaceStatusProvisioning {
		t.Fatalf("workspace status = %q, want still provisioning", got.Status)
	}
}

// TestFail_DoesNotRevertFailedWorkspace: failing a task only returns an
// active workspace to idle; a workspace already marked failed (or still
// provisioning) keeps its status.
func TestFail_DoesNotRevertFailedWorkspace(t *testing.T) {
	store, ws, _, _, o := setup(t)
	ctx := context.Background()
	task, err := o.Launch(ctx, ws.ID, "conv-1", registry.TaskKindShell, "", "")
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	cur, _ := store.GetWorkspace(ctx, ws.ID)
	cur.Status = registry.WorkspaceStatusFailed
	if err := store.UpdateWorkspace(ctx, cur); err != nil {
		t.Fatalf("UpdateWorkspace: %v", err)
	}
	if err := o.Fail(ctx, task.ID, registry.TaskFailure{Class: registry.ErrorClassInternal, Reason: "x"}); err != nil {
		t.Fatalf("Fail: %v", err)
	}
	got, _ := store.GetWorkspace(ctx, ws.ID)
	if got.Status != registry.WorkspaceStatusFailed {
		t.Fatalf("workspace status = %q, want failed kept", got.Status)
	}
}
