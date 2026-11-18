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

// LOOM-265: StartHoldSweeper gives the holds a previous run left a full
// lease from the start before it sweeps, so a hold whose lease ran out
// during the downtime isn't ended at once.
func TestStartHoldSweeper_RestartGrace(t *testing.T) {
	srv := fakeRouterServer(t)
	a, err := Build(testConfig(t, srv.URL))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer a.Close()
	ctx := context.Background()
	if err := a.store.CreateTarget(ctx, &registry.Target{ID: "t", Name: "t", Kind: registry.TargetKindLocal}); err != nil {
		t.Fatal(err)
	}
	if err := a.store.CreateWorkspace(ctx, &registry.Workspace{ID: "w", Name: "w", Path: "/w", TargetID: "t", Status: registry.WorkspaceStatusActive}); err != nil {
		t.Fatal(err)
	}
	if err := a.store.CreateTask(ctx, &registry.Task{ID: "k", WorkspaceID: "w", Kind: registry.TaskKindAgent,
		TmuxSession: "loomux-k", Status: registry.TaskStatusHumanTakeover, ConversationID: "conv-k"}); err != nil {
		t.Fatal(err)
	}
	if err := a.store.CreateSession(ctx, &registry.Session{ID: "s", TokenHash: "s-hash"}); err != nil {
		t.Fatal(err)
	}
	// Taken 10 minutes ago on a 45 s lease: expired while "down".
	if err := a.store.CreateTaskHold(ctx, &registry.TaskHold{ID: "h", TaskID: "k", SessionID: "s", Mode: registry.HoldModeDrive,
		TTL: 45 * time.Second, AcquiredAt: time.Now().UTC().Add(-10 * time.Minute)}); err != nil {
		t.Fatal(err)
	}

	start := time.Now().UTC()
	a.StartHoldSweeper(time.Hour, 0)
	deadline := time.Now().Add(5 * time.Second)
	for {
		h, err := a.store.GetTaskHold(ctx, "h")
		if err != nil {
			t.Fatal(err)
		}
		if !h.Active() {
			t.Fatalf("hold ended (%s) at start; want it given a lease", h.EndedReason)
		}
		if !h.ExpiresAt.Before(start.Add(45 * time.Second)) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expires_at = %v; never moved to a lease from the start", h.ExpiresAt)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
