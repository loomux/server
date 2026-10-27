package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Loomux/server/internal/metrics"
	"github.com/Loomux/server/notify"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/registry/sqlite"
)

// openNotifyStore opens a store whose vault can be read, as the
// notifier's redaction needs.
func openNotifyStore(t *testing.T) *sqlite.Store {
	t.Helper()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "n.db"), sqlite.WithMasterKey(bytes.Repeat([]byte{3}, 32)))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestTurnNotifierEvent(t *testing.T) {
	ctx := context.Background()
	store := openNotifyStore(t)
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

	// Cancelled: the user did it themselves.
	d.ErrorClass = registry.ErrorClassCancelled
	if _, ok := n.event(ctx, d); ok {
		t.Fatalf("an event for a cancelled turn")
	}
	d.ErrorClass = ""

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
	store := openNotifyStore(t)
	start := time.Now().UTC().Add(-time.Hour)
	end := start.Add(time.Minute)
	n := &turnNotifier{store: store, minDuration: 30 * time.Second}
	e, ok := n.event(ctx, &registry.Dispatch{ConversationID: "c", Status: registry.DispatchStatusSucceeded, Reply: "hi", CreatedAt: start, FinishedAt: &end})
	if !ok || e != (notify.Event{Kind: notify.KindDone, Summary: "hi"}) {
		t.Fatalf("event = %+v, %v", e, ok)
	}
}

// LOOM-123: a turn that ended on an offer still awaiting Approve or Deny
// needs the user, and links to the conversation the card is in. Once
// answered, the same turn is plain news.
func TestTurnNotifierEventOffer(t *testing.T) {
	ctx := context.Background()
	store := openNotifyStore(t)
	start := time.Now().UTC().Add(-time.Hour)
	end := start.Add(time.Minute)
	if err := store.CreateConfirmation(ctx, &registry.Confirmation{ID: "conf-1", ConversationID: "c", DispatchID: "d-1",
		Kind: registry.ConfirmationRunCommand, ExpiresAt: end.Add(time.Hour)}); err != nil {
		t.Fatalf("CreateConfirmation: %v", err)
	}
	n := &turnNotifier{store: store, minDuration: 30 * time.Second, publicURL: "https://loomux.example"}
	d := &registry.Dispatch{ID: "d-1", ConversationID: "c", Status: registry.DispatchStatusSucceeded,
		Reply: "I'd run this on jet01", CreatedAt: start, FinishedAt: &end}
	e, ok := n.event(ctx, d)
	if !ok || e.Kind != notify.KindNeedsYou || e.Link != "https://loomux.example/conversations/c" {
		t.Fatalf("event = %+v, %v; want needs_you linking to the conversation", e, ok)
	}

	if err := store.ResolveConfirmation(ctx, "conf-1", registry.ConfirmationApproved, ""); err != nil {
		t.Fatal(err)
	}
	if e, _ := n.event(ctx, d); e.Kind != notify.KindDone {
		t.Errorf("answered offer: kind %s, want done", e.Kind)
	}
}

// LOOM-102 review: nothing a notification carries leaves Loomux with a
// credential value in it; with the vault unreadable, the body says only
// to open Loomux.
func TestTurnNotifierEventRedacts(t *testing.T) {
	ctx := context.Background()
	store := openNotifyStore(t)
	const secret = "ghp_s3cretvalue"
	if err := store.CreateCredential(ctx, &registry.Credential{ID: "c", Name: "GH_TOKEN", Value: secret}); err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}
	if err := store.CreateTarget(ctx, &registry.Target{ID: "t", Name: "jet01", Kind: registry.TargetKindLocal}); err != nil {
		t.Fatalf("CreateTarget: %v", err)
	}
	if err := store.CreateWorkspace(ctx, &registry.Workspace{ID: "w", Name: "ws", Path: "/p", TargetID: "t", Status: registry.WorkspaceStatusIdle}); err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	start := time.Now().UTC().Add(-time.Hour)
	end := start.Add(time.Minute)
	n := &turnNotifier{store: store}
	d := &registry.Dispatch{ConversationID: "c1", Status: registry.DispatchStatusSucceeded, Reply: "pushed with " + secret,
		CreatedAt: start, FinishedAt: &end}

	check := func(what string) {
		t.Helper()
		e, ok := n.event(ctx, d)
		if !ok || strings.Contains(e.Summary, secret) || !strings.Contains(e.Summary, "[redacted]") {
			t.Errorf("%s: summary %q", what, e.Summary)
		}
	}
	check("reply")

	if err := store.CreateTask(ctx, &registry.Task{ID: "task", WorkspaceID: "w", Kind: registry.TaskKindAgent, TmuxSession: "s",
		Status: registry.TaskStatusNeedsAttention, ConversationID: "c1",
		Attention: &registry.Attention{Title: "Bash command", Detail: "curl -H 'Authorization: Bearer " + secret + "'"}}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	check("attention detail")

	d.Status, d.Error = registry.DispatchStatusFailed, "push rejected for "+secret
	check("error")

	// The vault unreadable (no key to decrypt it): fail closed.
	locked, err := sqlite.Open(filepath.Join(t.TempDir(), "locked.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = locked.Close() })
	n.store = failingCreds{locked}
	e, ok := n.event(ctx, d)
	if !ok || e.Summary != withheldSummary {
		t.Errorf("unreadable vault: summary %q, want %q", e.Summary, withheldSummary)
	}
}

type failingCreds struct{ registry.Store }

func (failingCreds) ListCredentials(context.Context) ([]*registry.Credential, error) {
	return nil, errors.New("vault locked")
}

// An agent's late reply (LOOM-121) is announced as done, with its
// workspace and link, and scrubbed like any other body.
func TestTurnNotifierLateReplyEvent(t *testing.T) {
	ctx := context.Background()
	store := openNotifyStore(t)
	const secret = "ghp_s3cretvalue"
	if err := store.CreateCredential(ctx, &registry.Credential{ID: "c", Name: "GH_TOKEN", Value: secret}); err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}
	if err := store.CreateTarget(ctx, &registry.Target{ID: "t", Name: "jet01", Kind: registry.TargetKindLocal}); err != nil {
		t.Fatalf("CreateTarget: %v", err)
	}
	if err := store.CreateWorkspace(ctx, &registry.Workspace{ID: "w", Name: "my-app", Path: "/p", TargetID: "t", Status: registry.WorkspaceStatusIdle}); err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	n := &turnNotifier{store: store, publicURL: "https://loomux.example"}
	task := &registry.Task{ID: "task", WorkspaceID: "w", ConversationID: "c1"}

	e := n.lateReplyEvent(ctx, task, "  The build passed; pushed with "+secret+"\n")
	if e.Kind != notify.KindDone || e.Workspace != "my-app" || e.Link != "https://loomux.example/conversations/c1" {
		t.Fatalf("event = %+v", e)
	}
	if strings.Contains(e.Summary, secret) || !strings.HasPrefix(e.Summary, "The build passed; pushed with [redacted]") {
		t.Fatalf("summary = %q, want it trimmed and scrubbed", e.Summary)
	}
}

// A notification leaves Loomux like a router model's input: under a
// target's relay policy none (a work machine's default), its body says
// nothing of the work — not the reply, the prompt, an error, a late
// reply or an offer — only that there's news and where. last_message and
// full keep the reply.
func TestTurnNotifierRelayPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, purpose, relay string
		withheld             bool
	}{
		{"full", "", registry.RelayFull, false},
		{"last_message", "", registry.RelayLastMessage, false},
		{"none", "", registry.RelayNone, true},
		{"work machine default", registry.TargetPurposeWork, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := openNotifyStore(t)
			if err := store.CreateTarget(ctx, &registry.Target{ID: "t", Name: "sc1", Kind: registry.TargetKindLocal,
				Policy: registry.TargetPolicy{Purpose: tc.purpose, Relay: tc.relay}}); err != nil {
				t.Fatalf("CreateTarget: %v", err)
			}
			if err := store.CreateWorkspace(ctx, &registry.Workspace{ID: "w", Name: "work-repo", Path: "/p", TargetID: "t",
				Status: registry.WorkspaceStatusIdle}); err != nil {
				t.Fatalf("CreateWorkspace: %v", err)
			}
			if err := store.CreateTask(ctx, &registry.Task{ID: "task", WorkspaceID: "w", Kind: registry.TaskKindAgent,
				TmuxSession: "s", Status: registry.TaskStatusAwaitingInput, ConversationID: "c"}); err != nil {
				t.Fatalf("CreateTask: %v", err)
			}
			n := &turnNotifier{store: store, minDuration: time.Minute}
			start := time.Now().UTC().Add(-5 * time.Minute)
			end := start.Add(2 * time.Minute)
			d := &registry.Dispatch{ID: "d", ConversationID: "c", Status: registry.DispatchStatusSucceeded,
				Reply: "WORK-CONTENT: quarterly numbers", CreatedAt: start, FinishedAt: &end}

			check := func(what string, e notify.Event) {
				t.Helper()
				if tc.withheld && (e.Summary != withheldSummary || strings.Contains(e.Summary, "WORK-CONTENT")) {
					t.Errorf("%s: summary = %q, want only %q", what, e.Summary, withheldSummary)
				}
				if !tc.withheld && !strings.Contains(e.Summary, "WORK-CONTENT") {
					t.Errorf("%s: summary = %q, want the reply kept", what, e.Summary)
				}
			}
			e, _ := n.event(ctx, d)
			check("done", e)
			if e.Workspace != "work-repo" {
				t.Errorf("workspace = %q, want it named either way", e.Workspace)
			}

			task, _ := store.GetTask(ctx, "task")
			task.Status = registry.TaskStatusNeedsAttention
			task.Attention = &registry.Attention{Title: "WORK-CONTENT prompt", Question: "Proceed?"}
			if err := store.UpdateTask(ctx, task); err != nil {
				t.Fatalf("UpdateTask: %v", err)
			}
			e, _ = n.event(ctx, d)
			check("needs you", e)

			d.Status, d.Error = registry.DispatchStatusFailed, "WORK-CONTENT in the agent's last output"
			e, _ = n.event(ctx, d)
			check("failed", e)

			check("late reply", n.lateReplyEvent(ctx, task, "WORK-CONTENT, later"))

			// An offer: no task, but its logged messages name the target.
			offer := &registry.Dispatch{ID: "d2", ConversationID: "c2", Status: registry.DispatchStatusSucceeded,
				Reply: "WORK-CONTENT: I'd run this on sc1", CreatedAt: start, FinishedAt: &end}
			if err := store.CreateDispatch(ctx, &registry.Dispatch{ID: "d2", ConversationID: "c2", Message: "check it"}, nil); err != nil {
				t.Fatalf("CreateDispatch: %v", err)
			}
			if err := store.CreateMessage(ctx, &registry.Message{ID: "m", ConversationID: "c2", DispatchID: "d2", OriginTargetID: "t",
				Role: registry.MessageRoleAssistant, Content: "offer"}); err != nil {
				t.Fatalf("CreateMessage: %v", err)
			}
			e, _ = n.event(ctx, offer)
			check("offer", e)
		})
	}
}

// LOOM-250: a turn that stopped on a prompt notifies however short it
// was; a short done or failed turn is still skipped, and the skip is
// logged and counted (LOOM-249).
func TestTurnNotifierShortNeedsYou(t *testing.T) {
	ctx := context.Background()
	store := openNotifyStore(t)
	if err := store.CreateTarget(ctx, &registry.Target{ID: "t", Name: "jet01", Kind: registry.TargetKindLocal}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateWorkspace(ctx, &registry.Workspace{ID: "w", Name: "ws", Path: "/p", TargetID: "t", Status: registry.WorkspaceStatusIdle}); err != nil {
		t.Fatal(err)
	}
	start := time.Now().UTC().Add(-time.Minute)
	if err := store.CreateTask(ctx, &registry.Task{
		ID: "task", WorkspaceID: "w", Kind: registry.TaskKindAgent, TmuxSession: "s",
		Status: registry.TaskStatusNeedsAttention, ConversationID: "c",
		Attention: &registry.Attention{Question: "Do you want to proceed?"},
	}); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	met := metrics.NewDiscard()
	n := &turnNotifier{store: store, minDuration: 30 * time.Second, metrics: met,
		logger: slog.New(slog.NewTextHandler(&logs, nil))}
	quick := start.Add(15 * time.Second)
	d := &registry.Dispatch{ID: "d", ConversationID: "c", Status: registry.DispatchStatusSucceeded,
		CreatedAt: start, StartedAt: &start, FinishedAt: &quick}

	e, ok := n.event(ctx, d)
	if !ok || e.Kind != notify.KindNeedsYou || e.Summary != "Do you want to proceed?" {
		t.Fatalf("15s needs-you turn: event = %+v, %v; want a needs-you", e, ok)
	}

	task, _ := store.GetTask(ctx, "task")
	task.Status, task.Attention = registry.TaskStatusAwaitingInput, nil
	if err := store.UpdateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if _, ok := n.event(ctx, d); ok {
		t.Fatalf("an event for a 15s done turn")
	}
	if got := notificationCount(t, met, "done", metrics.NotifySkippedShort); got != 1 {
		t.Errorf("skipped_short counter = %v, want 1", got)
	}
	if !strings.Contains(logs.String(), "notification skipped: short turn") {
		t.Errorf("no log line for the skip:\n%s", logs.String())
	}
}

// LOOM-249: an event the rate limit holds back is logged and counted,
// not dropped silently; one sent is counted too.
func TestTurnNotifierSendCountsOutcomes(t *testing.T) {
	var logs bytes.Buffer
	met := metrics.NewDiscard()
	rec := &recordingNotifier{}
	n := &turnNotifier{metrics: met, filter: notify.NewFilter(rec, notify.FilterConfig{
		Events: map[notify.Kind]bool{notify.KindDone: true}, Burst: 1, Refill: time.Hour,
		AfterFunc: func(time.Duration, func()) func() bool { return func() bool { return true } },
	})}
	log := slog.New(slog.NewTextHandler(&logs, nil))
	ctx := context.Background()
	n.send(ctx, notify.Event{Kind: notify.KindDone}, log)
	n.send(ctx, notify.Event{Kind: notify.KindDone}, log)
	n.send(ctx, notify.Event{Kind: notify.KindFailed}, log)
	for outcome, want := range map[string]int{metrics.NotifySent: 1, metrics.NotifyHeld: 1} {
		if got := notificationCount(t, met, "done", outcome); got != want {
			t.Errorf("done %s = %d, want %d", outcome, got, want)
		}
	}
	if got := notificationCount(t, met, "failed", metrics.NotifyOptedOut); got != 1 {
		t.Errorf("failed opted_out = %v, want 1", got)
	}
	if !strings.Contains(logs.String(), "notification held") {
		t.Errorf("no log line for the held event:\n%s", logs.String())
	}
}

type recordingNotifier struct{ n atomic.Int32 }

func (r *recordingNotifier) Notify(context.Context, notify.Event) error { r.n.Add(1); return nil }

// notificationCount scrapes met for loomux_notifications_total{kind,
// outcome}; 0 when it isn't there.
func notificationCount(t *testing.T, met *metrics.Metrics, kind, outcome string) int {
	t.Helper()
	rec := httptest.NewRecorder()
	met.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body, _ := io.ReadAll(rec.Body)
	prefix := `loomux_notifications_total{kind="` + kind + `",outcome="` + outcome + `"} `
	for _, line := range strings.Split(string(body), "\n") {
		if v, ok := strings.CutPrefix(line, prefix); ok {
			var n int
			if _, err := fmt.Sscan(v, &n); err != nil {
				t.Fatalf("parse %q: %v", line, err)
			}
			return n
		}
	}
	return 0
}
