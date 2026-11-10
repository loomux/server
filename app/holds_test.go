package app

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/registry/sqlite"
)

// LOOM-263: at startup every task in human-takeover without an active
// drive hold is logged, and only those.
func TestWarnUnheldTakeovers(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "holds.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.CreateTarget(ctx, &registry.Target{ID: "t", Name: "t", Kind: registry.TargetKindLocal}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateWorkspace(ctx, &registry.Workspace{ID: "w", Name: "w", Path: "/w", TargetID: "t", Status: registry.WorkspaceStatusIdle}); err != nil {
		t.Fatal(err)
	}
	for id, status := range map[string]registry.TaskStatus{
		"k-unheld":       registry.TaskStatusHumanTakeover,
		"k-held":         registry.TaskStatusHumanTakeover,
		"k-ended-hold":   registry.TaskStatusHumanTakeover,
		"k-only-watch":   registry.TaskStatusHumanTakeover,
		"k-not-takeover": registry.TaskStatusAwaitingInput,
	} {
		if err := store.CreateTask(ctx, &registry.Task{ID: id, WorkspaceID: "w", Kind: registry.TaskKindAgent,
			TmuxSession: "loomux-" + id, Status: status, ConversationID: "conv-" + id}); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	for id, mode := range map[string]registry.HoldMode{"k-held": registry.HoldModeDrive, "k-ended-hold": registry.HoldModeDrive, "k-only-watch": registry.HoldModeWatch} {
		if err := store.CreateTaskHold(ctx, &registry.TaskHold{ID: "h-" + id, TaskID: id, SessionID: "s", Mode: mode,
			TTL: time.Minute, AcquiredAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := store.EndTaskHold(ctx, "h-k-ended-hold", registry.HoldEndExpired, now); err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	warnUnheldTakeovers(ctx, store, slog.New(slog.NewTextHandler(&logs, nil)))
	out := logs.String()
	for _, want := range []string{"task_id=k-unheld", "task_id=k-ended-hold", "task_id=k-only-watch", "conversation_id=conv-k-unheld", "level=WARN"} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %s:\n%s", want, out)
		}
	}
	for _, not := range []string{"task_id=k-held", "task_id=k-not-takeover"} {
		if strings.Contains(out, not) {
			t.Errorf("log names %s, which has a hold or isn't taken over:\n%s", not, out)
		}
	}
	if n := strings.Count(out, "\n"); n != 3 {
		t.Errorf("%d log lines, want 3:\n%s", n, out)
	}

	// A nil logger is tolerated: app.Config.Logger is optional.
	warnUnheldTakeovers(ctx, store, nil)
}
