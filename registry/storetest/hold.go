package storetest

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Loomux/server/registry"
)

// runHolds is the conformance suite for task holds (LOOM-263).
func runHolds(t *testing.T, newStore func(t *testing.T) registry.Store) {
	t.Run("TaskHold", func(t *testing.T) { testTaskHoldRoundTrip(t, newStore(t)) })
	t.Run("TaskHoldOneActiveDrive", func(t *testing.T) { testTaskHoldOneActiveDrive(t, newStore(t)) })
	t.Run("TaskHoldCreateConcurrent", func(t *testing.T) { testTaskHoldCreateConcurrent(t, newStore(t)) })
	t.Run("TaskHoldWatchNotLimited", func(t *testing.T) { testTaskHoldWatchNotLimited(t, newStore(t)) })
	t.Run("TaskHoldRejects", func(t *testing.T) { testTaskHoldRejects(t, newStore(t)) })
	t.Run("TaskHoldRenew", func(t *testing.T) { testTaskHoldRenew(t, newStore(t)) })
	t.Run("TaskHoldEnd", func(t *testing.T) { testTaskHoldEnd(t, newStore(t)) })
	t.Run("TaskHoldEndConcurrent", func(t *testing.T) { testTaskHoldEndConcurrent(t, newStore(t)) })
	t.Run("TaskHoldAcquireDrive", func(t *testing.T) { testTaskHoldAcquireDrive(t, newStore(t)) })
	t.Run("TaskHoldAcquireReplaces", func(t *testing.T) { testTaskHoldAcquireReplaces(t, newStore(t)) })
	t.Run("TaskHoldAcquireConcurrent", func(t *testing.T) { testTaskHoldAcquireConcurrent(t, newStore(t)) })
	t.Run("TaskHoldRelease", func(t *testing.T) { testTaskHoldRelease(t, newStore(t)) })
	t.Run("TaskHoldReleaseTakeBack", func(t *testing.T) { testTaskHoldReleaseTakeBack(t, newStore(t)) })
	t.Run("TaskHoldReleaseKeepsOtherStatuses", func(t *testing.T) { testTaskHoldReleaseKeepsOtherStatuses(t, newStore(t)) })
	t.Run("TaskHoldReleaseVersusAcquire", func(t *testing.T) { testTaskHoldReleaseVersusAcquire(t, newStore(t)) })
	t.Run("TaskHoldListActive", func(t *testing.T) { testTaskHoldListActive(t, newStore(t)) })
	t.Run("TaskHoldExtendActive", func(t *testing.T) { testTaskHoldExtendActive(t, newStore(t)) })
	t.Run("TaskHoldsDeletedWithTask", func(t *testing.T) { testTaskHoldsDeletedWithTask(t, newStore(t)) })
	t.Run("DeleteTaskHoldsEndedBefore", func(t *testing.T) { testDeleteTaskHoldsEndedBefore(t, newStore(t)) })
}

// holdTasks creates n tasks in one fixture workspace, named prefix-0…
func holdTasks(t *testing.T, s registry.Store, prefix string, n int) []*registry.Task {
	t.Helper()
	ws := createTestWorkspace(t, s)
	out := make([]*registry.Task, n)
	for i := range n {
		id := fmt.Sprintf("%s-%d", prefix, i)
		out[i] = &registry.Task{ID: id, WorkspaceID: ws.ID, Kind: registry.TaskKindAgent, AgentType: "claude-code",
			TmuxSession: "loomux-" + id, Status: registry.TaskStatusAwaitingInput, ConversationID: "conv-" + id}
		if err := s.CreateTask(context.Background(), out[i]); err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
	}
	return out
}

func driveHold(id, taskID, sessionID string, acquired time.Time) *registry.TaskHold {
	return &registry.TaskHold{ID: id, TaskID: taskID, SessionID: sessionID, ClientKind: "tui", ClientLabel: "laptop",
		Mode: registry.HoldModeDrive, PrevStatus: registry.TaskStatusAwaitingInput,
		TTL: 45 * time.Second, AcquiredAt: acquired}
}

// holdTime is a fixed instant with no sub-second part, so a store that
// keeps less precision than Go's clock still round-trips it.
func holdTime() time.Time { return time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC) }

func testTaskHoldRoundTrip(t *testing.T, s registry.Store) {
	ctx := context.Background()
	task := holdTasks(t, s, "hold-rt", 1)[0]
	at := holdTime()
	h := driveHold("h1", task.ID, "sess-1", at)
	// Whatever the caller left in these, a created hold is active and its
	// lease runs from its acquire time.
	ended := at
	h.EndedAt, h.EndedReason = &ended, registry.HoldEndReleased
	h.RenewedAt, h.ExpiresAt = at.Add(-time.Hour), at.Add(time.Hour)
	if err := s.CreateTaskHold(ctx, h); err != nil {
		t.Fatalf("CreateTaskHold: %v", err)
	}
	if h.EndedAt != nil || h.EndedReason != "" || !h.RenewedAt.Equal(at) || !h.ExpiresAt.Equal(at.Add(45*time.Second)) {
		t.Errorf("after create: ended_at=%v reason=%q renewed_at=%v expires_at=%v; want an active hold, renewed at its acquire time, expiring a TTL later",
			h.EndedAt, h.EndedReason, h.RenewedAt, h.ExpiresAt)
	}
	got, err := s.GetTaskHold(ctx, "h1")
	if err != nil {
		t.Fatalf("GetTaskHold: %v", err)
	}
	if got.ID != "h1" || got.TaskID != task.ID || got.SessionID != "sess-1" || got.ClientKind != "tui" || got.ClientLabel != "laptop" ||
		got.Mode != registry.HoldModeDrive || got.PrevStatus != registry.TaskStatusAwaitingInput ||
		got.TTL != 45*time.Second || !got.AcquiredAt.Equal(at) || !got.RenewedAt.Equal(at) || !got.ExpiresAt.Equal(at.Add(45*time.Second)) ||
		got.EndedAt != nil || got.EndedReason != "" || !got.Active() {
		t.Errorf("GetTaskHold = %+v", got)
	}
	active, err := s.GetActiveDriveHold(ctx, task.ID)
	if err != nil || active.ID != "h1" {
		t.Errorf("GetActiveDriveHold = %+v, %v; want h1", active, err)
	}
	if _, err := s.GetTaskHold(ctx, "no-such"); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("GetTaskHold(unknown) = %v; want ErrNotFound", err)
	}
	if _, err := s.GetActiveDriveHold(ctx, "no-such-task"); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("GetActiveDriveHold(unknown task) = %v; want ErrNotFound", err)
	}

	// AcquiredAt is stamped when the caller leaves it zero.
	other := holdTasks2(t, s, task.WorkspaceID, "hold-rt-stamp")
	before := time.Now().Add(-time.Second)
	stamped := &registry.TaskHold{ID: "h2", TaskID: other.ID, SessionID: "sess-1", Mode: registry.HoldModeDrive, TTL: time.Minute}
	if err := s.CreateTaskHold(ctx, stamped); err != nil {
		t.Fatalf("CreateTaskHold (no times): %v", err)
	}
	if stamped.AcquiredAt.Before(before) || !stamped.RenewedAt.Equal(stamped.AcquiredAt) || !stamped.ExpiresAt.Equal(stamped.AcquiredAt.Add(time.Minute)) {
		t.Errorf("stamped acquired_at=%v renewed_at=%v expires_at=%v", stamped.AcquiredAt, stamped.RenewedAt, stamped.ExpiresAt)
	}
	// The task is CreateTaskHold's to leave alone.
	if got, _ := s.GetTask(ctx, other.ID); got.Status != registry.TaskStatusAwaitingInput {
		t.Errorf("task after CreateTaskHold = %s; want it untouched", got.Status)
	}
}

// holdTasks2 adds one more task to an existing workspace.
func holdTasks2(t *testing.T, s registry.Store, workspaceID, id string) *registry.Task {
	t.Helper()
	task := &registry.Task{ID: id, WorkspaceID: workspaceID, Kind: registry.TaskKindAgent, AgentType: "claude-code",
		TmuxSession: "loomux-" + id, Status: registry.TaskStatusAwaitingInput, ConversationID: "conv-" + id}
	if err := s.CreateTask(context.Background(), task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	return task
}

// One active drive hold per task: a second is ErrTaskHeld until the first
// ends, and other tasks aren't affected.
func testTaskHoldOneActiveDrive(t *testing.T, s registry.Store) {
	ctx := context.Background()
	tasks := holdTasks(t, s, "hold-one", 2)
	at := holdTime()
	if err := s.CreateTaskHold(ctx, driveHold("h1", tasks[0].ID, "sess-1", at)); err != nil {
		t.Fatalf("CreateTaskHold: %v", err)
	}
	if err := s.CreateTaskHold(ctx, driveHold("h2", tasks[0].ID, "sess-2", at)); !errors.Is(err, registry.ErrTaskHeld) {
		t.Fatalf("second drive hold = %v; want ErrTaskHeld", err)
	}
	if _, err := s.GetTaskHold(ctx, "h2"); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("the refused hold was stored: %v", err)
	}
	if err := s.CreateTaskHold(ctx, driveHold("h3", tasks[1].ID, "sess-2", at)); err != nil {
		t.Fatalf("drive hold on another task: %v", err)
	}
	if _, ended, err := s.EndTaskHold(ctx, "h1", registry.HoldEndReleased, at.Add(time.Minute)); err != nil || !ended {
		t.Fatalf("EndTaskHold = %v, %v", ended, err)
	}
	if err := s.CreateTaskHold(ctx, driveHold("h4", tasks[0].ID, "sess-2", at.Add(2*time.Minute))); err != nil {
		t.Fatalf("drive hold after the first ended: %v", err)
	}
	if active, err := s.GetActiveDriveHold(ctx, tasks[0].ID); err != nil || active.ID != "h4" {
		t.Errorf("GetActiveDriveHold = %+v, %v; want h4", active, err)
	}
	// The ended one is kept.
	if h, err := s.GetTaskHold(ctx, "h1"); err != nil || h.Active() || h.EndedReason != registry.HoldEndReleased {
		t.Errorf("ended hold = %+v, %v", h, err)
	}
}

func testTaskHoldCreateConcurrent(t *testing.T, s registry.Store) {
	ctx := context.Background()
	task := holdTasks(t, s, "hold-race", 1)[0]
	const takers = 8
	results := make(chan error, takers)
	for i := range takers {
		go func() {
			results <- s.CreateTaskHold(ctx, driveHold(fmt.Sprintf("h-%d", i), task.ID, fmt.Sprintf("sess-%d", i), holdTime()))
		}()
	}
	won := 0
	for range takers {
		switch err := <-results; {
		case err == nil:
			won++
		case !errors.Is(err, registry.ErrTaskHeld):
			t.Errorf("losing create = %v; want ErrTaskHeld", err)
		}
	}
	if won != 1 {
		t.Fatalf("%d drive holds created on one task, want exactly 1", won)
	}
	active, err := s.ListActiveTaskHolds(ctx)
	if err != nil || len(active) != 1 {
		t.Fatalf("active holds = %d, %v; want 1", len(active), err)
	}
}

func testTaskHoldWatchNotLimited(t *testing.T, s registry.Store) {
	ctx := context.Background()
	task := holdTasks(t, s, "hold-watch", 1)[0]
	at := holdTime()
	if err := s.CreateTaskHold(ctx, driveHold("drive", task.ID, "sess-1", at)); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		w := driveHold(fmt.Sprintf("watch-%d", i), task.ID, "sess-2", at.Add(time.Duration(i+1)*time.Second))
		w.Mode = registry.HoldModeWatch
		if err := s.CreateTaskHold(ctx, w); err != nil {
			t.Fatalf("watch hold %d beside a drive hold: %v", i, err)
		}
	}
	active, err := s.ListActiveTaskHolds(ctx)
	if err != nil || len(active) != 4 {
		t.Fatalf("active holds = %d, %v; want 4", len(active), err)
	}
	if h, err := s.GetActiveDriveHold(ctx, task.ID); err != nil || h.ID != "drive" {
		t.Errorf("GetActiveDriveHold = %+v, %v; want the drive hold, never a watch", h, err)
	}
	// With only watch holds left, the task has no drive hold.
	if _, _, err := s.EndTaskHold(ctx, "drive", registry.HoldEndReleased, at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetActiveDriveHold(ctx, task.ID); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("GetActiveDriveHold with only watch holds = %v; want ErrNotFound", err)
	}
}

func testTaskHoldRejects(t *testing.T, s registry.Store) {
	ctx := context.Background()
	task := holdTasks(t, s, "hold-rej", 1)[0]
	at := holdTime()
	if err := s.CreateTaskHold(ctx, driveHold("h1", "no-such-task", "sess-1", at)); !errors.Is(err, registry.ErrConflict) {
		t.Errorf("hold on an unknown task = %v; want ErrConflict", err)
	}
	bad := driveHold("h2", task.ID, "sess-1", at)
	bad.Mode = "steer"
	if err := s.CreateTaskHold(ctx, bad); err == nil {
		t.Error("a hold with an unknown mode was stored")
	}
	noTTL := driveHold("h2", task.ID, "sess-1", at)
	noTTL.TTL = 0
	if err := s.CreateTaskHold(ctx, noTTL); err == nil {
		t.Error("a hold with no TTL was stored")
	}
	if _, err := s.GetTaskHold(ctx, "h2"); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("a refused hold was stored: %v", err)
	}
	if err := s.CreateTaskHold(ctx, driveHold("h3", task.ID, "sess-1", at)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.EndTaskHold(ctx, "h3", registry.HoldEndReleased, at); err != nil {
		t.Fatal(err)
	}
	// An id already used, by an ended hold too, is a conflict, not "held".
	err := s.CreateTaskHold(ctx, driveHold("h3", task.ID, "sess-1", at))
	if !errors.Is(err, registry.ErrConflict) || errors.Is(err, registry.ErrTaskHeld) {
		t.Errorf("duplicate id = %v; want ErrConflict", err)
	}
	if _, _, err := s.EndTaskHold(ctx, "h3", "bored", at); err == nil {
		t.Error("EndTaskHold accepted an unknown reason")
	}
}

func testTaskHoldRenew(t *testing.T, s registry.Store) {
	ctx := context.Background()
	task := holdTasks(t, s, "hold-renew", 1)[0]
	at := holdTime()
	if err := s.CreateTaskHold(ctx, driveHold("h1", task.ID, "sess-1", at)); err != nil {
		t.Fatal(err)
	}
	// A renewal pushes the expiry out by the hold's own TTL.
	renewed, expires := at.Add(15*time.Second), at.Add(60*time.Second)
	h, err := s.RenewTaskHold(ctx, "h1", renewed)
	if err != nil {
		t.Fatalf("RenewTaskHold: %v", err)
	}
	if !h.RenewedAt.Equal(renewed) || !h.ExpiresAt.Equal(expires) || !h.AcquiredAt.Equal(at) || h.TTL != 45*time.Second || !h.Active() {
		t.Errorf("renewed hold = %+v", h)
	}
	if got, _ := s.GetTaskHold(ctx, "h1"); !got.RenewedAt.Equal(renewed) || !got.ExpiresAt.Equal(expires) {
		t.Errorf("stored after renew = %+v", got)
	}
	// A renewal that would end the lease earlier (two heartbeats landing
	// out of order) doesn't move the expiry back.
	if h, err := s.RenewTaskHold(ctx, "h1", at.Add(5*time.Second)); err != nil || !h.ExpiresAt.Equal(expires) {
		t.Errorf("an older renewal = %+v, %v; want expires_at left at %v", h, err, expires)
	}
	if _, err := s.RenewTaskHold(ctx, "h1", renewed); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RenewTaskHold(ctx, "no-such", renewed); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("RenewTaskHold(unknown) = %v; want ErrNotFound", err)
	}

	// An ended hold isn't brought back: it comes back as it ended.
	endedAt := at.Add(20 * time.Second)
	if _, _, err := s.EndTaskHold(ctx, "h1", registry.HoldEndExpired, endedAt); err != nil {
		t.Fatal(err)
	}
	h, err = s.RenewTaskHold(ctx, "h1", at.Add(30*time.Second))
	if !errors.Is(err, registry.ErrHoldEnded) {
		t.Fatalf("RenewTaskHold(ended) = %v; want ErrHoldEnded", err)
	}
	if h == nil || h.EndedReason != registry.HoldEndExpired || h.EndedAt == nil || !h.EndedAt.Equal(endedAt) ||
		!h.RenewedAt.Equal(renewed) || !h.ExpiresAt.Equal(expires) {
		t.Errorf("hold returned with ErrHoldEnded = %+v; want it unchanged, ended as expired", h)
	}
}

func testTaskHoldEnd(t *testing.T, s registry.Store) {
	ctx := context.Background()
	task := holdTasks(t, s, "hold-end", 1)[0]
	at := holdTime()
	if err := s.CreateTaskHold(ctx, driveHold("h1", task.ID, "sess-1", at)); err != nil {
		t.Fatal(err)
	}
	endedAt := at.Add(time.Minute)
	h, ended, err := s.EndTaskHold(ctx, "h1", registry.HoldEndTakenBack, endedAt)
	if err != nil || !ended {
		t.Fatalf("EndTaskHold = %v, %v", ended, err)
	}
	if h.Active() || !h.EndedAt.Equal(endedAt) || h.EndedReason != registry.HoldEndTakenBack {
		t.Errorf("ended hold = %+v", h)
	}
	// Ending again changes nothing and says so.
	h, ended, err = s.EndTaskHold(ctx, "h1", registry.HoldEndReleased, endedAt.Add(time.Hour))
	if err != nil || ended {
		t.Fatalf("second EndTaskHold = %v, %v; want false, nil", ended, err)
	}
	if !h.EndedAt.Equal(endedAt) || h.EndedReason != registry.HoldEndTakenBack {
		t.Errorf("hold after a second end = %+v; want its first time and reason", h)
	}
	if _, _, err := s.EndTaskHold(ctx, "no-such", registry.HoldEndReleased, at); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("EndTaskHold(unknown) = %v; want ErrNotFound", err)
	}
	if _, err := s.GetActiveDriveHold(ctx, task.ID); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("GetActiveDriveHold after end = %v; want ErrNotFound", err)
	}
	// Every reason the design names is storable.
	for i, reason := range []registry.HoldEndReason{registry.HoldEndReleased, registry.HoldEndExpired, registry.HoldEndTakenBack,
		registry.HoldEndTakenOver, registry.HoldEndSessionRevoked, registry.HoldEndTaskEnded} {
		id := fmt.Sprintf("r-%d", i)
		if err := s.CreateTaskHold(ctx, driveHold(id, task.ID, "sess-1", at)); err != nil {
			t.Fatal(err)
		}
		if h, ended, err := s.EndTaskHold(ctx, id, reason, at); err != nil || !ended || h.EndedReason != reason {
			t.Errorf("EndTaskHold(%s) = %+v, %v, %v", reason, h, ended, err)
		}
	}
}

func testTaskHoldEndConcurrent(t *testing.T, s registry.Store) {
	ctx := context.Background()
	task := holdTasks(t, s, "hold-end-race", 1)[0]
	at := holdTime()
	if err := s.CreateTaskHold(ctx, driveHold("h1", task.ID, "sess-1", at)); err != nil {
		t.Fatal(err)
	}
	reasons := []registry.HoldEndReason{registry.HoldEndReleased, registry.HoldEndExpired, registry.HoldEndTakenBack, registry.HoldEndSessionRevoked}
	type result struct {
		reason registry.HoldEndReason
		ended  bool
		err    error
	}
	results := make(chan result, len(reasons))
	for _, reason := range reasons {
		go func() {
			_, ended, err := s.EndTaskHold(ctx, "h1", reason, at.Add(time.Second))
			results <- result{reason, ended, err}
		}()
	}
	var winner registry.HoldEndReason
	won := 0
	for range reasons {
		r := <-results
		if r.err != nil {
			t.Fatalf("EndTaskHold: %v", r.err)
		}
		if r.ended {
			won++
			winner = r.reason
		}
	}
	if won != 1 {
		t.Fatalf("%d enders ended the hold, want exactly 1", won)
	}
	if h, _ := s.GetTaskHold(ctx, "h1"); h.EndedReason != winner {
		t.Errorf("stored reason = %q, want the winner's %q", h.EndedReason, winner)
	}
}

func testTaskHoldListActive(t *testing.T, s registry.Store) {
	ctx := context.Background()
	if list, err := s.ListActiveTaskHolds(ctx); err != nil || list == nil || len(list) != 0 {
		t.Fatalf("ListActiveTaskHolds (empty) = %v, %v; want an empty slice", list, err)
	}
	tasks := holdTasks(t, s, "hold-list", 3)
	at := holdTime()
	// Created out of order: the list is by acquire time.
	for _, c := range []struct {
		id   string
		task int
		at   time.Duration
	}{{"h-late", 0, 2 * time.Minute}, {"h-early", 1, 0}, {"h-ended", 2, time.Minute}} {
		if err := s.CreateTaskHold(ctx, driveHold(c.id, tasks[c.task].ID, "sess-1", at.Add(c.at))); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.EndTaskHold(ctx, "h-ended", registry.HoldEndReleased, at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListActiveTaskHolds(ctx)
	if err != nil || len(list) != 2 || list[0].ID != "h-early" || list[1].ID != "h-late" {
		t.Fatalf("ListActiveTaskHolds = %+v, %v; want h-early, h-late", list, err)
	}
}

func testTaskHoldsDeletedWithTask(t *testing.T, s registry.Store) {
	ctx := context.Background()
	tasks := holdTasks(t, s, "hold-del", 2)
	at := holdTime()
	for i, id := range []string{"h-ended", "h-active", "h-other"} {
		task := tasks[0]
		if id == "h-other" {
			task = tasks[1]
		}
		if err := s.CreateTaskHold(ctx, driveHold(id, task.ID, "sess-1", at.Add(time.Duration(i)*time.Second))); err != nil {
			t.Fatal(err)
		}
		if id == "h-ended" {
			if _, _, err := s.EndTaskHold(ctx, id, registry.HoldEndReleased, at); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := s.DeleteTask(ctx, tasks[0].ID); err != nil {
		t.Fatalf("DeleteTask with holds: %v", err)
	}
	for _, id := range []string{"h-ended", "h-active"} {
		if _, err := s.GetTaskHold(ctx, id); !errors.Is(err, registry.ErrNotFound) {
			t.Errorf("%s after its task was deleted = %v; want ErrNotFound", id, err)
		}
	}
	if _, err := s.GetTaskHold(ctx, "h-other"); err != nil {
		t.Errorf("another task's hold: %v", err)
	}
}

// Retention: ended holds go once they ended before the cutoff; an active
// one never does, however old or long expired.
func testDeleteTaskHoldsEndedBefore(t *testing.T, s registry.Store) {
	ctx := context.Background()
	tasks := holdTasks(t, s, "hold-ret", 3)
	now := time.Now().UTC().Truncate(time.Second)
	old := now.Add(-60 * 24 * time.Hour)
	for i, id := range []string{"h-old", "h-recent", "h-active"} {
		if err := s.CreateTaskHold(ctx, driveHold(id, tasks[i].ID, "sess-1", old)); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.EndTaskHold(ctx, "h-old", registry.HoldEndExpired, old.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.EndTaskHold(ctx, "h-recent", registry.HoldEndReleased, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if n, err := s.DeleteTaskHoldsEndedBefore(ctx, old); err != nil || n != 0 {
		t.Fatalf("DeleteTaskHoldsEndedBefore (before any ended) = %d, %v; want 0", n, err)
	}
	if n, err := s.DeleteTaskHoldsEndedBefore(ctx, now.Add(-30*24*time.Hour)); err != nil || n != 1 {
		t.Fatalf("DeleteTaskHoldsEndedBefore (30 days) = %d, %v; want 1", n, err)
	}
	if _, err := s.GetTaskHold(ctx, "h-old"); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("h-old = %v; want deleted", err)
	}
	if n, err := s.DeleteTaskHoldsEndedBefore(ctx, now.Add(time.Hour)); err != nil || n != 1 {
		t.Fatalf("DeleteTaskHoldsEndedBefore (everything ended) = %d, %v; want 1", n, err)
	}
	if h, err := s.GetTaskHold(ctx, "h-active"); err != nil || !h.Active() {
		t.Errorf("the active hold = %+v, %v; want kept", h, err)
	}
}

// takeover is what a caller of AcquireDriveHold does: the task as read,
// with the status it should have afterwards.
func takeover(t *testing.T, s registry.Store, taskID string) (*registry.Task, registry.TaskStatus) {
	t.Helper()
	task, err := s.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	from := task.Status
	task.Status = registry.TaskStatusHumanTakeover
	return task, from
}

// AcquireDriveHold writes the hold and the task's status together, or
// neither.
func testTaskHoldAcquireDrive(t *testing.T, s registry.Store) {
	ctx := context.Background()
	tasks := holdTasks(t, s, "hold-acq", 2)
	at := holdTime()

	// The row gains a prompt after the caller read the task: the takeover
	// writes the status only, never the caller's older copy of the rest.
	task, from := takeover(t, s, tasks[0].ID)
	newer := *tasks[0]
	newer.Attention = &registry.Attention{Kind: registry.AttentionQuestion, Question: "which?"}
	if applied, err := s.TransitionTask(ctx, &newer, from); err != nil || !applied {
		t.Fatalf("TransitionTask: %v, %v", applied, err)
	}
	ended, applied, err := s.AcquireDriveHold(ctx, driveHold("h1", task.ID, "sess-1", at), task, from, "")
	if err != nil || !applied || ended != nil {
		t.Fatalf("AcquireDriveHold = %+v, %v, %v; want nil, true, nil", ended, applied, err)
	}
	got, _ := s.GetTask(ctx, task.ID)
	if got.Status != registry.TaskStatusHumanTakeover || got.Attention == nil || got.Attention.Question != "which?" {
		t.Errorf("task after acquire = %s, attention %+v; want human-takeover and the stored prompt kept", got.Status, got.Attention)
	}
	if h, err := s.GetActiveDriveHold(ctx, task.ID); err != nil || h.ID != "h1" || !h.ExpiresAt.Equal(at.Add(45*time.Second)) {
		t.Errorf("active hold = %+v, %v", h, err)
	}

	// The status moved since the caller read it: nothing is written.
	stale := *tasks[1]
	stale.Status = registry.TaskStatusHumanTakeover
	ended, applied, err = s.AcquireDriveHold(ctx, driveHold("h2", stale.ID, "sess-1", at), &stale, registry.TaskStatusRunning, "")
	if err != nil || applied || ended != nil {
		t.Fatalf("AcquireDriveHold from a stale status = %+v, %v, %v; want nil, false, nil", ended, applied, err)
	}
	if _, err := s.GetTaskHold(ctx, "h2"); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("a hold was stored by a lost compare-and-set: %v", err)
	}
	if got, _ := s.GetTask(ctx, stale.ID); got.Status != registry.TaskStatusAwaitingInput {
		t.Errorf("task after a lost compare-and-set = %s; want it untouched", got.Status)
	}

	// Held already and no replace: ErrTaskHeld, and nothing changes.
	again, from := takeover(t, s, tasks[0].ID)
	if _, _, err := s.AcquireDriveHold(ctx, driveHold("h3", again.ID, "sess-2", at), again, from, ""); !errors.Is(err, registry.ErrTaskHeld) {
		t.Fatalf("AcquireDriveHold on a held task = %v; want ErrTaskHeld", err)
	}
	if h, _ := s.GetActiveDriveHold(ctx, again.ID); h == nil || h.ID != "h1" {
		t.Errorf("active hold after a refused acquire = %+v; want h1", h)
	}

	// An equal status is a hold on a task left as it is (a failed task
	// kept for a person): not even its updated_at moves.
	failed := holdTasks2(t, s, tasks[0].WorkspaceID, "hold-acq-failed")
	failed.Status, failed.ErrorClass, failed.FailureReason = registry.TaskStatusFailed, registry.ErrorClassLoginRequired, "sign in"
	if err := s.UpdateTask(ctx, failed); err != nil {
		t.Fatal(err)
	}
	before, _ := s.GetTask(ctx, failed.ID)
	if _, applied, err := s.AcquireDriveHold(ctx, driveHold("h4", failed.ID, "sess-1", at), failed, registry.TaskStatusFailed, ""); err != nil || !applied {
		t.Fatalf("AcquireDriveHold on a failed task = %v, %v", applied, err)
	}
	after, _ := s.GetTask(ctx, failed.ID)
	if after.Status != registry.TaskStatusFailed || after.ErrorClass != registry.ErrorClassLoginRequired || !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Errorf("failed task after acquire = %+v; want it untouched", after)
	}

	// Refused before anything is written.
	unknown := &registry.Task{ID: "no-such-task", Status: registry.TaskStatusHumanTakeover}
	if _, _, err := s.AcquireDriveHold(ctx, driveHold("h5", unknown.ID, "sess-1", at), unknown, registry.TaskStatusRunning, ""); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("AcquireDriveHold on an unknown task = %v; want ErrNotFound", err)
	}
	watch := driveHold("h6", tasks[1].ID, "sess-1", at)
	watch.Mode = registry.HoldModeWatch
	if _, _, err := s.AcquireDriveHold(ctx, watch, tasks[1], registry.TaskStatusAwaitingInput, ""); err == nil {
		t.Error("AcquireDriveHold accepted a watch hold")
	}
	if _, _, err := s.AcquireDriveHold(ctx, driveHold("h7", tasks[0].ID, "sess-1", at), tasks[1], registry.TaskStatusAwaitingInput, ""); err == nil {
		t.Error("AcquireDriveHold accepted a hold for another task")
	}
}

// A forced takeover: the old hold ends and the new one starts in one
// step, and a failure in between ends nothing.
func testTaskHoldAcquireReplaces(t *testing.T, s registry.Store) {
	ctx := context.Background()
	task := holdTasks(t, s, "hold-replace", 1)[0]
	at := holdTime()
	taken, from := takeover(t, s, task.ID)
	if _, applied, err := s.AcquireDriveHold(ctx, driveHold("h1", task.ID, "sess-1", at), taken, from, ""); err != nil || !applied {
		t.Fatalf("first acquire = %v, %v", applied, err)
	}
	watch := driveHold("w1", task.ID, "sess-3", at)
	watch.Mode = registry.HoldModeWatch
	if err := s.CreateTaskHold(ctx, watch); err != nil {
		t.Fatal(err)
	}

	stolenAt := at.Add(time.Minute)
	cur, from := takeover(t, s, task.ID) // already human-takeover: status stays
	ended, applied, err := s.AcquireDriveHold(ctx, driveHold("h2", task.ID, "sess-2", stolenAt), cur, from, registry.HoldEndTakenOver)
	if err != nil || !applied {
		t.Fatalf("AcquireDriveHold (replace) = %v, %v", applied, err)
	}
	if ended == nil || ended.ID != "h1" || ended.EndedReason != registry.HoldEndTakenOver || ended.EndedAt == nil || !ended.EndedAt.Equal(stolenAt) {
		t.Errorf("ended = %+v; want h1 taken_over at the new hold's acquire time", ended)
	}
	if h, _ := s.GetTaskHold(ctx, "h1"); h.Active() || h.EndedReason != registry.HoldEndTakenOver {
		t.Errorf("stored h1 = %+v", h)
	}
	if h, err := s.GetActiveDriveHold(ctx, task.ID); err != nil || h.ID != "h2" || h.SessionID != "sess-2" {
		t.Errorf("active drive hold = %+v, %v; want h2", h, err)
	}
	if got, _ := s.GetTask(ctx, task.ID); got.Status != registry.TaskStatusHumanTakeover {
		t.Errorf("task after a forced takeover = %s; want human-takeover throughout", got.Status)
	}
	// A watch hold isn't a drive hold: it is left alone.
	if h, _ := s.GetTaskHold(ctx, "w1"); !h.Active() {
		t.Errorf("the watch hold was ended by a drive replace: %+v", h)
	}

	// The insert fails (the id is used): the hold it would have replaced
	// is still active.
	if _, _, err := s.AcquireDriveHold(ctx, driveHold("h1", task.ID, "sess-9", stolenAt.Add(time.Minute)), cur, from, registry.HoldEndTakenOver); !errors.Is(err, registry.ErrConflict) {
		t.Fatalf("AcquireDriveHold with a used id = %v; want ErrConflict", err)
	}
	if h, _ := s.GetTaskHold(ctx, "h2"); !h.Active() || h.SessionID != "sess-2" {
		t.Errorf("h2 after a failed replace = %+v; want it still active", h)
	}
	if _, _, err := s.AcquireDriveHold(ctx, driveHold("h3", task.ID, "sess-9", stolenAt), cur, from, "bored"); err == nil {
		t.Error("AcquireDriveHold accepted an unknown replace reason")
	}
}

// Several sessions take the same waiting task at once: one gets it, the
// rest lose the compare-and-set and write nothing.
func testTaskHoldAcquireConcurrent(t *testing.T, s registry.Store) {
	ctx := context.Background()
	task := holdTasks(t, s, "hold-acq-race", 1)[0]
	const takers = 8
	type result struct {
		applied bool
		err     error
	}
	results := make(chan result, takers)
	for i := range takers {
		go func() {
			mine := *task
			mine.Status = registry.TaskStatusHumanTakeover
			_, applied, err := s.AcquireDriveHold(ctx, driveHold(fmt.Sprintf("h-%d", i), task.ID, fmt.Sprintf("sess-%d", i), holdTime()),
				&mine, registry.TaskStatusAwaitingInput, "")
			results <- result{applied, err}
		}()
	}
	won := 0
	for range takers {
		r := <-results
		if r.err != nil {
			t.Errorf("AcquireDriveHold: %v", r.err)
		}
		if r.applied {
			won++
		}
	}
	if won != 1 {
		t.Fatalf("%d takeovers applied to one task, want exactly 1", won)
	}
	if active, err := s.ListActiveTaskHolds(ctx); err != nil || len(active) != 1 {
		t.Fatalf("active holds = %d, %v; want 1", len(active), err)
	}
	if got, _ := s.GetTask(ctx, task.ID); got.Status != registry.TaskStatusHumanTakeover {
		t.Errorf("task = %s; want human-takeover", got.Status)
	}
}

// Releasing by hold id ends the hold and hands the task back, clearing
// its stale prompt; doing it again changes nothing.
func testTaskHoldRelease(t *testing.T, s registry.Store) {
	ctx := context.Background()
	tasks := holdTasks(t, s, "hold-rel", 2)
	at := holdTime()
	tasks[0].Status = registry.TaskStatusNeedsAttention
	tasks[0].Attention = &registry.Attention{Kind: registry.AttentionPermission, Title: "Bash command"}
	if err := s.UpdateTask(ctx, tasks[0]); err != nil {
		t.Fatal(err)
	}
	task, from := takeover(t, s, tasks[0].ID)
	if _, applied, err := s.AcquireDriveHold(ctx, driveHold("h1", task.ID, "sess-1", at), task, from, ""); err != nil || !applied {
		t.Fatalf("acquire = %v, %v", applied, err)
	}
	if held, _ := s.GetTask(ctx, task.ID); held.Attention == nil {
		t.Fatal("the prompt was cleared by the takeover; it is the release that clears it")
	}

	releasedAt := at.Add(time.Minute)
	res, err := s.ReleaseTaskHold(ctx, task.ID, "h1", registry.HoldEndReleased, releasedAt)
	if err != nil {
		t.Fatalf("ReleaseTaskHold: %v", err)
	}
	if res.Hold == nil || res.Hold.ID != "h1" || !res.Ended || res.Hold.EndedReason != registry.HoldEndReleased || !res.Hold.EndedAt.Equal(releasedAt) {
		t.Errorf("release = hold %+v, ended %v", res.Hold, res.Ended)
	}
	if !res.TaskReleased || res.Task.Status != registry.TaskStatusAwaitingInput || res.Task.Attention != nil {
		t.Errorf("release = task %s attention %+v released %v; want awaiting-input, no attention, true", res.Task.Status, res.Task.Attention, res.TaskReleased)
	}
	if got, _ := s.GetTask(ctx, task.ID); got.Status != registry.TaskStatusAwaitingInput || got.Attention != nil {
		t.Errorf("stored task = %s, attention %+v", got.Status, got.Attention)
	}

	// Again: the hold keeps its first end, and the task isn't touched.
	res, err = s.ReleaseTaskHold(ctx, task.ID, "h1", registry.HoldEndTakenBack, releasedAt.Add(time.Hour))
	if err != nil || res.Ended || res.TaskReleased || res.Hold.EndedReason != registry.HoldEndReleased || !res.Hold.EndedAt.Equal(releasedAt) {
		t.Errorf("second release = %+v, %v; want the hold as it first ended and nothing done", res, err)
	}

	// A stale hold id, after someone else took the task: the hold stays
	// ended and the task stays theirs.
	again, from := takeover(t, s, task.ID)
	if _, applied, err := s.AcquireDriveHold(ctx, driveHold("h2", task.ID, "sess-2", at.Add(2*time.Minute)), again, from, ""); err != nil || !applied {
		t.Fatalf("second acquire = %v, %v", applied, err)
	}
	res, err = s.ReleaseTaskHold(ctx, task.ID, "h1", registry.HoldEndReleased, at.Add(3*time.Minute))
	if err != nil || res.Ended || res.TaskReleased || res.Task.Status != registry.TaskStatusHumanTakeover {
		t.Errorf("release with a stale hold id = %+v, %v; want nothing done", res, err)
	}
	if h, err := s.GetActiveDriveHold(ctx, task.ID); err != nil || h.ID != "h2" {
		t.Errorf("the new holder's hold = %+v, %v; want h2 still active", h, err)
	}

	// Unknown hold, a hold of another task, an unknown task, a bad reason.
	if _, err := s.ReleaseTaskHold(ctx, task.ID, "no-such", registry.HoldEndReleased, at); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("release of an unknown hold = %v; want ErrNotFound", err)
	}
	if _, err := s.ReleaseTaskHold(ctx, tasks[1].ID, "h2", registry.HoldEndReleased, at); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("release of another task's hold = %v; want ErrNotFound", err)
	}
	if h, _ := s.GetTaskHold(ctx, "h2"); !h.Active() {
		t.Errorf("h2 was ended through another task: %+v", h)
	}
	if _, err := s.ReleaseTaskHold(ctx, "no-such-task", "", registry.HoldEndTakenBack, at); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("release on an unknown task = %v; want ErrNotFound", err)
	}
	if _, err := s.ReleaseTaskHold(ctx, task.ID, "h2", "bored", at); err == nil {
		t.Error("ReleaseTaskHold accepted an unknown reason")
	}
}

// Without a hold id it is Take back: whatever drive hold is active ends,
// and a task in human-takeover with no hold at all is still handed back.
func testTaskHoldReleaseTakeBack(t *testing.T, s registry.Store) {
	ctx := context.Background()
	tasks := holdTasks(t, s, "hold-back", 3)
	at := holdTime()
	task, from := takeover(t, s, tasks[0].ID)
	if _, applied, err := s.AcquireDriveHold(ctx, driveHold("h1", task.ID, "sess-1", at), task, from, ""); err != nil || !applied {
		t.Fatalf("acquire = %v, %v", applied, err)
	}
	watch := driveHold("w1", task.ID, "sess-3", at)
	watch.Mode = registry.HoldModeWatch
	if err := s.CreateTaskHold(ctx, watch); err != nil {
		t.Fatal(err)
	}
	res, err := s.ReleaseTaskHold(ctx, task.ID, "", registry.HoldEndTakenBack, at.Add(time.Minute))
	if err != nil || res.Hold == nil || res.Hold.ID != "h1" || !res.Ended || res.Hold.EndedReason != registry.HoldEndTakenBack || !res.TaskReleased {
		t.Fatalf("take back = %+v, %v; want h1 ended as taken_back and the task released", res, err)
	}
	if h, _ := s.GetTaskHold(ctx, "w1"); !h.Active() {
		t.Errorf("take back ended a watch hold: %+v", h)
	}

	// In human-takeover with no hold (taken before holds existed).
	legacy := tasks[1]
	legacy.Status = registry.TaskStatusHumanTakeover
	if err := s.UpdateTask(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	res, err = s.ReleaseTaskHold(ctx, legacy.ID, "", registry.HoldEndTakenBack, at)
	if err != nil || res.Hold != nil || res.Ended || !res.TaskReleased || res.Task.Status != registry.TaskStatusAwaitingInput {
		t.Errorf("take back of an unheld task = %+v, %v; want the task released and no hold", res, err)
	}

	// Nothing to take back: nothing happens.
	res, err = s.ReleaseTaskHold(ctx, tasks[2].ID, "", registry.HoldEndTakenBack, at)
	if err != nil || res.Hold != nil || res.Ended || res.TaskReleased || res.Task.Status != registry.TaskStatusAwaitingInput {
		t.Errorf("take back of a task nobody has = %+v, %v; want nothing done", res, err)
	}

	// A watch hold's release leaves the task where it is, held or not.
	lone := tasks[2]
	lone.Status = registry.TaskStatusHumanTakeover
	if err := s.UpdateTask(ctx, lone); err != nil {
		t.Fatal(err)
	}
	w2 := driveHold("w2", lone.ID, "sess-3", at)
	w2.Mode = registry.HoldModeWatch
	if err := s.CreateTaskHold(ctx, w2); err != nil {
		t.Fatal(err)
	}
	res, err = s.ReleaseTaskHold(ctx, lone.ID, "w2", registry.HoldEndReleased, at)
	if err != nil || !res.Ended || res.TaskReleased || res.Task.Status != registry.TaskStatusHumanTakeover {
		t.Errorf("release of a watch hold = %+v, %v; want the hold ended and the task untouched", res, err)
	}
}

// Only human-takeover is handed back: a held failed task stays failed,
// and a task that ended while held stays ended.
func testTaskHoldReleaseKeepsOtherStatuses(t *testing.T, s registry.Store) {
	ctx := context.Background()
	tasks := holdTasks(t, s, "hold-rel-keep", 2)
	at := holdTime()
	for i, status := range []registry.TaskStatus{registry.TaskStatusFailed, registry.TaskStatusCompleted} {
		task := tasks[i]
		task.Status = status
		if err := s.UpdateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
		id := fmt.Sprintf("h-%d", i)
		if _, applied, err := s.AcquireDriveHold(ctx, driveHold(id, task.ID, "sess-1", at), task, status, ""); err != nil || !applied {
			t.Fatalf("acquire on a %s task = %v, %v", status, applied, err)
		}
		res, err := s.ReleaseTaskHold(ctx, task.ID, id, registry.HoldEndReleased, at.Add(time.Minute))
		if err != nil || !res.Ended || res.TaskReleased || res.Task.Status != status {
			t.Errorf("release on a %s task = %+v, %v; want the hold ended and the status kept", status, res, err)
		}
		if got, _ := s.GetTask(ctx, task.ID); got.Status != status {
			t.Errorf("stored task = %s, want %s", got.Status, status)
		}
	}
}

// A release and a forced takeover of the same task at once: whichever
// lands first, the task is never left open to automation (awaiting-input)
// while a drive hold holds it.
func testTaskHoldReleaseVersusAcquire(t *testing.T, s registry.Store) {
	ctx := context.Background()
	const rounds = 20
	tasks := holdTasks(t, s, "hold-rel-race", rounds)
	at := holdTime()
	for i, task := range tasks {
		first, second := fmt.Sprintf("a-%d", i), fmt.Sprintf("b-%d", i)
		taken, from := takeover(t, s, task.ID)
		if _, applied, err := s.AcquireDriveHold(ctx, driveHold(first, task.ID, "sess-1", at), taken, from, ""); err != nil || !applied {
			t.Fatalf("acquire = %v, %v", applied, err)
		}
		errs := make(chan error, 2)
		go func() {
			_, err := s.ReleaseTaskHold(ctx, task.ID, first, registry.HoldEndReleased, at.Add(time.Second))
			errs <- err
		}()
		go func() {
			mine := *taken // human-takeover, as read while held
			_, _, err := s.AcquireDriveHold(ctx, driveHold(second, task.ID, "sess-2", at.Add(time.Second)), &mine,
				registry.TaskStatusHumanTakeover, registry.HoldEndTakenOver)
			errs <- err
		}()
		for range 2 {
			if err := <-errs; err != nil {
				t.Fatalf("round %d: %v", i, err)
			}
		}
		got, _ := s.GetTask(ctx, task.ID)
		h, err := s.GetActiveDriveHold(ctx, task.ID)
		held := err == nil
		switch {
		case held && got.Status != registry.TaskStatusHumanTakeover:
			t.Fatalf("round %d: task is %s while %s holds it", i, got.Status, h.ID)
		case !held && got.Status != registry.TaskStatusAwaitingInput:
			t.Fatalf("round %d: task is %s with no hold; want it released", i, got.Status)
		}
	}
}

// At startup every active hold gets a full lease of its own length from
// the start time; one that already runs longer is left.
func testTaskHoldExtendActive(t *testing.T, s registry.Store) {
	ctx := context.Background()
	tasks := holdTasks(t, s, "hold-extend", 4)
	at := holdTime()
	short := driveHold("h-short", tasks[0].ID, "sess-1", at) // 45s: expires at +45s
	long := driveHold("h-long", tasks[1].ID, "sess-1", at)
	long.TTL = 5 * time.Minute // expires at +5m
	fresh := driveHold("h-fresh", tasks[2].ID, "sess-1", at.Add(2*time.Minute))
	gone := driveHold("h-ended", tasks[3].ID, "sess-1", at)
	for _, h := range []*registry.TaskHold{short, long, fresh, gone} {
		if err := s.CreateTaskHold(ctx, h); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.EndTaskHold(ctx, "h-ended", registry.HoldEndExpired, at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	// The server comes back two minutes in: h-short gets 45s from then,
	// h-long five minutes from then; h-fresh already has exactly that.
	start := at.Add(2 * time.Minute)
	if n, err := s.ExtendActiveTaskHolds(ctx, start); err != nil || n != 2 {
		t.Fatalf("ExtendActiveTaskHolds = %d, %v; want 2", n, err)
	}
	for id, want := range map[string]time.Time{
		"h-short": start.Add(45 * time.Second),
		"h-long":  start.Add(5 * time.Minute),
		"h-fresh": start.Add(45 * time.Second),
		"h-ended": at.Add(45 * time.Second),
	} {
		h, _ := s.GetTaskHold(ctx, id)
		if !h.ExpiresAt.Equal(want) {
			t.Errorf("%s expires_at = %v, want %v", id, h.ExpiresAt, want)
		}
		if id != "h-fresh" && !h.RenewedAt.Equal(at) {
			t.Errorf("%s renewed_at = %v; want it left at %v (an extension isn't a heartbeat)", id, h.RenewedAt, at)
		}
	}
	if n, err := s.ExtendActiveTaskHolds(ctx, start); err != nil || n != 0 {
		t.Errorf("second ExtendActiveTaskHolds = %d, %v; want 0", n, err)
	}
}
