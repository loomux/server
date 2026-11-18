package api_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Loomux/server/api"
	"github.com/Loomux/server/orchestrator"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/targets"
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
	// exec is every target's executor; it records what was detached.
	exec *detachRecorder
}

// detachRecorder is the hold server's targets.TargetExecutor: it records
// DetachClients (LOOM-265), fails it while down, and has nothing else to
// do (any other call panics on the nil embedded interface).
type detachRecorder struct {
	targets.TargetExecutor
	mu       sync.Mutex
	detached []string
	down     bool
}

func (e *detachRecorder) TmuxSocket() string { return "loomux-test" }

func (e *detachRecorder) DetachClients(_ context.Context, session string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.down {
		return targets.ErrUnreachable
	}
	e.detached = append(e.detached, session)
	return nil
}

func (e *detachRecorder) sessions() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.detached...)
}

func (e *detachRecorder) setDown(down bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.down = down
}

func newHoldServer(t *testing.T) *holdServer {
	t.Helper()
	exec := &detachRecorder{}
	factory := func(*registry.Target) (targets.TargetExecutor, error) { return exec, nil }
	srv, _, store := newTestServerWith(t, func(s registry.Store) []api.Option {
		return []api.Option{api.WithTaskHolds(orchestrator.New(s, factory, nil)), api.WithTmuxSocket("loomux-test")}
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
	h := &holdServer{t: t, base: srv.URL, store: store, ws: ws, exec: exec}
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
		// 2^55 + 45: as a Duration in seconds it wraps to exactly 45s.
		"ttl wraps to 45": {map[string]any{"ttl_seconds": 36028797018964013}, "invalid_request"},
		"label too long":  {map[string]any{"client_label": strings.Repeat("x", 65)}, "invalid_request"},
		"label control":   {map[string]any{"client_label": "lap\x1b[31mtop"}, "invalid_request"},
		"label newline":   {map[string]any{"client_label": "lap\ntop"}, "invalid_request"},
		"wrong type":      {map[string]any{"force": "yes"}, "invalid_request"},
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

// A task that ends while it is held (nothing ends its hold yet): another
// session is told it has ended, with or without force, rather than sent
// from "held, use force" to "ended".
func TestTakeover_HeldTaskEnded(t *testing.T) {
	h := newHoldServer(t)
	h.task("t1", registry.TaskStatusAwaitingInput, "")
	_, taken := h.post("t1/takeover", h.tokenA, nil)
	task, _ := h.store.GetTask(context.Background(), "t1")
	task.Status = registry.TaskStatusCompleted
	if applied, err := h.store.TransitionTask(context.Background(), task, registry.TaskStatusHumanTakeover); err != nil || !applied {
		t.Fatalf("TransitionTask: %v, %v", applied, err)
	}
	for _, body := range []any{nil, map[string]any{"force": true}} {
		status, out := h.post("t1/takeover", h.tokenB, body)
		if status != http.StatusConflict || out.Code != "task_inactive" {
			t.Errorf("another session, body %v = %d %+v; want 409 task_inactive", body, status, out)
		}
	}
	// The holder can still release; the task stays completed.
	status, rel := h.post("t1/release", h.tokenA, map[string]string{"hold_id": taken.Hold.ID})
	if status != http.StatusOK || rel.Status != "completed" || rel.Hold.EndedReason != "released" {
		t.Errorf("release of a hold on an ended task = %d %+v", status, rel)
	}
}

// "The body may be left out" also holds for a client that sends a
// bodiless POST chunked, or only whitespace.
func TestHolds_EmptyChunkedBody(t *testing.T) {
	h := newHoldServer(t)
	h.task("t1", registry.TaskStatusAwaitingInput, "")
	send := func(path, body string) (int, holdAnswer) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, h.base+"/api/v1/tasks/"+path, struct{ io.Reader }{strings.NewReader(body)})
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+h.tokenA)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out holdAnswer
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	if status, out := send("t1/takeover", ""); status != http.StatusOK || out.Hold == nil {
		t.Fatalf("chunked takeover with no body = %d %+v", status, out)
	}
	if status, out := send("t1/release", " \n"); status != http.StatusOK || out.Status != "awaiting_input" {
		t.Errorf("chunked release with a whitespace body = %d %+v", status, out)
	}
	// A chunked body with content is still read, and still validated.
	if status, out := send("t1/takeover", ` {"ttl_seconds": 20}`); status != http.StatusOK || out.Hold.TTLSeconds != 20 {
		t.Errorf("chunked takeover with a body = %d %+v", status, out)
	}
	if status, out := send("t1/takeover", `{"mode":`); status != http.StatusBadRequest {
		t.Errorf("chunked malformed body = %d %+v; want 400", status, out)
	}
}

// rawChunked sends a chunked POST by hand: one chunk of whitespace, then
// finish decides how the body ends. It returns the answer's status.
func (h *holdServer) rawChunked(path, token string, finish func(net.Conn)) int {
	h.t.Helper()
	u, _ := url.Parse(h.base)
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		h.t.Fatal(err)
	}
	defer conn.Close()
	conn.Write([]byte("POST /api/v1/tasks/" + path + " HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer " + token +
		"\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\n\r\n2\r\n \n\r\n"))
	finish(conn)
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		h.t.Errorf("POST %s: no answer: %v", path, err)
		return 0
	}
	resp.Body.Close()
	return resp.StatusCode
}

// held reports whether the task has an active drive hold.
func (h *holdServer) held(id string) bool {
	h.t.Helper()
	_, err := h.store.GetActiveDriveHold(context.Background(), id)
	if err != nil && !errors.Is(err, registry.ErrNotFound) {
		h.t.Fatalf("GetActiveDriveHold: %v", err)
	}
	return err == nil
}

// A chunked body that can't be read to its end isn't "left out": one
// over the size limit or cut off is refused like any other body, and
// the hold stays as it was. Only a body that ends cleanly with nothing
// but whitespace counts as none.
func TestHolds_UnreadableChunkedBody(t *testing.T) {
	h := newHoldServer(t)
	h.task("t1", registry.TaskStatusAwaitingInput, "")
	h.task("t2", registry.TaskStatusAwaitingInput, "")
	status, taken := h.post("t1/takeover", h.tokenA, nil)
	if status != http.StatusOK || taken.Hold == nil {
		t.Fatalf("takeover = %d %+v", status, taken)
	}

	// Whitespace past the size limit, then a hold to release: the body
	// is too large, not a Take back.
	big := io.MultiReader(strings.NewReader(strings.Repeat(" ", 1<<20+1)), strings.NewReader(`{"hold_id":"other"}`))
	req, err := http.NewRequest(http.MethodPost, h.base+"/api/v1/tasks/t1/release", big)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+h.tokenB)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("release with an oversized whitespace body = %d, want 413", resp.StatusCode)
	}
	if !h.held("t1") {
		t.Fatal("an oversized body took the task back")
	}

	// Cut off before the last chunk: the rest might have named a hold.
	cut := func(c net.Conn) { c.(*net.TCPConn).CloseWrite() }
	if got := h.rawChunked("t1/release", h.tokenB, cut); got != http.StatusBadRequest {
		t.Errorf("release with a body cut off = %d, want 400", got)
	}
	if !h.held("t1") {
		t.Fatal("a body cut off took the task back")
	}
	if got := h.rawChunked("t2/takeover", h.tokenB, cut); got != http.StatusBadRequest {
		t.Errorf("takeover with a body cut off = %d, want 400", got)
	}
	if h.held("t2") || h.status("t2") != registry.TaskStatusAwaitingInput {
		t.Fatalf("a body cut off took the task over (status %s)", h.status("t2"))
	}

	// The same request with the body ended properly is still no body.
	end := func(c net.Conn) { c.Write([]byte("0\r\n\r\n")) }
	if got := h.rawChunked("t2/takeover", h.tokenB, end); got != http.StatusOK || !h.held("t2") {
		t.Errorf("takeover with a whitespace body = %d, held %v; want 200 and a hold", got, h.held("t2"))
	}
}

// A chunked body that stalls in its whitespace is ended by the read
// deadline as an error, not taken for no body.
func TestHolds_StalledChunkedBody(t *testing.T) {
	defer api.SetBodyReadTimeout(300 * time.Millisecond)()
	h := newHoldServer(t)
	h.task("t1", registry.TaskStatusAwaitingInput, "")
	start := time.Now()
	if got := h.rawChunked("t1/takeover", h.tokenA, func(net.Conn) {}); got != http.StatusBadRequest {
		t.Errorf("takeover with a stalled body = %d, want 400", got)
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("a stalled body held the request for %v", time.Since(start))
	}
	if h.held("t1") || h.status("t1") != registry.TaskStatusAwaitingInput {
		t.Fatalf("a stalled body took the task over (status %s)", h.status("t1"))
	}
}

// LOOM-265: Take back detaches the holder's clients before handing the
// task back, and fails closed: if they can't be detached the hold and
// the task's human_takeover stay, and the answer says so.
func TestRelease_TakeBackDetaches(t *testing.T) {
	h := newHoldServer(t)
	h.task("t1", registry.TaskStatusAwaitingInput, "")
	_, taken := h.post("t1/takeover", h.tokenA, nil)

	h.exec.setDown(true)
	status, out := h.post("t1/release", h.tokenB, nil)
	if status != http.StatusBadGateway || out.Code != "detach_failed" || out.Error == "" {
		t.Fatalf("take back with the target down = %d %+v; want 502 detach_failed", status, out)
	}
	if !h.held("t1") || h.status("t1") != registry.TaskStatusHumanTakeover {
		t.Fatalf("after a failed take back: held %v, status %s; want both kept", h.held("t1"), h.status("t1"))
	}

	h.exec.setDown(false)
	status, out = h.post("t1/release", h.tokenB, nil)
	if status != http.StatusOK || out.Hold == nil || out.Hold.ID != taken.Hold.ID || out.Hold.EndedReason != "taken_back" || out.Status != "awaiting_input" {
		t.Fatalf("take back = %d %+v", status, out)
	}
	if d := h.exec.sessions(); len(d) != 1 || d[0] != "loomux-t1" {
		t.Errorf("detached %v, want [loomux-t1]", d)
	}
	// The holder's own release detaches nobody.
	_, again := h.post("t1/takeover", h.tokenA, nil)
	if status, _ := h.post("t1/release", h.tokenA, map[string]string{"hold_id": again.Hold.ID}); status != http.StatusOK {
		t.Fatalf("release = %d", status)
	}
	if d := h.exec.sessions(); len(d) != 1 {
		t.Errorf("a holder's own release detached: %v", d)
	}
}

// LOOM-265: logging out, or revoking a session by id, ends its holds as
// session_revoked: their clients are detached and their tasks handed
// back. Another session's holds stay.
func TestHolds_EndWithTheirSession(t *testing.T) {
	for _, how := range []string{"logout", "revoke"} {
		t.Run(how, func(t *testing.T) {
			h := newHoldServer(t)
			h.task("t1", registry.TaskStatusAwaitingInput, "")
			h.task("t2", registry.TaskStatusNeedsAttention, "")
			h.task("t3", registry.TaskStatusAwaitingInput, "")
			_, a1 := h.post("t1/takeover", h.tokenA, nil)
			_, a2 := h.post("t2/takeover", h.tokenA, nil)
			_, b3 := h.post("t3/takeover", h.tokenB, nil)

			var status int
			if how == "logout" {
				status, _ = doJSON(t, http.MethodPost, h.base+"/api/v1/logout", h.tokenA, nil)
			} else {
				status, _ = doJSON(t, http.MethodDelete, h.base+"/api/v1/sessions/"+a1.Hold.SessionID, h.tokenB, nil)
			}
			if status != http.StatusNoContent {
				t.Fatalf("%s = %d", how, status)
			}
			ctx := context.Background()
			for _, id := range []string{a1.Hold.ID, a2.Hold.ID} {
				hold, err := h.store.GetTaskHold(ctx, id)
				if err != nil || hold.Active() || hold.EndedReason != registry.HoldEndSessionRevoked {
					t.Errorf("hold %s = %+v, %v; want ended as session_revoked", id, hold, err)
				}
			}
			for _, id := range []string{"t1", "t2"} {
				if got := h.status(id); got != registry.TaskStatusAwaitingInput {
					t.Errorf("%s = %s; want awaiting_input", id, got)
				}
			}
			if hold, _ := h.store.GetTaskHold(ctx, b3.Hold.ID); !hold.Active() {
				t.Error("another session's hold ended")
			}
			if d := h.exec.sessions(); len(d) != 2 {
				t.Errorf("detached %v; want t1's and t2's sessions", d)
			}
		})
	}
}

// A revoke whose detach fails still revokes (the token is dead at once);
// the hold is left for the sweeper, which ends it once it can detach.
func TestHolds_RevokeWithTheTargetDown(t *testing.T) {
	h := newHoldServer(t)
	h.task("t1", registry.TaskStatusAwaitingInput, "")
	_, a1 := h.post("t1/takeover", h.tokenA, nil)
	h.exec.setDown(true)
	if status, _ := doJSON(t, http.MethodPost, h.base+"/api/v1/logout", h.tokenA, nil); status != http.StatusNoContent {
		t.Fatalf("logout = %d", status)
	}
	if status, _ := doJSON(t, http.MethodGet, h.base+"/api/v1/sessions", h.tokenA, nil); status != http.StatusUnauthorized {
		t.Errorf("the logged-out token = %d; want 401", status)
	}
	if hold, _ := h.store.GetTaskHold(context.Background(), a1.Hold.ID); !hold.Active() || h.status("t1") != registry.TaskStatusHumanTakeover {
		t.Errorf("hold %+v, status %s; want kept until its clients are detached", hold, h.status("t1"))
	}
}

// LOOM-265 review: a forced takeover detaches the holder first and fails
// closed: 502 detach_failed, and the holder keeps the task.
func TestTakeover_ForceDetachFails(t *testing.T) {
	h := newHoldServer(t)
	h.task("t1", registry.TaskStatusAwaitingInput, "")
	_, taken := h.post("t1/takeover", h.tokenA, nil)
	h.exec.setDown(true)
	status, out := h.post("t1/takeover", h.tokenB, map[string]any{"force": true})
	if status != http.StatusBadGateway || out.Code != "detach_failed" {
		t.Fatalf("forced takeover with the target down = %d %+v; want 502 detach_failed", status, out)
	}
	if hold, _ := h.store.GetTaskHold(context.Background(), taken.Hold.ID); !hold.Active() {
		t.Error("the holder's hold ended")
	}
	h.exec.setDown(false)
	status, out = h.post("t1/takeover", h.tokenB, map[string]any{"force": true})
	if status != http.StatusOK || out.Hold == nil || out.Hold.SessionID == taken.Hold.SessionID {
		t.Fatalf("forced takeover = %d %+v", status, out)
	}
	if d := h.exec.sessions(); len(d) != 1 || d[0] != "loomux-t1" {
		t.Errorf("detached %v", d)
	}
}

// revokeDuringTakeover is a hold service whose takeover is overtaken by
// a logout: the session is deleted once the hold is taken, after the
// logout's own end of the session's holds would have run.
type revokeDuringTakeover struct {
	*orchestrator.Orchestrator
	store registry.Store
}

func (r revokeDuringTakeover) Takeover(ctx context.Context, req orchestrator.TakeoverRequest) (*orchestrator.TakeoverResult, error) {
	res, err := r.Orchestrator.Takeover(ctx, req)
	if derr := r.store.DeleteSession(ctx, req.SessionID); derr != nil {
		panic(derr)
	}
	return res, err
}

// LOOM-265 review: a takeover whose session is revoked while it is taken
// gets no pane: 401, and its hold is ended and its task handed back.
func TestTakeover_SessionRevokedMeanwhile(t *testing.T) {
	exec := &detachRecorder{}
	factory := func(*registry.Target) (targets.TargetExecutor, error) { return exec, nil }
	srv, _, store := newTestServerWith(t, func(s registry.Store) []api.Option {
		return []api.Option{api.WithTaskHolds(revokeDuringTakeover{orchestrator.New(s, factory, nil), s}), api.WithTmuxSocket("loomux-test")}
	})
	ctx := context.Background()
	if err := store.CreateTarget(ctx, &registry.Target{ID: "remote-1", Name: "jet01", Kind: registry.TargetKindRemote, Host: "jet01.example.com", User: "orski"}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateWorkspace(ctx, &registry.Workspace{ID: "ws-1", Name: "ws-1", Path: "/srv/app", TargetID: "remote-1", Status: registry.WorkspaceStatusActive}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateTask(ctx, &registry.Task{ID: "t1", WorkspaceID: "ws-1", Kind: registry.TaskKindAgent, AgentType: "claude-code",
		TmuxSession: "loomux-t1", Status: registry.TaskStatusAwaitingInput, ConversationID: "conv-t1"}); err != nil {
		t.Fatal(err)
	}
	token, _ := login(t, srv.URL, testPassword)
	status, raw := doJSON(t, http.MethodPost, srv.URL+"/api/v1/tasks/t1/takeover", token, nil)
	if status != http.StatusUnauthorized || strings.Contains(raw, "attach") {
		t.Fatalf("takeover by a session revoked meanwhile = %d %s; want 401 and no attach", status, raw)
	}
	if _, err := store.GetActiveDriveHold(ctx, "t1"); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("active hold after the refused takeover: %v", err)
	}
	if task, _ := store.GetTask(ctx, "t1"); task.Status != registry.TaskStatusAwaitingInput {
		t.Errorf("status = %s; want handed back", task.Status)
	}
	if d := exec.sessions(); len(d) != 1 {
		t.Errorf("detached %v; want the session's clients detached", d)
	}
}
