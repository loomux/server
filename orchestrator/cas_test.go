package orchestrator_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/Loomux/server/orchestrator"
	"github.com/Loomux/server/registry"
)

// LOOM-262: every status write that follows a wait or an external call is
// a compare-and-set against the status read before it. A status change
// made meanwhile — here written straight to the store, as a takeover
// (S3) or a cancel will — must survive the write, and the task is left as
// it is, not failed.

// hookDetector is a CompletionDetector whose Wait runs during, then
// returns err: the test's "meanwhile".
type hookDetector struct {
	during func()
	err    error
}

func (d *hookDetector) Wait(ctx context.Context, task *registry.Task) error {
	if d.during != nil {
		d.during()
	}
	return d.err
}

// syncBuffer is a bytes.Buffer safe to log into from any goroutine.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type casFixture struct {
	store    registry.Store
	ws       *registry.Workspace
	exec     *fakeExecutor
	detector *hookDetector
	logs     *syncBuffer
	o        *orchestrator.Orchestrator
}

func setupCAS(t *testing.T) *casFixture {
	t.Helper()
	f := &casFixture{store: newTestStore(t), exec: newFakeExecutor(), detector: &hookDetector{}, logs: &syncBuffer{}}
	f.ws = createFixtureWorkspace(t, f.store)
	f.o = orchestrator.New(f.store, f.exec.factory(), f.detector,
		orchestrator.WithLogger(slog.New(slog.NewTextHandler(f.logs, nil))))
	return f
}

// launch starts a task and puts it in status, as the store would have
// it when the path under test reads it.
func (f *casFixture) launch(t *testing.T, kind registry.TaskKind, status registry.TaskStatus) *registry.Task {
	t.Helper()
	agentType, command := "", ""
	switch kind {
	case registry.TaskKindAgent:
		agentType, command = "claude-code", "claude"
	case registry.TaskKindCommand:
		command = "true"
	}
	task, err := f.o.Launch(context.Background(), f.ws.ID, "conv-cas", kind, agentType, command)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if status != registry.TaskStatusRunning {
		f.setStatus(t, task.ID, status)
	}
	return task
}

// setStatus is the change made meanwhile: written straight to the store.
func (f *casFixture) setStatus(t *testing.T, taskID string, status registry.TaskStatus) {
	t.Helper()
	task, err := f.store.GetTask(context.Background(), taskID)
	if err != nil {
		t.Errorf("GetTask: %v", err)
		return
	}
	task.Status = status
	if err := f.store.UpdateTask(context.Background(), task); err != nil {
		t.Errorf("UpdateTask: %v", err)
	}
}

func (f *casFixture) task(t *testing.T, taskID string) *registry.Task {
	t.Helper()
	task, err := f.store.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	return task
}

// requireLeft checks a lost compare-and-set: ErrStatusChanged, the task
// still in the status set meanwhile with nothing of the lost write on it,
// its workspace untouched, and the loss logged.
func (f *casFixture) requireLeft(t *testing.T, err error, taskID string, want registry.TaskStatus, wsStatus registry.WorkspaceStatus) {
	t.Helper()
	if !errors.Is(err, orchestrator.ErrStatusChanged) {
		t.Fatalf("err = %v, want ErrStatusChanged", err)
	}
	got := f.task(t, taskID)
	if got.Status != want {
		t.Fatalf("status = %s, want %s kept: the change made meanwhile was overwritten", got.Status, want)
	}
	if got.CompletedAt != nil || got.FailureReason != "" || got.ErrorClass != "" || got.ExitCode != nil || got.Attention != nil {
		t.Fatalf("the lost write left fields behind: %+v", got)
	}
	ws, gerr := f.store.GetWorkspace(context.Background(), f.ws.ID)
	if gerr != nil {
		t.Fatalf("GetWorkspace: %v", gerr)
	}
	if ws.Status != wsStatus {
		t.Fatalf("workspace status = %s, want %s untouched", ws.Status, wsStatus)
	}
	if logs := f.logs.String(); !strings.Contains(logs, taskID) || !strings.Contains(logs, "status changed") {
		t.Fatalf("the lost compare-and-set wasn't logged with the task; logs:\n%s", logs)
	}
}

func TestWaitForCompletion_KeepsStatusChangedDuringTheWait(t *testing.T) {
	f := setupCAS(t)
	task := f.launch(t, registry.TaskKindAgent, registry.TaskStatusRunning)
	f.detector.during = func() { f.setStatus(t, task.ID, registry.TaskStatusHumanTakeover) }

	err := f.o.WaitForCompletion(context.Background(), task.ID)
	f.requireLeft(t, err, task.ID, registry.TaskStatusHumanTakeover, registry.WorkspaceStatusActive)
}

func TestWaitForCompletion_KeepsCancelDuringTheWait(t *testing.T) {
	f := setupCAS(t)
	task := f.launch(t, registry.TaskKindAgent, registry.TaskStatusRunning)
	f.detector.during = func() { f.setStatus(t, task.ID, registry.TaskStatusFailed) }

	err := f.o.WaitForCompletion(context.Background(), task.ID)
	f.requireLeft(t, err, task.ID, registry.TaskStatusFailed, registry.WorkspaceStatusActive)
}

// A turn waits from running: a task read in any other status was changed
// after its turn was sent (between SendMessage's write and the wait's
// read), so the wait doesn't start, and its end can't overwrite it.
func TestWaitForCompletion_KeepsStatusChangedBeforeTheWait(t *testing.T) {
	f := setupCAS(t)
	task := f.launch(t, registry.TaskKindAgent, registry.TaskStatusHumanTakeover)
	waited := false
	f.detector.during = func() { waited = true }

	err := f.o.WaitForCompletion(context.Background(), task.ID)
	f.requireLeft(t, err, task.ID, registry.TaskStatusHumanTakeover, registry.WorkspaceStatusActive)
	if waited {
		t.Fatal("WaitForCompletion waited on a task that isn't running")
	}
}

// The attention path: the wait ended at a prompt, read while running.
func TestNeedAttention_KeepsStatusChangedDuringTheWait(t *testing.T) {
	f := setupCAS(t)
	task := f.launch(t, registry.TaskKindAgent, registry.TaskStatusRunning)
	f.setStatus(t, task.ID, registry.TaskStatusHumanTakeover)

	err := f.o.NeedAttention(context.Background(), task.ID, registry.TaskStatusRunning,
		&registry.Attention{Kind: registry.AttentionPermission, Title: "Run ls?"})
	f.requireLeft(t, err, task.ID, registry.TaskStatusHumanTakeover, registry.WorkspaceStatusActive)
}

// The attention path's answer: read needs-attention, keys typed, then
// Resume.
func TestResume_KeepsStatusChangedWhileAnswering(t *testing.T) {
	f := setupCAS(t)
	task := f.launch(t, registry.TaskKindAgent, registry.TaskStatusNeedsAttention)
	f.setStatus(t, task.ID, registry.TaskStatusHumanTakeover)

	err := f.o.Resume(context.Background(), task.ID)
	f.requireLeft(t, err, task.ID, registry.TaskStatusHumanTakeover, registry.WorkspaceStatusActive)
}

// failTask through Fail: a turn's failure (timeout, exit, prompt) is
// decided on the status read before the wait.
func TestFail_KeepsStatusChangedDuringTheWait(t *testing.T) {
	f := setupCAS(t)
	task := f.launch(t, registry.TaskKindAgent, registry.TaskStatusRunning)
	f.setStatus(t, task.ID, registry.TaskStatusHumanTakeover)

	err := f.o.Fail(context.Background(), task.ID, registry.TaskStatusRunning,
		registry.TaskFailure{Class: registry.ErrorClassTimeout, Reason: "timed out"})
	f.requireLeft(t, err, task.ID, registry.TaskStatusHumanTakeover, registry.WorkspaceStatusActive)
}

// failTask inside SendMessage: the paste failed, but the status changed
// while it ran.
func TestSendMessage_FailedPasteKeepsStatusChangedDuringIt(t *testing.T) {
	f := setupCAS(t)
	task := f.launch(t, registry.TaskKindAgent, registry.TaskStatusAwaitingInput)
	f.exec.onPaste = func() error {
		f.setStatus(t, task.ID, registry.TaskStatusHumanTakeover)
		return errors.New("tmux: paste failed")
	}

	if err := f.o.SendMessage(context.Background(), task.ID, "hello"); err == nil {
		t.Fatal("SendMessage succeeded with a failed paste")
	}
	got := f.task(t, task.ID)
	if got.Status != registry.TaskStatusHumanTakeover || got.FailureReason != "" {
		t.Fatalf("task = %s %q, want human_takeover kept, not failed", got.Status, got.FailureReason)
	}
	if !strings.Contains(f.logs.String(), "status changed") {
		t.Fatalf("the lost compare-and-set wasn't logged; logs:\n%s", f.logs.String())
	}
}

func TestSendMessage_KeepsStatusChangedDuringThePaste(t *testing.T) {
	f := setupCAS(t)
	task := f.launch(t, registry.TaskKindAgent, registry.TaskStatusAwaitingInput)
	f.exec.onPaste = func() error {
		f.setStatus(t, task.ID, registry.TaskStatusHumanTakeover)
		return nil
	}

	err := f.o.SendMessage(context.Background(), task.ID, "hello")
	f.requireLeft(t, err, task.ID, registry.TaskStatusHumanTakeover, registry.WorkspaceStatusActive)
}

// Complete follows the turn's relay, an external call: it was decided on
// the awaiting-input the wait left.
func TestComplete_KeepsStatusChangedDuringTheRelay(t *testing.T) {
	f := setupCAS(t)
	ctx := context.Background()
	task := f.launch(t, registry.TaskKindAgent, registry.TaskStatusAwaitingInput)
	if err := f.store.SetWorkspaceRollingSummary(ctx, f.ws.ID, "before"); err != nil {
		t.Fatalf("SetWorkspaceRollingSummary: %v", err)
	}
	f.setStatus(t, task.ID, registry.TaskStatusHumanTakeover)

	err := f.o.Complete(ctx, task.ID, registry.TaskStatusAwaitingInput, "after")
	f.requireLeft(t, err, task.ID, registry.TaskStatusHumanTakeover, registry.WorkspaceStatusActive)
	if ws, _ := f.store.GetWorkspace(ctx, f.ws.ID); ws.RollingSummary != "before" {
		t.Fatalf("rolling summary = %q, want %q: a lost Complete writes nothing", ws.RollingSummary, "before")
	}
}

// FinishCommand follows the command's wait and an exit-status read: a
// lost compare-and-set mustn't kill the pane someone else now has.
func TestFinishCommand_KeepsStatusChangedAndThePane(t *testing.T) {
	f := setupCAS(t)
	ctx := context.Background()
	task := f.launch(t, registry.TaskKindCommand, registry.TaskStatusAwaitingInput)
	f.setStatus(t, task.ID, registry.TaskStatusHumanTakeover)

	err := f.o.FinishCommand(ctx, task.ID, registry.TaskStatusAwaitingInput, 0)
	f.requireLeft(t, err, task.ID, registry.TaskStatusHumanTakeover, registry.WorkspaceStatusActive)
	if live, _ := f.exec.HasSession(ctx, task.TmuxSession); !live {
		t.Fatal("FinishCommand killed the session after losing its compare-and-set")
	}
}
