package apitest_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Loomux/server/api/apitest"
	"github.com/Loomux/server/registry"
)

// These tests use the helper the way a client module does: seed, then
// talk HTTP to srv.URL with a token.

// call sends method path with a JSON body (nil for none) and decodes the
// JSON answer into a map.
func call(t *testing.T, srv *apitest.Server, token, method, path string, body any, headers ...string) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, srv.URL+"/api/v1"+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := map[string]any{}
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("%s %s: status %d, body %q: %v", method, path, resp.StatusCode, raw, err)
		}
	}
	return resp.StatusCode, out
}

func obj(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("not an object: %#v", v)
	}
	return m
}

func list(t *testing.T, v any) []any {
	t.Helper()
	l, ok := v.([]any)
	if !ok {
		t.Fatalf("not a list: %#v", v)
	}
	return l
}

func TestLoginAndAuth(t *testing.T) {
	srv := apitest.New(t)
	if status, out := call(t, srv, "", "GET", "/conversations", nil); status != http.StatusUnauthorized || out["code"] != "unauthorized" {
		t.Fatalf("without a token = %d %v", status, out)
	}
	if status, out := call(t, srv, "", "POST", "/login", map[string]string{"password": "wrong"}); status != http.StatusUnauthorized {
		t.Fatalf("login with a wrong password = %d %v", status, out)
	}
	a, b := srv.Login(), srv.Login()
	if a == "" || a == b {
		t.Fatalf("tokens %q, %q; want two distinct sessions", a, b)
	}
	status, out := call(t, srv, a, "GET", "/sessions", nil)
	if status != http.StatusOK || len(list(t, out["sessions"])) != 2 {
		t.Fatalf("GET /sessions = %d %v", status, out)
	}
	if status, _ := call(t, srv, a, "POST", "/logout", nil); status != http.StatusNoContent {
		t.Fatalf("logout = %d", status)
	}
	if status, _ := call(t, srv, a, "GET", "/sessions", nil); status != http.StatusUnauthorized {
		t.Fatalf("after logout = %d, want 401", status)
	}
	if status, out := call(t, srv, "", "GET", "/version", nil); status != http.StatusOK || out["api_version"] != "v1" {
		t.Fatalf("GET /version = %d %v", status, out)
	}

	other := apitest.New(t, apitest.WithPassword("another"))
	if other.Password != "another" || other.Login() == "" {
		t.Fatal("WithPassword not honoured")
	}
}

// Seeded conversations, prompts, offers and failed tasks come back
// through the conversation endpoints as a client sees them.
func TestSeedsShowThroughTheAPI(t *testing.T) {
	srv := apitest.New(t)
	token := srv.Login()

	task := srv.SeedConversation("conv-chat", "fix the build", "done: it was a missing import")
	prompt := srv.SeedPrompt("conv-prompt", registry.Attention{})
	failed := srv.SeedFailedTask("conv-failed", registry.ErrorClassLoginRequired, "the agent asked to be signed in")
	offer := srv.SeedOffer(registry.Confirmation{ConversationID: "conv-chat", TargetName: "jet01", Command: "make test"})
	srv.SeedTurn(task, "fix the build", "done", "$ make\nok")

	status, out := call(t, srv, token, "GET", "/conversations", nil)
	if status != http.StatusOK {
		t.Fatalf("GET /conversations = %d %v", status, out)
	}
	statuses := map[string]string{}
	for _, c := range list(t, out["conversations"]) {
		row := obj(t, c)
		statuses[row["conversation_id"].(string)], _ = row["status"].(string)
	}
	want := map[string]string{"conv-chat": "awaiting_input", "conv-prompt": "needs_attention", "conv-failed": "failed"}
	for id, st := range want {
		if statuses[id] != st {
			t.Errorf("conversation %s status = %q, want %q (all: %v)", id, statuses[id], st, statuses)
		}
	}

	status, out = call(t, srv, token, "GET", "/conversations/conv-chat", nil)
	if status != http.StatusOK {
		t.Fatalf("GET /conversations/conv-chat = %d %v", status, out)
	}
	msgs := list(t, out["messages"])
	if len(msgs) != 2 || obj(t, msgs[0])["role"] != "user" || obj(t, msgs[1])["content"] != "done: it was a missing import" ||
		obj(t, msgs[1])["task_id"] != task.ID {
		t.Errorf("messages = %v", msgs)
	}
	confs := list(t, out["confirmations"])
	if len(confs) != 1 || obj(t, confs[0])["id"] != offer.ID || obj(t, confs[0])["status"] != "pending" ||
		obj(t, confs[0])["command"] != "make test" || obj(t, confs[0])["kind"] != "run_command" {
		t.Errorf("confirmations = %v", confs)
	}

	status, out = call(t, srv, token, "GET", "/conversations/conv-prompt", nil)
	tasks := list(t, out["tasks"])
	if status != http.StatusOK || len(tasks) != 1 || obj(t, tasks[0])["id"] != prompt.ID || obj(t, tasks[0])["status"] != "needs_attention" {
		t.Fatalf("conv-prompt = %d %v", status, out)
	}
	if att := obj(t, obj(t, tasks[0])["attention"]); att["kind"] != "permission" || att["title"] != "Bash command" {
		t.Errorf("attention = %v", att)
	}

	status, out = call(t, srv, token, "GET", "/conversations/conv-failed", nil)
	tasks = list(t, out["tasks"])
	if status != http.StatusOK || len(tasks) != 1 || obj(t, tasks[0])["id"] != failed.ID ||
		obj(t, tasks[0])["error_class"] != "login_required" || obj(t, tasks[0])["failure_reason"] != "the agent asked to be signed in" {
		t.Errorf("conv-failed = %d %v", status, out)
	}

	status, out = call(t, srv, token, "GET", "/tasks/"+task.ID+"/transcript", nil)
	if turns := list(t, out["turns"]); status != http.StatusOK || len(turns) != 1 || obj(t, turns[0])["agent_message"] != "done" {
		t.Errorf("transcript = %d %v", status, out)
	}

	status, out = call(t, srv, token, "GET", "/tasks/"+task.ID+"/attach-info", nil)
	cmds := list(t, out["attach_commands"])
	if status != http.StatusOK || out["tmux_session"] != task.TmuxSession || out["tmux_socket"] != apitest.TmuxSocket ||
		len(cmds) != 1 || obj(t, cmds[0])["via"] != "ssh" || !strings.Contains(obj(t, cmds[0])["command"].(string), task.TmuxSession) {
		t.Errorf("attach-info = %d %v", status, out)
	}
	// The seeded task has a session on the fake target; a completed one
	// doesn't.
	if srv.Executor.Session(task.TmuxSession) == nil {
		t.Error("the seeded task has no session on the executor")
	}
	done := srv.SeedTask(apitest.Task{Status: registry.TaskStatusCompleted})
	if srv.Executor.Session(done.TmuxSession) != nil {
		t.Error("a completed task was given a session")
	}

	status, out = call(t, srv, token, "GET", "/workspaces", nil)
	if status != http.StatusOK || len(list(t, out["workspaces"])) != 1 {
		t.Errorf("GET /workspaces = %d %v", status, out)
	}
}

// A dispatch is a real job answered by the stub: blocking, async with
// polling, idempotent, one per conversation, cancellable.
func TestDispatch(t *testing.T) {
	srv := apitest.New(t)
	token := srv.Login()

	// Blocking, with the default reply.
	status, out := call(t, srv, token, "POST", "/dispatch?wait=true", map[string]string{"conversation_id": "conv-1", "message": "hello"})
	if status != http.StatusOK || out["status"] != "succeeded" || out["reply"] != apitest.DefaultReply || out["conversation_id"] != "conv-1" {
		t.Fatalf("blocking dispatch = %d %v", status, out)
	}
	seen := srv.Dispatcher.Dispatches()
	if len(seen) != 1 || seen[0].Message != "hello" || seen[0].ConversationID != "conv-1" || seen[0].ID != out["dispatch_id"] {
		t.Errorf("Dispatches() = %+v", seen)
	}
	// Both sides of the exchange are in the conversation.
	_, conv := call(t, srv, token, "GET", "/conversations/conv-1", nil)
	if msgs := list(t, conv["messages"]); len(msgs) != 2 || obj(t, msgs[0])["content"] != "hello" || obj(t, msgs[1])["content"] != apitest.DefaultReply {
		t.Errorf("messages after a dispatch = %v", msgs)
	}

	// Async with an idempotency key, then polled; a repeat is the same job.
	srv.Dispatcher.Reply("on it")
	body := map[string]string{"conversation_id": "conv-2", "message": "run the tests"}
	status, out = call(t, srv, token, "POST", "/dispatch", body, "Prefer", "respond-async", "Idempotency-Key", "key-1")
	if status != http.StatusAccepted || out["dispatch_id"] == "" {
		t.Fatalf("async dispatch = %d %v", status, out)
	}
	id := out["dispatch_id"].(string)
	if status, again := call(t, srv, token, "POST", "/dispatch", body, "Prefer", "respond-async", "Idempotency-Key", "key-1"); again["dispatch_id"] != id {
		t.Errorf("repeat with the same key = %d %v; want dispatch %s", status, again, id)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		status, out = call(t, srv, token, "GET", "/dispatches/"+id, nil)
		if status != http.StatusOK {
			t.Fatalf("GET /dispatches/%s = %d %v", id, status, out)
		}
		if out["status"] == "succeeded" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("dispatch still %v", out["status"])
		}
		time.Sleep(10 * time.Millisecond)
	}
	if out["reply"] != "on it" {
		t.Errorf("reply = %v", out["reply"])
	}

	// A failing handler is a failed job.
	srv.Dispatcher.Fail(errors.New("the router model is unavailable"))
	status, out = call(t, srv, token, "POST", "/dispatch?wait=true", map[string]string{"conversation_id": "conv-3", "message": "hi"})
	if status != http.StatusInternalServerError || out["status"] != "failed" || !strings.Contains(out["error"].(string), "unavailable") {
		t.Errorf("failing dispatch = %d %v", status, out)
	}

	// Blocked: the conversation is busy, and the job can be cancelled.
	release := srv.Dispatcher.Block()
	defer release()
	status, out = call(t, srv, token, "POST", "/dispatch", map[string]string{"conversation_id": "conv-4", "message": "long job"}, "Prefer", "respond-async")
	if status != http.StatusAccepted {
		t.Fatalf("blocked dispatch = %d %v", status, out)
	}
	busyID := out["dispatch_id"].(string)
	status, out = call(t, srv, token, "POST", "/dispatch", map[string]string{"conversation_id": "conv-4", "message": "another"}, "Prefer", "respond-async")
	if status != http.StatusConflict || out["code"] != "conversation_busy" || out["dispatch_id"] != busyID {
		t.Errorf("second dispatch into a busy conversation = %d %v", status, out)
	}
	if status, out := call(t, srv, token, "POST", "/dispatches/"+busyID+"/cancel", nil); status != http.StatusAccepted {
		t.Fatalf("cancel = %d %v", status, out)
	}
	deadline = time.Now().Add(5 * time.Second)
	for {
		_, out = call(t, srv, token, "GET", "/dispatches/"+busyID, nil)
		if out["status"] == "failed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cancelled dispatch still %v", out["status"])
		}
		time.Sleep(10 * time.Millisecond)
	}
	if out["error_class"] != "cancelled" {
		t.Errorf("cancelled dispatch error_class = %v", out["error_class"])
	}

	// An offer's answer reaches the handler with its confirmation id.
	offer := srv.SeedOffer(registry.Confirmation{ConversationID: "conv-5"})
	got := make(chan apitest.Dispatch, 1)
	srv.Dispatcher.Handle(func(_ context.Context, d apitest.Dispatch) (string, error) {
		got <- d
		return "approved", nil
	})
	status, out = call(t, srv, token, "POST", "/dispatch?wait=true", map[string]string{"conversation_id": "conv-5", "message": "yes", "confirmation_id": offer.ID})
	if status != http.StatusOK || out["reply"] != "approved" {
		t.Fatalf("answering an offer = %d %v", status, out)
	}
	if d := <-got; d.ConfirmationID != offer.ID || d.Message != "yes" {
		t.Errorf("handler saw %+v", d)
	}
}

// streamEvents opens the conversation's stream and returns a channel of
// "event data" pairs and a func that closes it.
func streamEvents(t *testing.T, srv *apitest.Server, token, conversationID string) (<-chan [2]string, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/v1/conversations/"+conversationID+"/stream", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("open stream: %v", err)
	}
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		cancel()
		t.Fatalf("stream = %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	events := make(chan [2]string, 64)
	go func() {
		defer close(events)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		var name string
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				events <- [2]string{name, strings.TrimPrefix(line, "data: ")}
			}
		}
	}()
	return events, cancel
}

// waitEvent reads events until one named name satisfies ok.
func waitEvent(t *testing.T, events <-chan [2]string, name string, ok func(map[string]any) bool) map[string]any {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case ev, open := <-events:
			if !open {
				t.Fatalf("stream closed before a %s event", name)
			}
			if ev[0] != name {
				continue
			}
			var data map[string]any
			if err := json.Unmarshal([]byte(ev[1]), &data); err != nil {
				t.Fatalf("%s data %q: %v", name, ev[1], err)
			}
			if ok(data) {
				return data
			}
		case <-timeout:
			t.Fatalf("no matching %s event within 5s", name)
		}
	}
}

// The conversation stream reports a dispatch's progress, the reply's
// message and a task's status changes, and ends with the test.
func TestStream(t *testing.T) {
	srv := apitest.New(t)
	token := srv.Login()
	task := srv.SeedTask(apitest.Task{ConversationID: "conv-1"})
	events, closeStream := streamEvents(t, srv, token, "conv-1")
	defer closeStream()

	srv.Dispatcher.Reply("streamed")
	status, out := call(t, srv, token, "POST", "/dispatch", map[string]string{"conversation_id": "conv-1", "message": "go"}, "Prefer", "respond-async")
	if status != http.StatusAccepted {
		t.Fatalf("dispatch = %d %v", status, out)
	}
	done := waitEvent(t, events, "dispatch_update", func(d map[string]any) bool { return d["status"] == "succeeded" })
	if done["dispatch_id"] != out["dispatch_id"] || done["reply"] != "streamed" {
		t.Errorf("dispatch_update = %v", done)
	}

	srv.SetTaskStatus(task.ID, registry.TaskStatusNeedsAttention, &registry.Attention{Kind: registry.AttentionQuestion, Question: "which branch?"})
	update := waitEvent(t, events, "task_update", func(d map[string]any) bool { return d["status"] == "needs_attention" })
	if update["task_id"] != task.ID {
		t.Errorf("task_update = %v", update)
	}

	msg := srv.SeedMessage("conv-1", registry.MessageRoleAssistant, "a late report", task)
	waitEvent(t, events, "message_added", func(d map[string]any) bool { return d["message_id"] == msg.ID })
	// Left open: New's cleanup ends it, or the test would hang here.
}

// Takeover, heartbeat and release (LOOM-264), as loomux attach drives
// them.
func TestTakeoverHeartbeatRelease(t *testing.T) {
	srv := apitest.New(t)
	laptop, phone := srv.Login(), srv.Login()
	task := srv.SeedPrompt("conv-1", registry.Attention{})

	status, out := call(t, srv, laptop, "POST", "/tasks/"+task.ID+"/takeover", map[string]any{"client_label": "laptop"})
	if status != http.StatusOK {
		t.Fatalf("takeover = %d %v", status, out)
	}
	hold := obj(t, out["hold"])
	holdID := hold["id"].(string)
	if hold["mode"] != "drive" || hold["prev_status"] != "needs_attention" || hold["client_label"] != "laptop" || hold["ttl_seconds"] != float64(45) {
		t.Errorf("hold = %v", hold)
	}
	if attach := obj(t, out["attach"]); attach["tmux_session"] != task.TmuxSession || len(list(t, attach["attach_commands"])) != 1 {
		t.Errorf("attach = %v", attach)
	}
	if got := srv.Task(task.ID); got.Status != registry.TaskStatusHumanTakeover {
		t.Fatalf("task = %s, want human-takeover", got.Status)
	}
	if h := srv.ActiveHold(task.ID); h == nil || h.ID != holdID {
		t.Errorf("ActiveHold = %+v", h)
	}
	// The conversation shows the task as taken over.
	_, conv := call(t, srv, phone, "GET", "/conversations/conv-1", nil)
	if tasks := list(t, conv["tasks"]); obj(t, tasks[0])["status"] != "human_takeover" {
		t.Errorf("conversation task = %v", tasks[0])
	}

	// Another session is told who holds it; watch isn't served yet.
	status, out = call(t, srv, phone, "POST", "/tasks/"+task.ID+"/takeover", nil)
	if status != http.StatusConflict || out["code"] != "task_held" || obj(t, out["hold"])["id"] != holdID {
		t.Errorf("second session = %d %v", status, out)
	}
	status, out = call(t, srv, phone, "POST", "/tasks/"+task.ID+"/takeover", map[string]any{"mode": "watch"})
	if status != http.StatusBadRequest || out["code"] != "unsupported_mode" {
		t.Errorf("watch = %d %v", status, out)
	}

	status, out = call(t, srv, laptop, "POST", "/tasks/"+task.ID+"/takeover/heartbeat", map[string]string{"hold_id": holdID})
	if status != http.StatusOK || obj(t, out["hold"])["id"] != holdID {
		t.Errorf("heartbeat = %d %v", status, out)
	}

	// Take back from the phone: the laptop's next heartbeat learns why.
	status, out = call(t, srv, phone, "POST", "/tasks/"+task.ID+"/release", nil)
	if status != http.StatusOK || out["status"] != "awaiting_input" || obj(t, out["hold"])["ended_reason"] != "taken_back" {
		t.Fatalf("take back = %d %v", status, out)
	}
	status, out = call(t, srv, laptop, "POST", "/tasks/"+task.ID+"/takeover/heartbeat", map[string]string{"hold_id": holdID})
	if status != http.StatusGone || out["code"] != "hold_ended" || out["ended_reason"] != "taken_back" {
		t.Errorf("heartbeat after a take back = %d %v", status, out)
	}
	if got := srv.Task(task.ID); got.Status != registry.TaskStatusAwaitingInput || got.Attention != nil {
		t.Errorf("task after release = %s, attention %+v", got.Status, got.Attention)
	}
	if srv.ActiveHold(task.ID) != nil {
		t.Error("a hold is still active after the release")
	}

	// A forced takeover, then a release by hold id; and a failed task
	// kept for a sign-in is held without changing status.
	_, first := call(t, srv, laptop, "POST", "/tasks/"+task.ID+"/takeover", nil)
	status, out = call(t, srv, phone, "POST", "/tasks/"+task.ID+"/takeover", map[string]any{"force": true, "ttl_seconds": 15})
	if status != http.StatusOK || obj(t, out["hold"])["id"] == obj(t, first["hold"])["id"] || obj(t, out["hold"])["ttl_seconds"] != float64(15) {
		t.Fatalf("forced takeover = %d %v", status, out)
	}
	status, out = call(t, srv, phone, "POST", "/tasks/"+task.ID+"/release", map[string]any{"hold_id": obj(t, out["hold"])["id"]})
	if status != http.StatusOK || out["status"] != "awaiting_input" || obj(t, out["hold"])["ended_reason"] != "released" {
		t.Errorf("release = %d %v", status, out)
	}
	failed := srv.SeedFailedTask("conv-2", registry.ErrorClassLoginRequired, "sign in")
	status, out = call(t, srv, laptop, "POST", "/tasks/"+failed.ID+"/takeover", nil)
	if status != http.StatusOK || obj(t, out["hold"])["prev_status"] != "failed" || srv.Task(failed.ID).Status != registry.TaskStatusFailed {
		t.Errorf("takeover of a failed task = %d %v", status, out)
	}
	done := srv.SeedTask(apitest.Task{Status: registry.TaskStatusCompleted})
	if status, out := call(t, srv, laptop, "POST", "/tasks/"+done.ID+"/takeover", nil); status != http.StatusConflict || out["code"] != "task_inactive" {
		t.Errorf("takeover of a completed task = %d %v", status, out)
	}
}

// WithClock decides when a lease runs out.
func TestWithClock(t *testing.T) {
	now := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	srv := apitest.New(t, apitest.WithClock(func() time.Time { return now }))
	token := srv.Login()
	task := srv.SeedTask(apitest.Task{})
	status, out := call(t, srv, token, "POST", "/tasks/"+task.ID+"/takeover", nil)
	if status != http.StatusOK {
		t.Fatalf("takeover = %d %v", status, out)
	}
	if got := obj(t, out["hold"])["expires_at"]; got != "2026-11-10T09:00:45Z" {
		t.Errorf("expires_at = %v, want the fake clock plus 45s", got)
	}
	now = now.Add(15 * time.Second)
	_, out = call(t, srv, token, "POST", "/tasks/"+task.ID+"/takeover/heartbeat", map[string]any{"hold_id": obj(t, out["hold"])["id"]})
	if got := obj(t, out["hold"])["expires_at"]; got != "2026-11-10T09:01:00Z" {
		t.Errorf("expires_at after a heartbeat = %v", got)
	}
}

// Cancelling a task: a waiting one is failed as cancelled, a taken-over
// one is refused, an ended one too.
func TestCancelTask(t *testing.T) {
	srv := apitest.New(t)
	token := srv.Login()
	task := srv.SeedTask(apitest.Task{})
	if status, out := call(t, srv, token, "POST", "/tasks/"+task.ID+"/cancel", nil); status != http.StatusAccepted {
		t.Fatalf("cancel = %d %v", status, out)
	}
	if got := srv.Task(task.ID); got.Status != registry.TaskStatusFailed || got.ErrorClass != registry.ErrorClassCancelled {
		t.Errorf("cancelled task = %s/%s", got.Status, got.ErrorClass)
	}
	if status, _ := call(t, srv, token, "POST", "/tasks/"+task.ID+"/cancel", nil); status != http.StatusConflict {
		t.Errorf("cancel of an ended task = %d, want 409", status)
	}
	held := srv.SeedTask(apitest.Task{})
	call(t, srv, token, "POST", "/tasks/"+held.ID+"/takeover", nil)
	if status, _ := call(t, srv, token, "POST", "/tasks/"+held.ID+"/cancel", nil); status != http.StatusConflict {
		t.Errorf("cancel of a taken-over task = %d, want 409", status)
	}

	// A running task under a dispatch is cancelled through it.
	release := srv.Dispatcher.Block()
	defer release()
	running := srv.SeedTask(apitest.Task{ConversationID: "conv-run", Status: registry.TaskStatusRunning})
	_, d := call(t, srv, token, "POST", "/dispatch", map[string]string{"conversation_id": "conv-run", "message": "go"}, "Prefer", "respond-async")
	deadline := time.Now().Add(5 * time.Second)
	for len(srv.Dispatcher.Dispatches()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the dispatch never started")
		}
		time.Sleep(5 * time.Millisecond)
	}
	status, out := call(t, srv, token, "POST", "/tasks/"+running.ID+"/cancel", nil)
	if status != http.StatusAccepted || out["dispatch_id"] != d["dispatch_id"] {
		t.Errorf("cancel of a running task = %d %v; want its dispatch %v", status, out, d["dispatch_id"])
	}
	if got := srv.Task(running.ID); got.Status != registry.TaskStatusFailed || got.ErrorClass != registry.ErrorClassCancelled {
		t.Errorf("running task after cancel = %s/%s", got.Status, got.ErrorClass)
	}
}

// The fake executor behaves like a target for what the server does to a
// pane.
func TestExecutor(t *testing.T) {
	srv := apitest.New(t)
	ctx := context.Background()
	task := srv.SeedTask(apitest.Task{})
	e := srv.Executor
	if e.TmuxSocket() != apitest.TmuxSocket {
		t.Errorf("socket = %q", e.TmuxSocket())
	}
	if err := e.PasteText(ctx, task.TmuxSession, "hello", true); err != nil {
		t.Fatal(err)
	}
	if err := e.SendKey(ctx, task.TmuxSession, "Escape"); err != nil {
		t.Fatal(err)
	}
	e.SetPane(task.TmuxSession, "$ hello")
	if pane, err := e.CapturePane(ctx, task.TmuxSession); err != nil || pane != "$ hello" {
		t.Errorf("CapturePane = %q, %v", pane, err)
	}
	if s := e.Session(task.TmuxSession); s == nil || len(s.Input) != 2 || s.Input[0] != "hello" || s.Input[1] != "Escape" {
		t.Errorf("session = %+v", s)
	}
	if _, err := e.RunOnce(ctx, "tmux ls"); err != nil || len(e.Commands()) != 1 {
		t.Errorf("RunOnce recorded %v, %v", e.Commands(), err)
	}
	if err := e.KillSession(ctx, task.TmuxSession); err != nil {
		t.Fatal(err)
	}
	if ok, _ := e.HasSession(ctx, task.TmuxSession); ok || e.Session(task.TmuxSession) != nil {
		t.Error("the session is still there after KillSession")
	}
	if err := e.SendKeys(ctx, task.TmuxSession, "x", false); err == nil {
		t.Error("typing into a killed session succeeded")
	}
}
