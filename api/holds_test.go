package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Loomux/server/api"
	"github.com/Loomux/server/orchestrator"
	"github.com/Loomux/server/registry"
)

// LOOM-264: POST /api/v1/tasks/{id}/takeover, /takeover/heartbeat and
// /release (docs/design/tui-client-design.md, "API (additions within
// v1)").

type holdJSON struct {
	ID          string     `json:"id"`
	TaskID      string     `json:"task_id"`
	SessionID   string     `json:"session_id"`
	ClientKind  string     `json:"client_kind"`
	ClientLabel string     `json:"client_label"`
	Mode        string     `json:"mode"`
	PrevStatus  string     `json:"prev_status"`
	TTLSeconds  int        `json:"ttl_seconds"`
	AcquiredAt  time.Time  `json:"acquired_at"`
	RenewedAt   time.Time  `json:"renewed_at"`
	ExpiresAt   time.Time  `json:"expires_at"`
	EndedAt     *time.Time `json:"ended_at"`
	EndedReason string     `json:"ended_reason"`
}

// holdAnswer is every body the hold endpoints answer with, decoded
// loosely: the fields a case doesn't have stay zero.
type holdAnswer struct {
	TaskID string    `json:"task_id"`
	Status string    `json:"status"`
	Hold   *holdJSON `json:"hold"`
	Attach *struct {
		TaskID         string `json:"task_id"`
		TmuxSession    string `json:"tmux_session"`
		TmuxSocket     string `json:"tmux_socket"`
		AttachCommand  string `json:"attach_command"`
		AttachCommands []struct {
			Via     string `json:"via"`
			Command string `json:"command"`
		} `json:"attach_commands"`
		Target struct {
			Name string `json:"name"`
		} `json:"target"`
	} `json:"attach"`
	Error       string `json:"error"`
	Code        string `json:"code"`
	EndedReason string `json:"ended_reason"`
}

// holdServer is a test server with the hold endpoints on a real
// orchestrator over its store, one remote target and workspace, and two
// logged-in sessions.
type holdServer struct {
	t      *testing.T
	base   string
	store  registry.Store
	ws     *registry.Workspace
	tokenA string
	tokenB string
}

func newHoldServer(t *testing.T) *holdServer {
	t.Helper()
	srv, _, store := newTestServerWith(t, func(s registry.Store) []api.Option {
		return []api.Option{api.WithTaskHolds(orchestrator.New(s, nil, nil)), api.WithTmuxSocket("loomux-test")}
	})
	ctx := context.Background()
	target := &registry.Target{ID: "remote-1", Name: "jet01", Kind: registry.TargetKindRemote, Host: "jet01.example.com", User: "orski"}
	if err := store.CreateTarget(ctx, target); err != nil {
		t.Fatalf("CreateTarget: %v", err)
	}
	ws := &registry.Workspace{ID: "ws-1", Name: "ws-1", Path: "/srv/app", TargetID: target.ID, Status: registry.WorkspaceStatusActive}
	if err := store.CreateWorkspace(ctx, ws); err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	h := &holdServer{t: t, base: srv.URL, store: store, ws: ws}
	h.tokenA, _ = login(t, srv.URL, testPassword)
	h.tokenB, _ = login(t, srv.URL, testPassword)
	return h
}

// task creates a task in status; class is its error class when failed.
func (h *holdServer) task(id string, status registry.TaskStatus, class registry.ErrorClass) *registry.Task {
	h.t.Helper()
	task := &registry.Task{ID: id, WorkspaceID: h.ws.ID, Kind: registry.TaskKindAgent, AgentType: "claude-code",
		TmuxSession: "loomux-" + id, Status: status, ConversationID: "conv-" + id, ErrorClass: class}
	if status == registry.TaskStatusNeedsAttention {
		task.Attention = &registry.Attention{Kind: registry.AttentionPermission, Title: "Bash command"}
	}
	if err := h.store.CreateTask(context.Background(), task); err != nil {
		h.t.Fatalf("CreateTask: %v", err)
	}
	return task
}

// post sends body (nil for none) and decodes the answer.
func (h *holdServer) post(path, token string, body any) (int, holdAnswer) {
	h.t.Helper()
	status, raw := doJSON(h.t, http.MethodPost, h.base+"/api/v1/tasks/"+path, token, body)
	var out holdAnswer
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		h.t.Fatalf("POST %s: status %d, body %q: %v", path, status, raw, err)
	}
	return status, out
}

func (h *holdServer) status(id string) registry.TaskStatus {
	h.t.Helper()
	task, err := h.store.GetTask(context.Background(), id)
	if err != nil {
		h.t.Fatalf("GetTask: %v", err)
	}
	return task.Status
}

// sessionID is the id of the session token authenticates as.
func (h *holdServer) sessionID(token string) string {
	h.t.Helper()
	status, raw := doJSON(h.t, http.MethodGet, h.base+"/api/v1/sessions", token, nil)
	var out struct {
		Sessions []struct {
			ID      string `json:"id"`
			Current bool   `json:"current"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil || status != http.StatusOK {
		h.t.Fatalf("GET /sessions: %d %q: %v", status, raw, err)
	}
	for _, s := range out.Sessions {
		if s.Current {
			return s.ID
		}
	}
	h.t.Fatalf("no current session in %q", raw)
	return ""
}

// The whole handshake: take, heartbeat, release.
func TestTakeover_HeartbeatRelease(t *testing.T) {
	h := newHoldServer(t)
	h.task("t1", registry.TaskStatusNeedsAttention, "")
	before := time.Now().Add(-time.Second)

	status, out := h.post("t1/takeover", h.tokenA, map[string]any{"client_label": "  laptop  "})
	if status != http.StatusOK {
		t.Fatalf("takeover = %d %+v", status, out)
	}
	hold := out.Hold
	if out.TaskID != "t1" || hold == nil || hold.ID == "" || hold.TaskID != "t1" || hold.SessionID != h.sessionID(h.tokenA) ||
		hold.ClientLabel != "laptop" || hold.Mode != "drive" || hold.PrevStatus != "needs_attention" || hold.TTLSeconds != 45 ||
		hold.EndedAt != nil || hold.EndedReason != "" {
		t.Errorf("takeover hold = %+v", hold)
	}
	if hold.AcquiredAt.Before(before) || !hold.RenewedAt.Equal(hold.AcquiredAt) || !hold.ExpiresAt.Equal(hold.AcquiredAt.Add(45*time.Second)) {
		t.Errorf("hold times = acquired %v renewed %v expires %v", hold.AcquiredAt, hold.RenewedAt, hold.ExpiresAt)
	}
	// One round trip gets the attach-info too.
	if out.Attach == nil || out.Attach.TaskID != "t1" || out.Attach.TmuxSession != "loomux-t1" || out.Attach.TmuxSocket != "loomux-test" ||
		!strings.Contains(out.Attach.AttachCommand, "loomux-t1") || out.Attach.Target.Name != "jet01" ||
		len(out.Attach.AttachCommands) != 1 || out.Attach.AttachCommands[0].Via != "ssh" {
		t.Errorf("takeover attach = %+v", out.Attach)
	}
	if got := h.status("t1"); got != registry.TaskStatusHumanTakeover {
		t.Fatalf("task status = %s, want human-takeover", got)
	}

	time.Sleep(10 * time.Millisecond)
	status, out = h.post("t1/takeover/heartbeat", h.tokenA, map[string]string{"hold_id": hold.ID})
	if status != http.StatusOK || out.Hold == nil || out.Hold.ID != hold.ID {
		t.Fatalf("heartbeat = %d %+v", status, out)
	}
	if !out.Hold.RenewedAt.After(hold.RenewedAt) || !out.Hold.ExpiresAt.Equal(out.Hold.RenewedAt.Add(45*time.Second)) || !out.Hold.AcquiredAt.Equal(hold.AcquiredAt) {
		t.Errorf("after heartbeat = renewed %v expires %v (was %v, %v)", out.Hold.RenewedAt, out.Hold.ExpiresAt, hold.RenewedAt, hold.ExpiresAt)
	}

	status, out = h.post("t1/release", h.tokenA, map[string]string{"hold_id": hold.ID})
	if status != http.StatusOK || out.TaskID != "t1" || out.Status != "awaiting_input" || out.Hold == nil ||
		out.Hold.ID != hold.ID || out.Hold.EndedReason != "released" || out.Hold.EndedAt == nil {
		t.Fatalf("release = %d %+v (hold %+v)", status, out, out.Hold)
	}
	task, _ := h.store.GetTask(context.Background(), "t1")
	if task.Status != registry.TaskStatusAwaitingInput || task.Attention != nil {
		t.Errorf("task after release = %s, attention %+v; want awaiting-input with the stale prompt cleared", task.Status, task.Attention)
	}

	// Releasing again answers 200 with the hold as it ended.
	status, again := h.post("t1/release", h.tokenA, map[string]string{"hold_id": hold.ID})
	if status != http.StatusOK || again.Hold == nil || again.Hold.EndedReason != "released" || !again.Hold.EndedAt.Equal(*out.Hold.EndedAt) || again.Status != "awaiting_input" {
		t.Errorf("second release = %d %+v (hold %+v)", status, again, again.Hold)
	}
	// And its heartbeat is gone, saying why.
	status, gone := h.post("t1/takeover/heartbeat", h.tokenA, map[string]string{"hold_id": hold.ID})
	if status != http.StatusGone || gone.Code != "hold_ended" || gone.EndedReason != "released" || gone.Error == "" ||
		gone.Hold == nil || gone.Hold.ID != hold.ID {
		t.Errorf("heartbeat of a released hold = %d %+v", status, gone)
	}
}

// The hold is checked before the status: the same session gets its hold
// back (a restarted client, with the task already human_takeover), and
// another session gets 409 task_held with the hold.
func TestTakeover_HoldCheckedBeforeStatus(t *testing.T) {
	h := newHoldServer(t)
	h.task("t1", registry.TaskStatusRunning, "")

	// No body at all is a drive takeover with the defaults.
	status, first := h.post("t1/takeover", h.tokenA, nil)
	if status != http.StatusOK || first.Hold.Mode != "drive" || first.Hold.PrevStatus != "running" || first.Hold.ClientLabel != "" {
		t.Fatalf("takeover with no body = %d %+v", status, first.Hold)
	}
	time.Sleep(10 * time.Millisecond)
	status, again := h.post("t1/takeover", h.tokenA, map[string]any{"ttl_seconds": 300, "client_label": "other"})
	if status != http.StatusOK || again.Hold.ID != first.Hold.ID || !again.Hold.RenewedAt.After(first.Hold.RenewedAt) ||
		again.Hold.TTLSeconds != 45 || again.Hold.PrevStatus != "running" || again.Attach == nil {
		t.Errorf("same session again = %d %+v; want its first hold renewed", status, again.Hold)
	}

	status, refused := h.post("t1/takeover", h.tokenB, map[string]any{"client_label": "phone"})
	if status != http.StatusConflict || refused.Code != "task_held" || refused.Error == "" || refused.Hold == nil ||
		refused.Hold.ID != first.Hold.ID || refused.Hold.SessionID != h.sessionID(h.tokenA) || refused.Attach != nil {
		t.Fatalf("another session = %d %+v (hold %+v); want 409 task_held with the holder's hold", status, refused, refused.Hold)
	}
	if got := h.status("t1"); got != registry.TaskStatusHumanTakeover {
		t.Errorf("status = %s", got)
	}

	// force takes it: the old hold ends as taken_over, the task stays
	// human_takeover.
	status, forced := h.post("t1/takeover", h.tokenB, map[string]any{"client_label": "phone", "force": true, "ttl_seconds": 15})
	if status != http.StatusOK || forced.Hold.ID == first.Hold.ID || forced.Hold.SessionID != h.sessionID(h.tokenB) ||
		forced.Hold.ClientLabel != "phone" || forced.Hold.TTLSeconds != 15 || forced.Hold.PrevStatus != "human_takeover" {
		t.Fatalf("forced takeover = %d %+v", status, forced.Hold)
	}
	if got := h.status("t1"); got != registry.TaskStatusHumanTakeover {
		t.Errorf("status after a forced takeover = %s", got)
	}
	status, gone := h.post("t1/takeover/heartbeat", h.tokenA, map[string]string{"hold_id": first.Hold.ID})
	if status != http.StatusGone || gone.Code != "hold_ended" || gone.EndedReason != "taken_over" {
		t.Errorf("the loser's heartbeat = %d %+v; want 410 hold_ended taken_over", status, gone)
	}
	// The loser releasing its ended hold doesn't hand the task back from
	// under the new holder.
	status, rel := h.post("t1/release", h.tokenA, map[string]string{"hold_id": first.Hold.ID})
	if status != http.StatusOK || rel.Status != "human_takeover" || rel.Hold.EndedReason != "taken_over" {
		t.Errorf("the loser's release = %d %+v", status, rel)
	}
	if got := h.status("t1"); got != registry.TaskStatusHumanTakeover {
		t.Errorf("status after the loser's release = %s; want human-takeover", got)
	}
}

// Release without hold_id is Take back, from any session.
func TestRelease_TakeBack(t *testing.T) {
	h := newHoldServer(t)
	h.task("t1", registry.TaskStatusAwaitingInput, "")
	_, taken := h.post("t1/takeover", h.tokenA, nil)

	status, out := h.post("t1/release", h.tokenB, nil)
	if status != http.StatusOK || out.Status != "awaiting_input" || out.Hold == nil || out.Hold.ID != taken.Hold.ID || out.Hold.EndedReason != "taken_back" {
		t.Fatalf("take back = %d %+v (hold %+v)", status, out, out.Hold)
	}
	if got := h.status("t1"); got != registry.TaskStatusAwaitingInput {
		t.Errorf("status = %s", got)
	}
	status, gone := h.post("t1/takeover/heartbeat", h.tokenA, map[string]string{"hold_id": taken.Hold.ID})
	if status != http.StatusGone || gone.EndedReason != "taken_back" {
		t.Errorf("heartbeat after a take back = %d %+v", status, gone)
	}
	// Nothing held: 200, no hold, nothing changed. An empty object is the
	// same as no body.
	status, out = h.post("t1/release", h.tokenB, map[string]string{})
	if status != http.StatusOK || out.Hold != nil || out.Status != "awaiting_input" {
		t.Errorf("take back with nothing held = %d %+v", status, out)
	}
	// A task in human_takeover with no hold (from before holds existed)
	// is handed back too.
	h.task("legacy", registry.TaskStatusHumanTakeover, "")
	status, out = h.post("legacy/release", h.tokenA, nil)
	if status != http.StatusOK || out.Status != "awaiting_input" || out.Hold != nil {
		t.Errorf("take back of an unheld human_takeover task = %d %+v", status, out)
	}
	// …or taken where it stands.
	h.task("legacy2", registry.TaskStatusHumanTakeover, "")
	status, out = h.post("legacy2/takeover", h.tokenA, nil)
	if status != http.StatusOK || out.Hold.PrevStatus != "human_takeover" {
		t.Errorf("takeover of an unheld human_takeover task = %d %+v", status, out.Hold)
	}
}

// A failed task whose pane was kept for a person can be held and stays
// failed; any other ended task is 409 task_inactive.
func TestTakeover_EndedTasks(t *testing.T) {
	h := newHoldServer(t)
	for id, class := range map[string]registry.ErrorClass{
		"login": registry.ErrorClassLoginRequired, "limit": registry.ErrorClassAgentRateLimited, "apierr": registry.ErrorClassAgentAPIError,
	} {
		h.task(id, registry.TaskStatusFailed, class)
		status, out := h.post(id+"/takeover", h.tokenA, nil)
		if status != http.StatusOK || out.Hold.PrevStatus != "failed" || out.Attach == nil {
			t.Errorf("%s: takeover of a failed task = %d %+v", class, status, out)
			continue
		}
		if got := h.status(id); got != registry.TaskStatusFailed {
			t.Errorf("%s: status while held = %s, want failed", class, got)
		}
		if status, refused := h.post(id+"/takeover", h.tokenB, nil); status != http.StatusConflict || refused.Code != "task_held" {
			t.Errorf("%s: another session = %d %+v; want 409 task_held (who is in the pane)", class, status, refused)
		}
		status, rel := h.post(id+"/release", h.tokenA, map[string]string{"hold_id": out.Hold.ID})
		if status != http.StatusOK || rel.Status != "failed" || rel.Hold.EndedReason != "released" {
			t.Errorf("%s: release = %d %+v; want the hold ended and the task still failed", class, status, rel)
		}
	}
	h.task("timeout", registry.TaskStatusFailed, registry.ErrorClassTimeout)
	h.task("done", registry.TaskStatusCompleted, "")
	for _, id := range []string{"timeout", "done"} {
		status, out := h.post(id+"/takeover", h.tokenA, nil)
		if status != http.StatusConflict || out.Code != "task_inactive" || out.Error == "" || out.Hold != nil {
			t.Errorf("%s: takeover = %d %+v; want 409 task_inactive", id, status, out)
		}
		if holds, _ := h.store.ListActiveTaskHolds(context.Background()); len(holds) != 0 {
			t.Errorf("%s: a refused takeover left a hold", id)
		}
	}
}

// mode: drive is accepted, watch is 400 unsupported_mode (never a drive
// hold), anything else is a plain 400. So are a bad ttl and label.
func TestTakeover_Validation(t *testing.T) {
	h := newHoldServer(t)
	h.task("t1", registry.TaskStatusAwaitingInput, "")
	for name, tc := range map[string]struct {
		body any
		code string
	}{
		"watch":            {map[string]any{"mode": "watch"}, "unsupported_mode"},
		"watch with force": {map[string]any{"mode": "watch", "force": true}, "unsupported_mode"},
		"unknown mode":     {map[string]any{"mode": "steer"}, "invalid_request"},
		"mode case":        {map[string]any{"mode": "Drive"}, "invalid_request"},
		"ttl too short":    {map[string]any{"ttl_seconds": 14}, "invalid_request"},
		"ttl too long":     {map[string]any{"ttl_seconds": 301}, "invalid_request"},
		"ttl zero":         {map[string]any{"ttl_seconds": 0}, "invalid_request"},
		"ttl negative":     {map[string]any{"ttl_seconds": -45}, "invalid_request"},
		"ttl overflow":     {map[string]any{"ttl_seconds": 9223372036854775807}, "invalid_request"},
		"label too long":   {map[string]any{"client_label": strings.Repeat("x", 65)}, "invalid_request"},
		"label control":    {map[string]any{"client_label": "lap\x1b[31mtop"}, "invalid_request"},
		"label newline":    {map[string]any{"client_label": "lap\ntop"}, "invalid_request"},
		"wrong type":       {map[string]any{"force": "yes"}, "invalid_request"},
	} {
		status, out := h.post("t1/takeover", h.tokenA, tc.body)
		if status != http.StatusBadRequest || out.Code != tc.code || out.Error == "" || out.Hold != nil {
			t.Errorf("%s: takeover = %d %+v; want 400 %s", name, status, out, tc.code)
		}
	}
	if holds, _ := h.store.ListActiveTaskHolds(context.Background()); len(holds) != 0 {
		t.Errorf("refused takeovers left %d holds", len(holds))
	}
	if got := h.status("t1"); got != registry.TaskStatusAwaitingInput {
		t.Errorf("status after refused takeovers = %s", got)
	}
	// The bounds and an explicit drive are accepted; a 64-character
	// label of multi-byte characters is 64 characters.
	status, out := h.post("t1/takeover", h.tokenA, map[string]any{"mode": "drive", "ttl_seconds": 300, "client_label": strings.Repeat("é", 64)})
	if status != http.StatusOK || out.Hold.TTLSeconds != 300 || out.Hold.ClientLabel != strings.Repeat("é", 64) {
		t.Errorf("takeover at the bounds = %d %+v", status, out.Hold)
	}
}

func TestHolds_NotFoundAndOwnership(t *testing.T) {
	h := newHoldServer(t)
	h.task("t1", registry.TaskStatusAwaitingInput, "")
	h.task("t2", registry.TaskStatusAwaitingInput, "")
	_, taken := h.post("t1/takeover", h.tokenA, nil)

	for name, tc := range map[string]struct {
		path, token string
		body        any
		status      int
		code        string
	}{
		"takeover of an unknown task":       {"nope/takeover", h.tokenA, nil, http.StatusNotFound, "not_found"},
		"heartbeat on an unknown task":      {"nope/takeover/heartbeat", h.tokenA, map[string]string{"hold_id": taken.Hold.ID}, http.StatusNotFound, "not_found"},
		"heartbeat of an unknown hold":      {"t1/takeover/heartbeat", h.tokenA, map[string]string{"hold_id": "nope"}, http.StatusNotFound, "not_found"},
		"heartbeat through another task":    {"t2/takeover/heartbeat", h.tokenA, map[string]string{"hold_id": taken.Hold.ID}, http.StatusNotFound, "not_found"},
		"heartbeat without hold_id":         {"t1/takeover/heartbeat", h.tokenA, map[string]string{}, http.StatusBadRequest, "invalid_request"},
		"heartbeat by another session":      {"t1/takeover/heartbeat", h.tokenB, map[string]string{"hold_id": taken.Hold.ID}, http.StatusForbidden, "forbidden"},
		"release on an unknown task":        {"nope/release", h.tokenA, nil, http.StatusNotFound, "not_found"},
		"release of an unknown hold":        {"t1/release", h.tokenA, map[string]string{"hold_id": "nope"}, http.StatusNotFound, "not_found"},
		"release of a hold on another task": {"t2/release", h.tokenA, map[string]string{"hold_id": taken.Hold.ID}, http.StatusNotFound, "not_found"},
		"takeover without a token":          {"t1/takeover", "", nil, http.StatusUnauthorized, "unauthorized"},
		"heartbeat without a token":         {"t1/takeover/heartbeat", "", map[string]string{"hold_id": taken.Hold.ID}, http.StatusUnauthorized, "unauthorized"},
		"release without a token":           {"t1/release", "", nil, http.StatusUnauthorized, "unauthorized"},
	} {
		status, out := h.post(tc.path, tc.token, tc.body)
		if status != tc.status || out.Code != tc.code || out.Error == "" {
			t.Errorf("%s = %d %+v; want %d %s", name, status, out, tc.status, tc.code)
		}
	}
	// None of that touched the hold.
	if hold, err := h.store.GetActiveDriveHold(context.Background(), "t1"); err != nil || hold.ID != taken.Hold.ID {
		t.Errorf("hold after the refused calls = %+v, %v", hold, err)
	}
	// A malformed body is a 400 on each.
	for _, path := range []string{"t1/takeover", "t1/takeover/heartbeat", "t1/release"} {
		resp := authedRequest(t, http.MethodPost, h.base+"/api/v1/tasks/"+path, h.tokenA, []byte(`{"hold_id":`))
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s with a malformed body = %d, want 400", path, resp.StatusCode)
		}
	}
	if got := h.status("t1"); got != registry.TaskStatusHumanTakeover {
		t.Errorf("a malformed release changed the task: %s", got)
	}
}

// Without a TaskHolds the endpoints answer 501, like the other optional
// ones.
func TestHolds_NotConfigured(t *testing.T) {
	srv, _, _ := newTestServer(t)
	token, _ := login(t, srv.URL, testPassword)
	for _, path := range []string{"t1/takeover", "t1/takeover/heartbeat", "t1/release"} {
		status, body := doJSON(t, http.MethodPost, srv.URL+"/api/v1/tasks/"+path, token, map[string]string{"hold_id": "h"})
		if status != http.StatusNotImplemented || !strings.Contains(body, `"code":"not_implemented"`) {
			t.Errorf("%s = %d %s; want 501 not_implemented", path, status, body)
		}
	}
}
