package sqlite

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/Loomux/server/registry"
)

// LOOM-193: a retention delete removes at most retentionBatch rows per
// statement (each its own transaction, so the write lock is never held
// for the whole backlog), carries on until a chunk comes back short,
// returns the total, and stops between chunks once its context is done.
func TestRetentionDeletesInBatches(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.retentionBatch = 2
	var chunks []int
	s.afterRetentionChunk = func(_ string, n int) { chunks = append(chunks, n) }

	now := time.Now().UTC()
	old := now.Add(-40 * 24 * time.Hour)
	for i := range 5 {
		if err := s.CreateSession(ctx, &registry.Session{ID: fmt.Sprint("s-", i), TokenHash: fmt.Sprint("h-", i), LastUsedAt: old}); err != nil {
			t.Fatal(err)
		}
		d := &registry.Dispatch{ID: fmt.Sprint("d-", i), ConversationID: fmt.Sprint("conv-", i), Message: "hi", RequestHash: "h",
			Status: registry.DispatchStatusRunning, StartedAt: &old}
		msg := &registry.Message{ID: fmt.Sprint("m-", i), ConversationID: d.ConversationID, Role: registry.MessageRoleUser, Content: "hi"}
		if err := s.CreateDispatch(ctx, d, msg); err != nil {
			t.Fatal(err)
		}
		d.Status, d.FinishedAt = registry.DispatchStatusSucceeded, &old
		if err := s.TransitionDispatch(ctx, d, registry.DispatchStatusRunning); err != nil {
			t.Fatal(err)
		}
		c := &registry.Confirmation{ID: fmt.Sprint("c-", i), ConversationID: "conv", Kind: registry.ConfirmationPolicy, ExpiresAt: now}
		if err := s.CreateConfirmation(ctx, c); err != nil {
			t.Fatal(err)
		}
		if err := s.ResolveConfirmation(ctx, c.ID, registry.ConfirmationApproved); err != nil {
			t.Fatal(err)
		}
	}
	// One of each that stays.
	if err := s.CreateSession(ctx, &registry.Session{ID: "s-live", TokenHash: "h-live"}); err != nil {
		t.Fatal(err)
	}

	cutoff := now.Add(-37 * 24 * time.Hour)
	later := now.Add(time.Second)
	for _, tc := range []struct {
		table string
		del   func(context.Context) (int, error)
	}{
		{"sessions", func(ctx context.Context) (int, error) { return s.DeleteSessionsExpiredBefore(ctx, cutoff, time.Time{}) }},
		{"dispatches", func(ctx context.Context) (int, error) { return s.DeleteDispatchesFinishedBefore(ctx, cutoff) }},
		{"confirmations", func(ctx context.Context) (int, error) { return s.DeleteConfirmationsResolvedBefore(ctx, later) }},
	} {
		chunks = nil
		if n, err := tc.del(ctx); err != nil || n != 5 {
			t.Errorf("%s: deleted %d, %v; want 5", tc.table, n, err)
		}
		if want := []int{2, 2, 1}; !slices.Equal(chunks, want) {
			t.Errorf("%s: chunks %v; want %v", tc.table, chunks, want)
		}
	}
	if list, err := s.ListSessions(ctx); err != nil || len(list) != 1 || list[0].ID != "s-live" {
		t.Errorf("sessions left = %+v, %v; want s-live", list, err)
	}
	if msgs, err := s.ListMessagesByConversation(ctx, "conv-0"); err != nil || len(msgs) != 1 || msgs[0].DispatchID != "" {
		t.Errorf("messages of a deleted dispatch = %+v, %v; want kept, no longer naming it", msgs, err)
	}

	// A multiple of the batch ends on an empty chunk.
	for i := range 4 {
		if err := s.CreateSession(ctx, &registry.Session{ID: fmt.Sprint("s2-", i), TokenHash: fmt.Sprint("h2-", i), LastUsedAt: old}); err != nil {
			t.Fatal(err)
		}
	}
	chunks = nil
	if n, err := s.DeleteSessionsExpiredBefore(ctx, cutoff, time.Time{}); err != nil || n != 4 || !slices.Equal(chunks, []int{2, 2, 0}) {
		t.Errorf("4 sessions: deleted %d, %v in chunks %v; want 4 in [2 2 0]", n, err, chunks)
	}

	// Cancelled after the first chunk, it stops there and says how many
	// it deleted.
	for i := range 5 {
		if err := s.CreateSession(ctx, &registry.Session{ID: fmt.Sprint("s3-", i), TokenHash: fmt.Sprint("h3-", i), LastUsedAt: old}); err != nil {
			t.Fatal(err)
		}
	}
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	chunks = nil
	s.afterRetentionChunk = func(_ string, n int) { chunks = append(chunks, n); cancel() }
	n, err := s.DeleteSessionsExpiredBefore(cctx, cutoff, time.Time{})
	if n != 2 || !errors.Is(err, context.Canceled) || len(chunks) != 1 {
		t.Errorf("cancelled delete = %d, %v after chunks %v; want 2, context.Canceled after one", n, err, chunks)
	}
	if list, _ := s.ListSessions(ctx); len(list) != 4 {
		t.Errorf("%d sessions left after a cancelled delete; want 4 (s-live and 3 not reached)", len(list))
	}
}
