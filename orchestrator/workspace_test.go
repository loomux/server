package orchestrator_test

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Loomux/server/orchestrator/detectortest"
	"github.com/Loomux/server/registry/sqlite"

	"github.com/Loomux/server/orchestrator"
	"github.com/Loomux/server/registry"
)

func TestDeleteWorkspaceKillsItsSessionsAndRows(t *testing.T) {
	store, ws, exec, _, o := setup(t)
	ctx := context.Background()

	open, err := o.Launch(ctx, ws.ID, "conv-1", registry.TaskKindAgent, "claude-code", "claude")
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if _, err := o.DeleteWorkspace(ctx, ws.ID); !errors.Is(err, registry.ErrConflict) {
		t.Fatalf("DeleteWorkspace mid-turn: err = %v, want ErrConflict", err)
	}
	if err := o.Takeover(ctx, open.ID); err != nil {
		t.Fatalf("Takeover: %v", err)
	}
	if _, err := o.DeleteWorkspace(ctx, ws.ID); !errors.Is(err, registry.ErrConflict) {
		t.Fatalf("DeleteWorkspace during a takeover: err = %v, want ErrConflict", err)
	}
	open.Status = registry.TaskStatusAwaitingInput
	if err := store.UpdateTask(ctx, open); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	done, err := o.Launch(ctx, ws.ID, "conv-2", registry.TaskKindShell, "", "ls")
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if err := o.Complete(ctx, done.ID, "listed"); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	left, err := o.DeleteWorkspace(ctx, ws.ID)
	if err != nil {
		t.Fatalf("DeleteWorkspace: %v", err)
	}
	if len(left) != 0 {
		t.Fatalf("sessions not killed = %v, want none", left)
	}
	for _, task := range []*registry.Task{open, done} {
		if s := exec.sessionFor(task.TmuxSession); s == nil || s.alive {
			t.Fatalf("session %s still alive after delete", task.TmuxSession)
		}
		if _, err := store.GetTask(ctx, task.ID); !errors.Is(err, registry.ErrNotFound) {
			t.Fatalf("GetTask(%s) after delete: err = %v, want ErrNotFound", task.ID, err)
		}
	}
	if _, err := store.GetWorkspace(ctx, ws.ID); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("GetWorkspace after delete: err = %v, want ErrNotFound", err)
	}
}

// An unreachable target doesn't stop the delete: the rows go, and the
// sessions it couldn't kill are reported (the orphan sweep gets them
// once the target is back, no task owning them any more).
func TestDeleteWorkspaceUnreachableTargetReportsSessions(t *testing.T) {
	store, ws, exec, _, o := setup(t)
	ctx := context.Background()
	task, err := o.Launch(ctx, ws.ID, "conv-1", registry.TaskKindAgent, "claude-code", "claude")
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	task.Status = registry.TaskStatusAwaitingInput
	if err := store.UpdateTask(ctx, task); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	exec.unreachable = true

	left, err := o.DeleteWorkspace(ctx, ws.ID)
	if err != nil {
		t.Fatalf("DeleteWorkspace: %v", err)
	}
	if len(left) != 1 || left[0] != task.TmuxSession {
		t.Fatalf("sessions not killed = %v, want [%s]", left, task.TmuxSession)
	}
	if _, err := store.GetWorkspace(ctx, ws.ID); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("GetWorkspace after delete: err = %v, want ErrNotFound", err)
	}
}

func TestDeleteWorkspaceWithScopedCredentialKillsNothing(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"), sqlite.WithMasterKey(bytes.Repeat([]byte{7}, 32)))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ws := createFixtureWorkspace(t, store)
	exec := newFakeExecutor()
	o := orchestrator.New(store, exec.factory(), detectortest.NewManualDetector())
	ctx := context.Background()
	task, err := o.Launch(ctx, ws.ID, "conv-1", registry.TaskKindAgent, "claude-code", "claude")
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	task.Status = registry.TaskStatusAwaitingInput
	if err := store.UpdateTask(ctx, task); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	if err := store.CreateCredential(ctx, &registry.Credential{ID: "c", Name: "TOKEN", WorkspaceID: ws.ID, Value: "v"}); err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}
	if _, err := o.DeleteWorkspace(ctx, ws.ID); !errors.Is(err, registry.ErrConflict) {
		t.Fatalf("DeleteWorkspace: err = %v, want ErrConflict", err)
	}
	if s := exec.sessionFor(task.TmuxSession); s == nil || !s.alive {
		t.Fatalf("session killed although the delete was refused")
	}
}

func TestDeleteWorkspaceRefusesProvisioning(t *testing.T) {
	store, ws, _, _, o := setup(t)
	ctx := context.Background()
	ws.Status = registry.WorkspaceStatusProvisioning
	if err := store.UpdateWorkspace(ctx, ws); err != nil {
		t.Fatalf("UpdateWorkspace: %v", err)
	}
	if _, err := o.DeleteWorkspace(ctx, ws.ID); !errors.Is(err, registry.ErrConflict) {
		t.Fatalf("DeleteWorkspace while provisioning: err = %v, want ErrConflict", err)
	}
}

func TestDeleteWorkspaceUnknown(t *testing.T) {
	_, _, _, _, o := setup(t)
	if _, err := o.DeleteWorkspace(context.Background(), "nope"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("DeleteWorkspace: err = %v, want ErrNotFound", err)
	}
}

func TestReopenWorkspace(t *testing.T) {
	store, ws, exec, _, o := setup(t)
	ctx := context.Background()
	ws.Status = registry.WorkspaceStatusFailed
	ws.StatusReason = "clone failed"
	if err := store.UpdateWorkspace(ctx, ws); err != nil {
		t.Fatalf("UpdateWorkspace: %v", err)
	}

	exec.runOnceOut = "missing\n"
	err := o.ReopenWorkspace(ctx, ws.ID)
	var conflict *registry.ConflictError
	if !errors.As(err, &conflict) || !strings.Contains(conflict.Reason, "/fixture/path") {
		t.Fatalf("ReopenWorkspace without its directory: err = %v, want a conflict naming the path", err)
	}

	exec.runOnceOut = "present\n"
	if err := o.ReopenWorkspace(ctx, ws.ID); err != nil {
		t.Fatalf("ReopenWorkspace: %v", err)
	}
	got, err := store.GetWorkspace(ctx, ws.ID)
	if err != nil {
		t.Fatalf("GetWorkspace: %v", err)
	}
	if got.Status != registry.WorkspaceStatusIdle || got.StatusReason != "" {
		t.Fatalf("after reopen: status %q reason %q, want idle and no reason", got.Status, got.StatusReason)
	}
	if !strings.Contains(exec.lastRunOnce, "/fixture/path") {
		t.Fatalf("directory check ran %q, want it to name the workspace path", exec.lastRunOnce)
	}
}

func TestArchiveWorkspace(t *testing.T) {
	store, ws, _, _, o := setup(t)
	ctx := context.Background()
	task, err := o.Launch(ctx, ws.ID, "c", registry.TaskKindShell, "", "ls")
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if err := o.ArchiveWorkspace(ctx, ws.ID); !errors.Is(err, registry.ErrConflict) {
		t.Fatalf("ArchiveWorkspace while a task runs: err = %v, want ErrConflict", err)
	}
	if err := o.Complete(ctx, task.ID, "done"); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if err := o.ArchiveWorkspace(ctx, ws.ID); err != nil {
		t.Fatalf("ArchiveWorkspace: %v", err)
	}
	got, err := store.GetWorkspace(ctx, ws.ID)
	if err != nil || got.Status != registry.WorkspaceStatusArchived {
		t.Fatalf("after archive: %+v, %v; want archived", got, err)
	}
}

func TestReopenWorkspaceRefusesProvisioning(t *testing.T) {
	store, ws, exec, _, o := setup(t)
	ctx := context.Background()
	ws.Status = registry.WorkspaceStatusProvisioning
	if err := store.UpdateWorkspace(ctx, ws); err != nil {
		t.Fatalf("UpdateWorkspace: %v", err)
	}
	exec.runOnceOut = "present\n"
	if err := o.ReopenWorkspace(ctx, ws.ID); !errors.Is(err, registry.ErrConflict) {
		t.Fatalf("ReopenWorkspace while provisioning: err = %v, want ErrConflict", err)
	}
}
