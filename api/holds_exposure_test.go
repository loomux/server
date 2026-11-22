package api_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/Loomux/server/api"
	"github.com/Loomux/server/registry"
)

// LOOM-267 (the terminal client's S6): holds are shown where clients
// look (docs/design/tui-client-design.md, "API (additions within v1)",
// Exposure): hold (active or null) on each task of a conversation and on
// attach-info, held_by and pending_confirmation_id on conversation list
// rows, and a task_hold stream event on every acquire, end and steal.

// rawFields decodes raw into its top-level fields, so a test can tell a
// field sent as null from one left out.
func rawFields(t *testing.T, raw []byte) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
	return m
}

// get GETs path under /api/v1 and fails the test unless it is a 200.
func (h *holdServer) get(path, token string) string {
	h.t.Helper()
	status, raw := doJSON(h.t, http.MethodGet, h.base+"/api/v1/"+path, token, nil)
	if status != http.StatusOK {
		h.t.Fatalf("GET %s: %d %s", path, status, raw)
	}
	return raw
}

// conversationTaskHold is GET /conversations/{conv}'s task taskID's hold
// field, raw: "null" when the task isn't held.
func (h *holdServer) conversationTaskHold(conv, taskID string) json.RawMessage {
	h.t.Helper()
	var out struct {
		Tasks []map[string]json.RawMessage `json:"tasks"`
	}
	raw := h.get("conversations/"+conv, h.tokenA)
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		h.t.Fatalf("decode conversation: %v", err)
	}
	for _, task := range out.Tasks {
		var id string
		_ = json.Unmarshal(task["id"], &id)
		if id != taskID {
			continue
		}
		hold, ok := task["hold"]
		if !ok {
			h.t.Fatalf("conversation %s task %s has no hold field: %s", conv, taskID, raw)
		}
		return hold
	}
	h.t.Fatalf("conversation %s has no task %s: %s", conv, taskID, raw)
	return nil
}

func holdIDOf(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	if string(raw) == "null" {
		return ""
	}
	var hold holdJSON
	if err := json.Unmarshal(raw, &hold); err != nil {
		t.Fatalf("decode hold %s: %v", raw, err)
	}
	if hold.ID == "" || hold.EndedAt != nil {
		t.Fatalf("hold %s: want an active hold with an id", raw)
	}
	return hold.ID
}

// A conversation's tasks carry their active hold, and null when there is
// none: before the takeover, while held, and after the release.
func TestConversation_TasksCarryTheirHold(t *testing.T) {
	h := newHoldServer(t)
	h.task("t1", registry.TaskStatusAwaitingInput, "")

	if got := h.conversationTaskHold("conv-t1", "t1"); string(got) != "null" {
		t.Fatalf("hold before takeover = %s, want null", got)
	}
	status, took := h.post("t1/takeover", h.tokenA, map[string]any{"client_label": "laptop"})
	if status != http.StatusOK {
		t.Fatalf("takeover: %d %+v", status, took)
	}
	held := h.conversationTaskHold("conv-t1", "t1")
	if got := holdIDOf(t, held); got != took.Hold.ID {
		t.Fatalf("held task's hold = %s, want hold %s", held, took.Hold.ID)
	}
	var hold holdJSON
	_ = json.Unmarshal(held, &hold)
	if hold.ClientLabel != "laptop" || hold.Mode != "drive" || hold.SessionID != h.sessionID(h.tokenA) {
		t.Fatalf("held task's hold = %+v, want laptop's drive hold of session A", hold)
	}

	if status, rel := h.post("t1/release", h.tokenA, map[string]string{"hold_id": took.Hold.ID}); status != http.StatusOK {
		t.Fatalf("release: %d %+v", status, rel)
	}
	if got := h.conversationTaskHold("conv-t1", "t1"); string(got) != "null" {
		t.Fatalf("hold after release = %s, want null", got)
	}
}

// Attach-info carries the task's hold too, and so does the attach-info
// a takeover answers with: the hold just taken, not the state before it.
func TestAttachInfo_CarriesTheHold(t *testing.T) {
	h := newHoldServer(t)
	h.task("t1", registry.TaskStatusAwaitingInput, "")

	attachHold := func() json.RawMessage {
		t.Helper()
		hold, ok := rawFields(t, []byte(h.get("tasks/t1/attach-info", h.tokenB)))["hold"]
		if !ok {
			t.Fatal("attach-info has no hold field")
		}
		return hold
	}
	if got := attachHold(); string(got) != "null" {
		t.Fatalf("attach-info hold before takeover = %s, want null", got)
	}

	status, raw := doJSON(t, http.MethodPost, h.base+"/api/v1/tasks/t1/takeover", h.tokenA, map[string]any{})
	if status != http.StatusOK {
		t.Fatalf("takeover: %d %s", status, raw)
	}
	var took struct {
		Hold   holdJSON                   `json:"hold"`
		Attach map[string]json.RawMessage `json:"attach"`
	}
	if err := json.Unmarshal([]byte(raw), &took); err != nil {
		t.Fatal(err)
	}
	if got := holdIDOf(t, took.Attach["hold"]); got != took.Hold.ID {
		t.Fatalf("takeover's attach.hold = %s, want the hold taken (%s)", took.Attach["hold"], took.Hold.ID)
	}
	if got := holdIDOf(t, attachHold()); got != took.Hold.ID {
		t.Fatalf("attach-info hold = %s, want %s", got, took.Hold.ID)
	}
}

type conversationRow struct {
	ConversationID string `json:"conversation_id"`
	Status         string `json:"status"`
	HeldBy         *struct {
		ClientKind  string    `json:"client_kind"`
		ClientLabel string    `json:"client_label"`
		Since       time.Time `json:"since"`
	} `json:"held_by"`
	PendingConfirmationID string `json:"pending_confirmation_id"`
}

func (h *holdServer) rows() map[string]conversationRow {
	h.t.Helper()
	var out struct {
		Conversations []conversationRow `json:"conversations"`
	}
	if err := json.Unmarshal([]byte(h.get("conversations", h.tokenA)), &out); err != nil {
		h.t.Fatal(err)
	}
	rows := make(map[string]conversationRow, len(out.Conversations))
	for _, c := range out.Conversations {
		rows[c.ConversationID] = c
	}
	return rows
}

// A list row names who holds its conversation's task, and since when;
// a row whose task isn't held has no held_by.
func TestListConversations_HeldBy(t *testing.T) {
	h := newHoldServer(t)
	h.task("t1", registry.TaskStatusAwaitingInput, "")
	h.task("t2", registry.TaskStatusAwaitingInput, "")

	status, took := h.post("t1/takeover", h.tokenA, map[string]any{"client_label": "laptop"})
	if status != http.StatusOK {
		t.Fatalf("takeover: %d %+v", status, took)
	}
	rows := h.rows()
	held := rows["conv-t1"]
	if held.HeldBy == nil || held.HeldBy.ClientLabel != "laptop" || !held.HeldBy.Since.Equal(took.Hold.AcquiredAt) {
		t.Fatalf("held row = %+v, want held_by laptop since %v", held, took.Hold.AcquiredAt)
	}
	if held.HeldBy.ClientKind != took.Hold.ClientKind {
		t.Fatalf("held_by.client_kind = %q, want the hold's %q", held.HeldBy.ClientKind, took.Hold.ClientKind)
	}
	if rows["conv-t2"].HeldBy != nil {
		t.Fatalf("unheld row = %+v, want no held_by", rows["conv-t2"])
	}

	if status, _ := h.post("t1/release", h.tokenB, nil); status != http.StatusOK {
		t.Fatalf("take back: %d", status)
	}
	if got := h.rows()["conv-t1"]; got.HeldBy != nil {
		t.Fatalf("row after take back = %+v, want no held_by", got)
	}
}

// A list row carries the id of the offer its conversation is waiting on
// a yes for: the newest one still pending and not past its deadline.
func TestListConversations_PendingConfirmation(t *testing.T) {
	h := newHoldServer(t)
	ctx := context.Background()
	now := time.Now().UTC()
	h.task("t-pending", registry.TaskStatusCompleted, "")
	h.task("t-expired", registry.TaskStatusCompleted, "")
	h.task("t-answered", registry.TaskStatusCompleted, "")
	h.task("t-none", registry.TaskStatusCompleted, "")

	offer := func(id, conv string, created, expires time.Time) {
		t.Helper()
		if err := h.store.CreateConfirmation(ctx, &registry.Confirmation{ID: id, ConversationID: conv, Kind: "run_command",
			Command: "ls", Status: registry.ConfirmationPending, CreatedAt: created, ExpiresAt: expires}); err != nil {
			t.Fatalf("CreateConfirmation: %v", err)
		}
	}
	offer("c-old", "conv-t-pending", now.Add(-2*time.Minute), now.Add(time.Hour))
	offer("c-new", "conv-t-pending", now.Add(-time.Minute), now.Add(time.Hour))
	offer("c-expired", "conv-t-expired", now.Add(-time.Hour), now.Add(-time.Minute))
	offer("c-answered", "conv-t-answered", now.Add(-time.Minute), now.Add(time.Hour))
	if err := h.store.ResolveConfirmation(ctx, "c-answered", registry.ConfirmationApproved, ""); err != nil {
		t.Fatalf("ResolveConfirmation: %v", err)
	}

	rows := h.rows()
	for conv, want := range map[string]string{
		"conv-t-pending":  "c-new",
		"conv-t-expired":  "",
		"conv-t-answered": "",
		"conv-t-none":     "",
	} {
		if got := rows[conv].PendingConfirmationID; got != want {
			t.Errorf("%s: pending_confirmation_id = %q, want %q", conv, got, want)
		}
	}
}

// taskHoldEvent is the data of a stream's `event: task_hold`.
type taskHoldEvent struct {
	TaskID      string    `json:"task_id"`
	Hold        *holdJSON `json:"hold"`
	EndedReason string    `json:"ended_reason"`
}

// nextTaskHold reads r's events up to the next task_hold.
func nextTaskHold(t *testing.T, r *bufio.Reader) taskHoldEvent {
	t.Helper()
	for {
		ev := readSSEEventWithTimeout(t, r, 3*time.Second)
		if ev.Event != "task_hold" {
			continue
		}
		var out taskHoldEvent
		if err := json.Unmarshal([]byte(ev.Data), &out); err != nil {
			t.Fatalf("task_hold %s: %v", ev.Data, err)
		}
		return out
	}
}

// The stream reports each acquire, steal and end of a hold on the
// conversation's tasks, and not a renewal: take (hold, no reason),
// heartbeat (nothing), forced takeover by another session (the new hold,
// taken_over), release (null, released).
func TestStream_TaskHoldEvents(t *testing.T) {
	const poll = 20 * time.Millisecond
	h := newHoldServer(t, api.WithStreamPollInterval(poll))
	h.task("t1", registry.TaskStatusAwaitingInput, "")
	r := openStream(t, h.base, h.tokenA, "conv-t1")

	_, took := h.post("t1/takeover", h.tokenA, map[string]any{"client_label": "laptop"})
	ev := nextTaskHold(t, r)
	if ev.TaskID != "t1" || ev.Hold == nil || ev.Hold.ID != took.Hold.ID || ev.EndedReason != "" {
		t.Fatalf("acquire event = %+v, want t1's hold %s and no ended_reason", ev, took.Hold.ID)
	}

	if status, _ := h.post("t1/takeover/heartbeat", h.tokenA, map[string]string{"hold_id": took.Hold.ID}); status != http.StatusOK {
		t.Fatalf("heartbeat: %d", status)
	}
	// Long enough for a renewal to have been reported, had it been.
	time.Sleep(10 * poll)

	status, stole := h.post("t1/takeover", h.tokenB, map[string]any{"client_label": "desk", "force": true})
	if status != http.StatusOK {
		t.Fatalf("forced takeover: %d %+v", status, stole)
	}
	ev = nextTaskHold(t, r)
	if ev.Hold == nil || ev.Hold.ID != stole.Hold.ID || ev.EndedReason != "taken_over" {
		t.Fatalf("event after heartbeat and steal = %+v, want the steal: hold %s, taken_over", ev, stole.Hold.ID)
	}

	if status, _ := h.post("t1/release", h.tokenB, map[string]string{"hold_id": stole.Hold.ID}); status != http.StatusOK {
		t.Fatalf("release: %d", status)
	}
	ev = nextTaskHold(t, r)
	if ev.TaskID != "t1" || ev.Hold != nil || ev.EndedReason != "released" {
		t.Fatalf("release event = %+v, want t1, hold null, released", ev)
	}
}

// A hold there when the stream connects is the past, as for the other
// events: only changes after connect are reported.
func TestStream_TaskHoldBaseline(t *testing.T) {
	const poll = 20 * time.Millisecond
	h := newHoldServer(t, api.WithStreamPollInterval(poll))
	h.task("t1", registry.TaskStatusAwaitingInput, "")
	if status, _ := h.post("t1/takeover", h.tokenA, nil); status != http.StatusOK {
		t.Fatalf("takeover: %d", status)
	}
	r := openStream(t, h.base, h.tokenA, "conv-t1")
	time.Sleep(10 * poll)
	if status, _ := h.post("t1/release", h.tokenB, nil); status != http.StatusOK {
		t.Fatalf("take back: %d", status)
	}
	ev := nextTaskHold(t, r)
	if ev.Hold != nil || ev.EndedReason != "taken_back" {
		t.Fatalf("first task_hold = %+v, want the take back (null, taken_back), not the hold there at connect", ev)
	}
}
