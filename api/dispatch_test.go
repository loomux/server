package api_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Loomux/server/api"
	"github.com/Loomux/server/registry"
)

// LOOM-80 dispatch jobs: see docs/design/async-dispatch-design.md.

type dispatchReply struct {
	DispatchID     string `json:"dispatch_id"`
	ConversationID string `json:"conversation_id"`
	Status         string `json:"status"`
	Reply          string `json:"reply"`
	Error          string `json:"error"`
	ErrorClass     string `json:"error_class"`
	ConfirmationID string `json:"confirmation_id"`
	Code           string `json:"code"`
}

func postDispatch(t *testing.T, ctx context.Context, url, token string, body map[string]string, headers map[string]string) (*http.Response, dispatchReply, error) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, dispatchReply{}, err
	}
	defer resp.Body.Close()
	var out dispatchReply
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp, out, nil
}

func mustPostDispatch(t *testing.T, url, token string, body, headers map[string]string) (*http.Response, dispatchReply) {
	t.Helper()
	resp, out, err := postDispatch(t, context.Background(), url, token, body, headers)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return resp, out
}

func getDispatch(t *testing.T, baseURL, token, id string) (int, dispatchReply) {
	t.Helper()
	resp := authedRequest(t, http.MethodGet, baseURL+"/api/v1/dispatches/"+id, token, nil)
	defer resp.Body.Close()
	var out dispatchReply
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// pollDispatch polls GET /dispatches/{id} until it is no longer queued or
// running.
func pollDispatch(t *testing.T, baseURL, token, id string) dispatchReply {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		code, d := getDispatch(t, baseURL, token, id)
		if code != http.StatusOK {
			t.Fatalf("GET dispatch %s: status %d", id, code)
		}
		if d.Status != "queued" && d.Status != "running" {
			return d
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("dispatch %s never finished", id)
	return dispatchReply{}
}

// With neither mode asked for, the request doesn't wait (LOOM-81 shipped).
func TestDispatch_DefaultIsAsync(t *testing.T) {
	srv, dispatcher, _ := newTestServer(t)
	dispatcher.DispatchFunc = func(ctx context.Context, c, m, h string) (string, error) { return "the reply", nil }
	token, _ := login(t, srv.URL, testPassword)

	resp, out := mustPostDispatch(t, srv.URL+"/api/v1/dispatch", token, map[string]string{"conversation_id": "c1", "message": "hi"}, nil)
	if resp.StatusCode != http.StatusAccepted || out.DispatchID == "" || out.ConversationID != "c1" ||
		resp.Header.Get("Location") != "/api/v1/dispatches/"+out.DispatchID {
		t.Fatalf("default dispatch = %d %+v (Location %q)", resp.StatusCode, out, resp.Header.Get("Location"))
	}
	if resp.Header.Get("Preference-Applied") != "" {
		t.Errorf("Preference-Applied set without a Prefer header")
	}
}

func TestDispatch_WaitTrueBlocks_ReturnsReplyAndIDs(t *testing.T) {
	srv, dispatcher, _ := newTestServer(t)
	dispatcher.DispatchFunc = func(ctx context.Context, c, m, h string) (string, error) { return "the reply", nil }
	token, _ := login(t, srv.URL, testPassword)

	resp, out := mustPostDispatch(t, srv.URL+"/api/v1/dispatch?wait=true", token, map[string]string{"conversation_id": "c1", "message": "hi"}, nil)
	if resp.StatusCode != http.StatusOK || out.Reply != "the reply" || out.DispatchID == "" || out.ConversationID != "c1" {
		t.Fatalf("blocking dispatch = %d %+v", resp.StatusCode, out)
	}
}

func TestDispatch_Async_PreferHeader(t *testing.T) {
	srv, dispatcher, _ := newTestServer(t)
	release := make(chan struct{})
	dispatcher.DispatchFunc = func(ctx context.Context, c, m, h string) (string, error) {
		<-release
		return "later", nil
	}
	token, _ := login(t, srv.URL, testPassword)

	resp, out := mustPostDispatch(t, srv.URL+"/api/v1/dispatch", token,
		map[string]string{"message": "hi"}, map[string]string{"Prefer": "respond-async"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	if out.DispatchID == "" || out.ConversationID == "" || out.Status != "queued" {
		t.Fatalf("202 body = %+v", out)
	}
	if loc := resp.Header.Get("Location"); loc != "/api/v1/dispatches/"+out.DispatchID {
		t.Fatalf("Location = %q", loc)
	}
	if pa := resp.Header.Get("Preference-Applied"); pa != "respond-async" {
		t.Fatalf("Preference-Applied = %q", pa)
	}
	close(release)
	if d := pollDispatch(t, srv.URL, token, out.DispatchID); d.Status != "succeeded" || d.Reply != "later" {
		t.Fatalf("finished dispatch = %+v", d)
	}
}

// ?async=true is no longer part of the API (freeze review item 4): it is
// ignored, so ?wait=true&async=true is simply a blocking request.
func TestDispatch_AsyncQueryParamIgnored(t *testing.T) {
	srv, dispatcher, _ := newTestServer(t)
	dispatcher.DispatchFunc = func(ctx context.Context, c, m, h string) (string, error) { return "ok", nil }
	token, _ := login(t, srv.URL, testPassword)
	resp, out := mustPostDispatch(t, srv.URL+"/api/v1/dispatch?wait=true&async=true", token, map[string]string{"conversation_id": "c", "message": "hi"}, nil)
	if resp.StatusCode != http.StatusOK || out.Reply != "ok" {
		t.Fatalf("?wait=true&async=true = %d %+v, want a blocking 200", resp.StatusCode, out)
	}
}

func TestDispatch_WaitAndPreferAsync_BadRequest(t *testing.T) {
	srv, _, _ := newTestServer(t)
	token, _ := login(t, srv.URL, testPassword)
	resp, _ := mustPostDispatch(t, srv.URL+"/api/v1/dispatch?wait=true", token, map[string]string{"conversation_id": "c", "message": "hi"},
		map[string]string{"Prefer": "respond-async"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// Acceptance 1: closing the browser mid-turn doesn't affect the task, and
// reopening the conversation shows the result.
func TestDispatch_ClientDisconnectMidTurn_JobFinishes(t *testing.T) {
	srv, dispatcher, _ := newTestServer(t)
	started := make(chan struct{})
	release := make(chan struct{})
	var jobCtxErr atomic.Value
	dispatcher.DispatchFunc = func(ctx context.Context, c, m, h string) (string, error) {
		close(started)
		<-release
		if ctx.Err() != nil {
			jobCtxErr.Store(ctx.Err().Error())
		}
		return "finished anyway", nil
	}
	token, _ := login(t, srv.URL, testPassword)

	reqCtx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, _, err := postDispatch(t, reqCtx, srv.URL+"/api/v1/dispatch?wait=true", token, map[string]string{"conversation_id": "c-gone", "message": "long job"}, nil)
		errc <- err
	}()
	<-started
	cancel() // the tab closes
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("client request err = %v, want canceled", err)
	}
	close(release)

	var conv struct {
		Messages []struct {
			Role       string `json:"role"`
			Content    string `json:"content"`
			DispatchID string `json:"dispatch_id"`
		} `json:"messages"`
		Dispatches []dispatchReply `json:"dispatches"`
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp := authedRequest(t, http.MethodGet, srv.URL+"/api/v1/conversations/c-gone", token, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET conversation: %d", resp.StatusCode)
		}
		_ = json.NewDecoder(resp.Body).Decode(&conv)
		resp.Body.Close()
		if len(conv.Dispatches) == 1 && conv.Dispatches[0].Status == "succeeded" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("conversation never showed a finished dispatch: %+v", conv)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if conv.Dispatches[0].Reply != "finished anyway" {
		t.Fatalf("dispatch = %+v", conv.Dispatches[0])
	}
	if len(conv.Messages) == 0 || conv.Messages[0].Role != "user" || conv.Messages[0].Content != "long job" ||
		conv.Messages[0].DispatchID != conv.Dispatches[0].DispatchID {
		t.Fatalf("user message not in history: %+v", conv.Messages)
	}
	if v := jobCtxErr.Load(); v != nil {
		t.Fatalf("job context was cancelled with the request: %v", v)
	}
}

// Acceptance 2: two identical POSTs with the same Idempotency-Key produce
// one dispatch.
func TestDispatch_IdempotencyKey_OneDispatch(t *testing.T) {
	srv, dispatcher, _ := newTestServer(t)
	var calls atomic.Int32
	dispatcher.DispatchFunc = func(ctx context.Context, c, m, h string) (string, error) {
		calls.Add(1)
		return "once", nil
	}
	token, _ := login(t, srv.URL, testPassword)
	body := map[string]string{"conversation_id": "c-idem", "message": "do it once"}
	hdr := map[string]string{"Idempotency-Key": "key-123"}

	r1, a := mustPostDispatch(t, srv.URL+"/api/v1/dispatch?wait=true", token, body, hdr)
	r2, b := mustPostDispatch(t, srv.URL+"/api/v1/dispatch?wait=true", token, body, hdr)
	if r1.StatusCode != http.StatusOK || r2.StatusCode != http.StatusOK {
		t.Fatalf("statuses = %d, %d", r1.StatusCode, r2.StatusCode)
	}
	if a.DispatchID != b.DispatchID || b.Reply != "once" {
		t.Fatalf("responses = %+v / %+v, want one dispatch", a, b)
	}
	if calls.Load() != 1 {
		t.Fatalf("dispatcher called %d times, want 1", calls.Load())
	}
}

func TestDispatch_IdempotencyKeyReused_Unprocessable(t *testing.T) {
	srv, dispatcher, _ := newTestServer(t)
	dispatcher.DispatchFunc = func(ctx context.Context, c, m, h string) (string, error) { return "ok", nil }
	token, _ := login(t, srv.URL, testPassword)
	hdr := map[string]string{"Idempotency-Key": "key-x"}
	mustPostDispatch(t, srv.URL+"/api/v1/dispatch?wait=true", token, map[string]string{"conversation_id": "c", "message": "one"}, hdr)
	resp, out := mustPostDispatch(t, srv.URL+"/api/v1/dispatch?wait=true", token, map[string]string{"conversation_id": "c", "message": "two"}, hdr)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	if out.Code != "idempotency_conflict" || out.Error == "" {
		t.Fatalf("reused-key body = %+v, want code idempotency_conflict and an error", out)
	}
}

func TestDispatch_ConversationBusy_Conflict(t *testing.T) {
	srv, dispatcher, _ := newTestServer(t)
	release := make(chan struct{})
	defer close(release)
	dispatcher.DispatchFunc = func(ctx context.Context, c, m, h string) (string, error) {
		<-release
		return "ok", nil
	}
	token, _ := login(t, srv.URL, testPassword)
	async := map[string]string{"Prefer": "respond-async"}
	_, first := mustPostDispatch(t, srv.URL+"/api/v1/dispatch", token, map[string]string{"conversation_id": "c-busy", "message": "one"}, async)
	resp, second := mustPostDispatch(t, srv.URL+"/api/v1/dispatch", token, map[string]string{"conversation_id": "c-busy", "message": "two"}, async)
	if resp.StatusCode != http.StatusConflict || second.DispatchID != first.DispatchID {
		t.Fatalf("second dispatch = %d %+v, want 409 naming %s", resp.StatusCode, second, first.DispatchID)
	}
	if second.Code != "conversation_busy" || second.Error == "" {
		t.Fatalf("busy body = %+v, want code conversation_busy and an error", second)
	}
}

func TestDispatch_Failure_BlockingReturns500WithClass(t *testing.T) {
	srv, dispatcher, _ := newTestServer(t)
	dispatcher.DispatchFunc = func(ctx context.Context, c, m, h string) (string, error) {
		return "", errors.New("router: dispatch: it broke")
	}
	token, _ := login(t, srv.URL, testPassword)
	resp, out := mustPostDispatch(t, srv.URL+"/api/v1/dispatch?wait=true", token, map[string]string{"conversation_id": "c", "message": "hi"}, nil)
	if resp.StatusCode != http.StatusInternalServerError || out.Error != "router: dispatch: it broke" ||
		out.ErrorClass != "internal" || out.DispatchID == "" {
		t.Fatalf("failed dispatch = %d %+v", resp.StatusCode, out)
	}
}

func TestDispatch_ShuttingDown_ServiceUnavailable(t *testing.T) {
	srv, dispatcher, _ := newTestServer(t)
	_ = dispatcher.Jobs.Shutdown(context.Background())
	token, _ := login(t, srv.URL, testPassword)
	resp, _ := mustPostDispatch(t, srv.URL+"/api/v1/dispatch?wait=true", token, map[string]string{"conversation_id": "c", "message": "hi"}, nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}

func TestGetDispatch_Unknown_NotFound(t *testing.T) {
	srv, _, _ := newTestServer(t)
	token, _ := login(t, srv.URL, testPassword)
	if code, _ := getDispatch(t, srv.URL, token, "nope"); code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", code)
	}
}

// A conversation whose only turn is still queued already resolves: its
// user message was stored at submit.
func TestGetConversation_InFlightDispatchOnly(t *testing.T) {
	srv, dispatcher, _ := newTestServer(t)
	release := make(chan struct{})
	defer close(release)
	dispatcher.DispatchFunc = func(ctx context.Context, c, m, h string) (string, error) {
		<-release
		return "ok", nil
	}
	token, _ := login(t, srv.URL, testPassword)
	_, d := mustPostDispatch(t, srv.URL+"/api/v1/dispatch", token, map[string]string{"conversation_id": "c-new", "message": "hi"}, map[string]string{"Prefer": "respond-async"})

	resp := authedRequest(t, http.MethodGet, srv.URL+"/api/v1/conversations/c-new", token, nil)
	defer resp.Body.Close()
	var conv struct {
		Dispatches []dispatchReply `json:"dispatches"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&conv)
	if resp.StatusCode != http.StatusOK || len(conv.Dispatches) != 1 || conv.Dispatches[0].DispatchID != d.DispatchID ||
		(conv.Dispatches[0].Status != "queued" && conv.Dispatches[0].Status != "running") {
		t.Fatalf("conversation = %d %+v", resp.StatusCode, conv)
	}
}

func openStream(t *testing.T, baseURL, token, conversationID string) *bufio.Reader {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, baseURL+"/api/v1/conversations/"+conversationID+"/stream", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return bufio.NewReader(resp.Body)
}

func readDispatchUpdate(t *testing.T, r *bufio.Reader) dispatchReply {
	t.Helper()
	for {
		ev := readSSEEventWithTimeout(t, r, 3*time.Second)
		if ev.Event != "dispatch_update" {
			continue
		}
		var d dispatchReply
		if err := json.Unmarshal([]byte(ev.Data), &d); err != nil {
			t.Fatalf("unmarshal %q: %v", ev.Data, err)
		}
		return d
	}
}

// The stream reports a job's progress; a client connecting mid-turn first
// gets the job's current state, then its result.
func TestStream_DispatchUpdates(t *testing.T) {
	srv, dispatcher, _ := newTestServer(t, api.WithStreamPollInterval(10*time.Millisecond))
	started := make(chan struct{})
	release := make(chan struct{})
	dispatcher.DispatchFunc = func(ctx context.Context, c, m, h string) (string, error) {
		close(started)
		<-release
		return "streamed", nil
	}
	token, _ := login(t, srv.URL, testPassword)
	_, d := mustPostDispatch(t, srv.URL+"/api/v1/dispatch", token, map[string]string{"conversation_id": "c-sse", "message": "hi"}, map[string]string{"Prefer": "respond-async"})
	<-started

	r := openStream(t, srv.URL, token, "c-sse")
	first := readDispatchUpdate(t, r)
	if first.DispatchID != d.DispatchID || first.Status != "running" {
		t.Fatalf("first dispatch_update = %+v, want running", first)
	}
	close(release)
	last := readDispatchUpdate(t, r)
	if last.DispatchID != d.DispatchID || last.Status != "succeeded" || last.Reply != "streamed" {
		t.Fatalf("second dispatch_update = %+v, want succeeded", last)
	}
}

// Finished jobs from earlier turns aren't replayed on connect.
func TestStream_DoesNotReplayFinishedDispatches(t *testing.T) {
	srv, dispatcher, _ := newTestServer(t, api.WithStreamPollInterval(10*time.Millisecond))
	release := make(chan struct{})
	dispatcher.DispatchFunc = func(ctx context.Context, c, m, h string) (string, error) {
		if m == "new" {
			<-release // in flight until the stream has seen it
		}
		return m, nil
	}
	token, _ := login(t, srv.URL, testPassword)
	mustPostDispatch(t, srv.URL+"/api/v1/dispatch?wait=true", token, map[string]string{"conversation_id": "c-old", "message": "old"}, nil)

	r := openStream(t, srv.URL, token, "c-old")
	_, d := mustPostDispatch(t, srv.URL+"/api/v1/dispatch", token, map[string]string{"conversation_id": "c-old", "message": "new"}, map[string]string{"Prefer": "respond-async"})
	first := readDispatchUpdate(t, r)
	if first.DispatchID != d.DispatchID {
		t.Fatalf("replayed an earlier finished dispatch: %+v", first)
	}
	close(release)
	for {
		u := readDispatchUpdate(t, r)
		if u.DispatchID != d.DispatchID {
			t.Fatalf("replayed an earlier finished dispatch: %+v", u)
		}
		if u.Status == "succeeded" {
			return
		}
	}
}

// A message logged outside any turn the client is following — an agent's
// late reply (LOOM-121) — is announced, so the client can refetch the
// transcript. Messages already there when the stream opens aren't.
func TestStream_MessageAdded(t *testing.T) {
	srv, _, store := newTestServer(t, api.WithStreamPollInterval(10*time.Millisecond))
	ctx := context.Background()
	old := &registry.Message{ID: "m-old", ConversationID: "c-msg", Role: registry.MessageRoleAssistant, Content: "earlier"}
	if err := store.CreateMessage(ctx, old); err != nil {
		t.Fatal(err)
	}
	ws := createTestWorkspace(t, store, "ws-msg", registry.WorkspaceStatusIdle)
	createTestTask(t, store, "t-1", ws.ID, "c-msg", registry.TaskStatusAwaitingInput)
	token, _ := login(t, srv.URL, testPassword)
	r := openStream(t, srv.URL, token, "c-msg")
	time.Sleep(50 * time.Millisecond) // the stream has seen the old message
	late := &registry.Message{ID: "m-late", ConversationID: "c-msg", TaskID: "t-1", Role: registry.MessageRoleAssistant, Content: "the build finished"}
	if err := store.CreateMessage(ctx, late); err != nil {
		t.Fatal(err)
	}
	for {
		ev := readSSEEventWithTimeout(t, r, 3*time.Second)
		if ev.Event != "message_added" {
			continue
		}
		var m struct {
			MessageID string `json:"message_id"`
			TaskID    string `json:"task_id"`
			Role      string `json:"role"`
		}
		if err := json.Unmarshal([]byte(ev.Data), &m); err != nil {
			t.Fatalf("unmarshal %q: %v", ev.Data, err)
		}
		if m.MessageID != "m-late" || m.TaskID != "t-1" || m.Role != "assistant" {
			t.Fatalf("message_added = %+v, want the late message only", m)
		}
		return
	}
}

// A well-formed conversation_id (a UUID, or a short id of letters, digits
// and dashes) is accepted; a malformed one or an over-long workspace_hint
// is 400 invalid_request before anything is dispatched (LOOM-154).
func TestDispatch_ValidatesConversationIDAndWorkspaceHint(t *testing.T) {
	srv, dispatcher, _ := newTestServer(t)
	var calls atomic.Int32
	dispatcher.DispatchFunc = func(ctx context.Context, c, m, h string) (string, error) {
		calls.Add(1)
		return "ok", nil
	}
	token, _ := login(t, srv.URL, testPassword)

	for _, id := range []string{"0f8fad5b-d9cb-469f-a165-70867728950e", "c-1", strings.Repeat("a", 64)} {
		resp, out := mustPostDispatch(t, srv.URL+"/api/v1/dispatch?wait=true", token, map[string]string{"conversation_id": id, "message": "hi"}, nil)
		if resp.StatusCode != http.StatusOK || out.ConversationID != id {
			t.Errorf("conversation_id %q: status %d, conversation %q; want 200 and the same id", id, resp.StatusCode, out.ConversationID)
		}
	}
	accepted := calls.Load()

	for _, body := range []map[string]string{
		{"conversation_id": strings.Repeat("a", 65), "message": "hi"},
		{"conversation_id": "c_1", "message": "hi"},
		{"conversation_id": "c1", "message": "hi", "workspace_hint": strings.Repeat("w", 256)},
	} {
		resp, out := mustPostDispatch(t, srv.URL+"/api/v1/dispatch?wait=true", token, body, nil)
		if resp.StatusCode != http.StatusBadRequest || out.Code != "invalid_request" {
			t.Errorf("%.40v: status %d code %q, want 400 invalid_request", body, resp.StatusCode, out.Code)
		}
	}
	if calls.Load() != accepted {
		t.Error("a rejected request still reached the dispatcher")
	}
}
