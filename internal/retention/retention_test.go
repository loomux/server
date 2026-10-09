package retention_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Loomux/server/internal/metrics"
	"github.com/Loomux/server/internal/retention"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/registry/sqlite"
)

const day = 24 * time.Hour

// LOOM-193: with the defaults, a session goes 7 days after it expired
// (by its sliding window or its absolute lifetime), a dispatch 90 days
// after it finished and a confirmation 30 days after it was answered.
// A running dispatch, a pending confirmation and every message stay.
// Each sweep's counts are logged and counted in the metrics.
func TestSweep(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	start := time.Now().UTC()

	for id, lastUsed := range map[string]time.Time{
		"s-old":   start.Add(-40 * day), // slid out 10 days ago
		"s-grace": start.Add(-35 * day), // slid out 5 days ago
		"s-max":   start,                // used until the end of its lifetime, below
	} {
		if err := store.CreateSession(ctx, &registry.Session{ID: id, TokenHash: "h-" + id, LastUsedAt: lastUsed}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.TouchSession(ctx, "s-max", start.Add(95*day)); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"d-done", "d-running"} {
		d := &registry.Dispatch{ID: id, ConversationID: "conv-" + id, Message: "hi", RequestHash: "h", Status: registry.DispatchStatusRunning, StartedAt: &start}
		msg := &registry.Message{ID: "m-" + id, ConversationID: d.ConversationID, Role: registry.MessageRoleUser, Content: "hi"}
		if err := store.CreateDispatch(ctx, d, msg); err != nil {
			t.Fatal(err)
		}
		if id == "d-done" {
			d.Status, d.FinishedAt = registry.DispatchStatusSucceeded, &start
			if err := store.TransitionDispatch(ctx, d, registry.DispatchStatusRunning); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, id := range []string{"c-done", "c-pending"} {
		if err := store.CreateConfirmation(ctx, &registry.Confirmation{ID: id, ConversationID: "conv", Kind: registry.ConfirmationPolicy, ExpiresAt: start.Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.ResolveConfirmation(ctx, "c-done", registry.ConfirmationApproved); err != nil {
		t.Fatal(err)
	}

	now := start
	var logs bytes.Buffer
	m := metrics.NewDiscard()
	sweeper := retention.New(store, retention.Config{
		Sessions: 7 * day, SessionTTL: 30 * day, SessionMaxAge: 90 * day,
		Dispatches: 90 * day, Confirmations: 30 * day,
	}, retention.WithClock(func() time.Time { return now }), retention.WithMetrics(m),
		retention.WithLogger(slog.New(slog.NewTextHandler(&logs, nil))))

	for _, step := range []struct {
		at   time.Duration
		want retention.Counts
	}{
		{0, retention.Counts{Sessions: 1}},             // s-old
		{3 * day, retention.Counts{Sessions: 1}},       // s-grace, 8 days after it slid out
		{29 * day, retention.Counts{}},                 // c-done is 29 days old
		{31 * day, retention.Counts{Confirmations: 1}}, // c-done
		{89 * day, retention.Counts{}},                 // d-done is 89 days old
		{91 * day, retention.Counts{Dispatches: 1}},    // d-done; s-max's lifetime ended a day ago
		{98 * day, retention.Counts{Sessions: 1}},      // s-max, 8 days after its lifetime ended
		{1000 * day, retention.Counts{}},               // nothing else ever goes
	} {
		now = start.Add(step.at)
		if got := sweeper.Sweep(ctx); got != step.want {
			t.Errorf("sweep at +%s = %+v; want %+v", step.at, got, step.want)
		}
	}

	if d, err := store.GetDispatch(ctx, "d-running"); err != nil || d.Status != registry.DispatchStatusRunning {
		t.Errorf("running dispatch = %+v, %v; want it kept", d, err)
	}
	if list, err := store.ListConfirmationsByConversation(ctx, "conv"); err != nil || len(list) != 1 || list[0].ID != "c-pending" {
		t.Errorf("confirmations left = %+v, %v; want c-pending", list, err)
	}
	for _, conv := range []string{"conv-d-done", "conv-d-running"} {
		if msgs, err := store.ListMessagesByConversation(ctx, conv); err != nil || len(msgs) != 1 {
			t.Errorf("messages of %s = %+v, %v; want kept", conv, msgs, err)
		}
	}
	if list, err := store.ListSessions(ctx); err != nil || len(list) != 0 {
		t.Errorf("sessions left = %+v, %v; want none", list, err)
	}

	body := scrape(t, m)
	for _, want := range []string{
		`loomux_retention_deleted_total{table="sessions"} 3`,
		`loomux_retention_deleted_total{table="dispatches"} 1`,
		`loomux_retention_deleted_total{table="confirmations"} 1`,
		"loomux_retention_last_sweep_timestamp_seconds",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics lack %s:\n%s", want, body)
		}
	}
	if !strings.Contains(logs.String(), `msg="retention sweep" sessions=1 dispatches=0 confirmations=0`) {
		t.Errorf("logs lack the sweep's counts:\n%s", logs.String())
	}
}

// LOOM-193: a zero retention keeps that kind of row however old it is.
func TestSweep_ZeroKeeps(t *testing.T) {
	ctx := context.Background()
	store := &fakeStore{}
	retention.New(store, retention.Config{SessionTTL: day}, retention.WithClock(func() time.Time {
		return time.Now().Add(1000 * day)
	})).Sweep(ctx)
	if store.calls != 0 {
		t.Errorf("%d deletes with every retention 0; want none", store.calls)
	}
}

// LOOM-193: one delete failing is logged and counted, what it deleted
// before failing still counts, and the others still run.
func TestSweep_FailureCounted(t *testing.T) {
	ctx := context.Background()
	store := &fakeStore{dispatchErr: errors.New("disk I/O error"), n: 2}
	m := metrics.NewDiscard()
	var logs bytes.Buffer
	got := retention.New(store, retention.Config{Sessions: day, Dispatches: day, Confirmations: day},
		retention.WithMetrics(m), retention.WithLogger(slog.New(slog.NewTextHandler(&logs, nil)))).Sweep(ctx)
	if want := (retention.Counts{Sessions: 2, Dispatches: 2, Confirmations: 2}); got != want {
		t.Errorf("Sweep = %+v; want %+v", got, want)
	}
	body := scrape(t, m)
	for _, want := range []string{`loomux_retention_errors_total{table="dispatches"} 1`, `loomux_retention_deleted_total{table="dispatches"} 2`} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics lack %s:\n%s", want, body)
		}
	}
	if !strings.Contains(logs.String(), "table=dispatches") || !strings.Contains(logs.String(), "disk I/O error") {
		t.Errorf("logs lack the failure:\n%s", logs.String())
	}
}

type fakeStore struct {
	calls       int
	n           int
	dispatchErr error
}

func (f *fakeStore) DeleteSessionsExpiredBefore(context.Context, time.Time, time.Time) (int, error) {
	f.calls++
	return f.n, nil
}

func (f *fakeStore) DeleteDispatchesFinishedBefore(context.Context, time.Time) (int, error) {
	f.calls++
	return f.n, f.dispatchErr
}

func (f *fakeStore) DeleteConfirmationsResolvedBefore(context.Context, time.Time) (int, error) {
	f.calls++
	return f.n, nil
}

func scrape(t *testing.T, m *metrics.Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body, err := io.ReadAll(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}
