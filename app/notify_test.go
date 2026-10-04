package app

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Loomux/server/notify"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/registry/sqlite"
)

func TestTurnNotifierEvent(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "n.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.CreateTarget(ctx, &registry.Target{ID: "t", Name: "jet01", Kind: registry.TargetKindLocal}); err != nil {
		t.Fatalf("CreateTarget: %v", err)
	}
	if err := store.CreateWorkspace(ctx, &registry.Workspace{ID: "w", Name: "theWyseKube", Path: "/p", TargetID: "t", Status: registry.WorkspaceStatusIdle}); err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	start := time.Now().UTC().Add(-2 * time.Minute)
	if err := store.CreateTask(ctx, &registry.Task{
		ID: "task", WorkspaceID: "w", Kind: registry.TaskKindAgent, TmuxSession: "s",
		Status: registry.TaskStatusAwaitingInput, ConversationID: "conv/1",
	}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	n := &turnNotifier{store: store, minDuration: time.Minute, publicURL: "https://loomux.example/"}
	finished := start.Add(90 * time.Second)
	d := &registry.Dispatch{
		ID: "d", ConversationID: "conv/1", Status: registry.DispatchStatusSucceeded,
		Reply: "\n\nAll pods are healthy.\nDetails follow.", CreatedAt: start, StartedAt: &start, FinishedAt: &finished,
	}

	e, ok := n.event(ctx, d)
	if !ok {
		t.Fatalf("no event for a 90s turn")
	}
	want := notify.Event{Kind: notify.KindDone, Workspace: "theWyseKube", Summary: "All pods are healthy.\nDetails follow.",
		Link: "https://loomux.example/conversations/conv%2F1"}
	if e != want {
		t.Fatalf("event = %+v\nwant  %+v", e, want)
	}

	// Stopped on a prompt: needs you, with what it asks.
	task, _ := store.GetTask(ctx, "task")
	task.Status = registry.TaskStatusNeedsAttention
	task.Attention = &registry.Attention{Title: "Bash command", Detail: "kubectl delete pod x", Question: "Do you want to proceed?"}
	if err := store.UpdateTask(ctx, task); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	e, _ = n.event(ctx, d)
	if e.Kind != notify.KindNeedsYou || e.Summary != "Bash command: kubectl delete pod x — Do you want to proceed?" {
		t.Fatalf("needs-you event = %+v", e)
	}

	// Failed: the error is the summary.
	d.Status, d.Error = registry.DispatchStatusFailed, "the agent exited"
	e, _ = n.event(ctx, d)
	if e.Kind != notify.KindFailed || e.Summary != "the agent exited" {
		t.Fatalf("failed event = %+v", e)
	}

	// A quick turn: the user was watching.
	quick := start.Add(10 * time.Second)
	d.FinishedAt = &quick
	if _, ok := n.event(ctx, d); ok {
		t.Fatalf("an event for a 10s turn")
	}
}

// A turn that touched no task (a direct answer) still notifies, with no
// workspace; without a public URL there is no link.
func TestTurnNotifierEventNoTask(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "n.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	start := time.Now().UTC().Add(-time.Hour)
	end := start.Add(time.Minute)
	n := &turnNotifier{store: store, minDuration: 30 * time.Second}
	e, ok := n.event(ctx, &registry.Dispatch{ConversationID: "c", Status: registry.DispatchStatusSucceeded, Reply: "hi", CreatedAt: start, FinishedAt: &end})
	if !ok || e != (notify.Event{Kind: notify.KindDone, Summary: "hi"}) {
		t.Fatalf("event = %+v, %v", e, ok)
	}
}
