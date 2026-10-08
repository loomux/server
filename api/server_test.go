package api_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/Loomux/server/api"
	"github.com/Loomux/server/dispatch"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/registry/sqlite"
)

const testPassword = "correct-horse-battery-staple"

// fakeDispatcher stands in for the router: tests set DispatchFunc, and a
// real dispatch.Service (Jobs) runs it as a job, exactly as app wires the
// router in production (LOOM-80).
type fakeDispatcher struct {
	DispatchFunc func(ctx context.Context, conversationID, message, workspaceHint string) (string, error)
	Jobs         *dispatch.Service
}

func (f *fakeDispatcher) Dispatch(ctx context.Context, conversationID, message, workspaceHint string) (string, error) {
	return f.DispatchFunc(ctx, conversationID, message, workspaceHint)
}

func newTestStore(t *testing.T) registry.Store {
	t.Helper()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return store
}

// testPasswordHash is testPassword's bcrypt hash at the minimum cost,
// made once: the default cost, on every test server and login under
// -race, made this package the slowest in the suite.
var testPasswordHash = func() func(t *testing.T) string {
	var once sync.Once
	var hash []byte
	var err error
	return func(t *testing.T) string {
		t.Helper()
		once.Do(func() { hash, err = bcrypt.GenerateFromPassword([]byte(testPassword), bcrypt.MinCost) })
		if err != nil {
			t.Fatalf("bcrypt: %v", err)
		}
		return string(hash)
	}
}()

func newTestServer(t *testing.T, opts ...api.Option) (*httptest.Server, *fakeDispatcher, registry.Store) {
	t.Helper()
	return newTestServerWith(t, func(registry.Store) []api.Option { return opts })
}

// newTestServerWith is newTestServer for options that need the store.
func newTestServerWith(t *testing.T, optsFor func(registry.Store) []api.Option) (*httptest.Server, *fakeDispatcher, registry.Store) {
	t.Helper()
	return newTestServerOn(t, newTestStore(t), optsFor)
}

// newTestServerOn is newTestServerWith on a store of the caller's (one
// with a master key, say).
func newTestServerOn(t *testing.T, store registry.Store, optsFor func(registry.Store) []api.Option) (*httptest.Server, *fakeDispatcher, registry.Store) {
	t.Helper()
	hash := testPasswordHash(t)
	dispatcher := &fakeDispatcher{}
	dispatcher.Jobs = dispatch.New(store, func(ctx context.Context, d *registry.Dispatch) (string, error) {
		return dispatcher.Dispatch(ctx, d.ConversationID, d.Message, d.WorkspaceHint)
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = dispatcher.Jobs.Shutdown(ctx)
	})
	server := api.NewServer(dispatcher.Jobs, store, store, store, store, store, store, []byte(hash), optsFor(store)...)
	httpSrv := httptest.NewServer(server)
	t.Cleanup(httpSrv.Close)
	return httpSrv, dispatcher, store
}

// createTestWorkspace inserts a target + workspace directly via the store,
// as a fixture for endpoint tests that aren't exercising workspace-creation
// behavior themselves.
func createTestWorkspace(t *testing.T, s registry.Store, name string, status registry.WorkspaceStatus) *registry.Workspace {
	t.Helper()
	target := &registry.Target{ID: "target-" + name, Name: "target-" + name, Kind: registry.TargetKindLocal}
	if err := s.CreateTarget(context.Background(), target); err != nil {
		t.Fatalf("CreateTarget: %v", err)
	}
	ws := &registry.Workspace{
		ID:       "ws-" + name,
		Name:     name,
		Path:     "/fixture/" + name,
		TargetID: target.ID,
		Status:   status,
	}
	if err := s.CreateWorkspace(context.Background(), ws); err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	return ws
}

// createTestTask inserts a task (and its workspace/target, if needed)
// directly via the store, as a fixture for conversation-endpoint tests
// that aren't exercising task-creation behavior themselves.
func createTestTask(t *testing.T, s registry.Store, id, workspaceID, conversationID string, status registry.TaskStatus) *registry.Task {
	t.Helper()
	task := &registry.Task{
		ID:             id,
		WorkspaceID:    workspaceID,
		Kind:           registry.TaskKindShell,
		TmuxSession:    "sess-" + id,
		Status:         status,
		ConversationID: conversationID,
	}
	if err := s.CreateTask(context.Background(), task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	return task
}

// createTestMessage inserts a message directly via the store, as a
// fixture for conversation-endpoint tests that aren't exercising
// message-creation behavior themselves.
func createTestMessage(t *testing.T, s registry.Store, id, conversationID, taskID string, role registry.MessageRole, content string) *registry.Message {
	t.Helper()
	msg := &registry.Message{ID: id, ConversationID: conversationID, TaskID: taskID, Role: role, Content: content}
	if err := s.CreateMessage(context.Background(), msg); err != nil {
		t.Fatalf("CreateMessage: %v", err)
	}
	return msg
}

func login(t *testing.T, baseURL, password string) (token string, status int) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"password": password})
	resp, err := http.Post(baseURL+"/api/v1/login", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /api/v1/login: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", resp.StatusCode
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode login response: %v", err)
	}
	return out.Token, resp.StatusCode
}

func authedRequest(t *testing.T, method, url, token string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	return resp
}

func TestLogin_CorrectPassword_ReturnsToken(t *testing.T) {
	srv, _, _ := newTestServer(t)
	token, status := login(t, srv.URL, testPassword)
	if status != http.StatusOK {
		t.Fatalf("login status = %d, want %d", status, http.StatusOK)
	}
	if token == "" {
		t.Fatal("login returned an empty token")
	}
}

func TestLogin_WrongPassword_ReturnsUnauthorized(t *testing.T) {
	srv, _, _ := newTestServer(t)
	_, status := login(t, srv.URL, "wrong-password")
	if status != http.StatusUnauthorized {
		t.Fatalf("login status = %d, want %d", status, http.StatusUnauthorized)
	}
}

func TestLogin_MalformedBody_ReturnsBadRequest(t *testing.T) {
	srv, _, _ := newTestServer(t)
	resp, err := http.Post(srv.URL+"/api/v1/login", "application/json", bytes.NewReader([]byte("{not json")))
	if err != nil {
		t.Fatalf("POST /api/v1/login: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
}

func TestDispatch_NoToken_ReturnsUnauthorized(t *testing.T) {
	srv, _, _ := newTestServer(t)
	body, _ := json.Marshal(map[string]string{"conversation_id": "c1", "message": "hi"})
	resp := authedRequest(t, http.MethodPost, srv.URL+"/api/v1/dispatch?wait=true", "", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

func TestDispatch_InvalidToken_ReturnsUnauthorized(t *testing.T) {
	srv, _, _ := newTestServer(t)
	body, _ := json.Marshal(map[string]string{"conversation_id": "c1", "message": "hi"})
	resp := authedRequest(t, http.MethodPost, srv.URL+"/api/v1/dispatch?wait=true", "not-a-real-token", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

func TestDispatch_ValidToken_CallsDispatcherAndReturnsReply(t *testing.T) {
	srv, dispatcher, _ := newTestServer(t)
	dispatcher.DispatchFunc = func(ctx context.Context, conversationID, message, workspaceHint string) (string, error) {
		if conversationID != "c1" || message != "hello" || workspaceHint != "" {
			t.Fatalf("Dispatch called with (%q, %q, %q), want (c1, hello, \"\")", conversationID, message, workspaceHint)
		}
		return "the reply", nil
	}

	token, status := login(t, srv.URL, testPassword)
	if status != http.StatusOK {
		t.Fatalf("login status = %d", status)
	}

	body, _ := json.Marshal(map[string]string{"conversation_id": "c1", "message": "hello"})
	resp := authedRequest(t, http.MethodPost, srv.URL+"/api/v1/dispatch?wait=true", token, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	var out struct {
		Reply string `json:"reply"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Reply != "the reply" {
		t.Fatalf("reply = %q, want %q", out.Reply, "the reply")
	}
}

// TestDispatch_WorkspaceHint_ReachesDispatcher proves the optional
// workspace_hint request field (LOOM-46) flows through unmodified to
// Dispatcher.Dispatch, and that omitting it entirely (as every request
// before this ticket did) still resolves to an empty hint rather than
// an error.
func TestDispatch_WorkspaceHint_ReachesDispatcher(t *testing.T) {
	srv, dispatcher, _ := newTestServer(t)
	var gotHint string
	dispatcher.DispatchFunc = func(ctx context.Context, conversationID, message, workspaceHint string) (string, error) {
		gotHint = workspaceHint
		return "ok", nil
	}
	token, _ := login(t, srv.URL, testPassword)

	body, _ := json.Marshal(map[string]string{
		"conversation_id": "c1", "message": "hello", "workspace_hint": "ws-hinted",
	})
	resp := authedRequest(t, http.MethodPost, srv.URL+"/api/v1/dispatch?wait=true", token, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if gotHint != "ws-hinted" {
		t.Fatalf("Dispatch received workspaceHint = %q, want %q", gotHint, "ws-hinted")
	}
}

func TestDispatch_MissingFields_ReturnsBadRequest(t *testing.T) {
	srv, _, _ := newTestServer(t)
	token, _ := login(t, srv.URL, testPassword)

	body, _ := json.Marshal(map[string]string{"conversation_id": "", "message": ""})
	resp := authedRequest(t, http.MethodPost, srv.URL+"/api/v1/dispatch?wait=true", token, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
}

func TestLogout_RevokesToken(t *testing.T) {
	srv, dispatcher, _ := newTestServer(t)
	dispatcher.DispatchFunc = func(ctx context.Context, conversationID, message, workspaceHint string) (string, error) {
		return "ok", nil
	}
	token, _ := login(t, srv.URL, testPassword)

	logoutResp := authedRequest(t, http.MethodPost, srv.URL+"/api/v1/logout", token, nil)
	defer logoutResp.Body.Close()
	if logoutResp.StatusCode != http.StatusNoContent {
		t.Fatalf("logout status = %d, want %d", logoutResp.StatusCode, http.StatusNoContent)
	}

	body, _ := json.Marshal(map[string]string{"conversation_id": "c1", "message": "hi"})
	resp := authedRequest(t, http.MethodPost, srv.URL+"/api/v1/dispatch?wait=true", token, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("dispatch after logout status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

type sessionSummary struct {
	ID         string `json:"id"`
	CreatedAt  string `json:"created_at"`
	LastUsedAt string `json:"last_used_at"`
	Current    bool   `json:"current"`
}

func listSessions(t *testing.T, baseURL, token string) (*http.Response, []sessionSummary) {
	t.Helper()
	resp := authedRequest(t, http.MethodGet, baseURL+"/api/v1/sessions", token, nil)
	if resp.StatusCode != http.StatusOK {
		return resp, nil
	}
	var out struct {
		Sessions []sessionSummary `json:"sessions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp, out.Sessions
}

func TestListSessions_NoToken_ReturnsUnauthorized(t *testing.T) {
	srv, _, _ := newTestServer(t)
	resp := authedRequest(t, http.MethodGet, srv.URL+"/api/v1/sessions", "", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

// TestListSessions_MultipleLogins_ListsAllMarkingCurrent proves
// GET /api/v1/sessions (LOOM-47) returns every active session — not
// just the caller's own — most-recently-used first, with exactly the
// session backing the request's own token marked "current".
func TestListSessions_MultipleLogins_ListsAllMarkingCurrent(t *testing.T) {
	srv, dispatcher, _ := newTestServer(t)
	dispatcher.DispatchFunc = func(ctx context.Context, conversationID, message, workspaceHint string) (string, error) {
		return "ok", nil
	}

	tokenA, status := login(t, srv.URL, testPassword)
	if status != http.StatusOK {
		t.Fatalf("login A status = %d", status)
	}
	time.Sleep(10 * time.Millisecond)
	tokenB, status := login(t, srv.URL, testPassword)
	if status != http.StatusOK {
		t.Fatalf("login B status = %d", status)
	}

	resp, sessions := listSessions(t, srv.URL, tokenB)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if len(sessions) != 2 {
		t.Fatalf("got %d sessions, want 2", len(sessions))
	}
	// Most-recently-used first: the request made with tokenB just now
	// (listing itself touches tokenB's LastUsedAt via requireAuth) sorts
	// ahead of tokenA, which has been idle since login.
	if !sessions[0].Current {
		t.Fatalf("sessions[0] = %+v, want the current (tokenB) session listed first and marked current", sessions[0])
	}
	if sessions[1].Current {
		t.Fatalf("sessions[1] = %+v, want the other (tokenA) session not marked current", sessions[1])
	}

	_ = tokenA // both tokens exist only to prove two distinct sessions are listed
}

// TestListSessions_ExpiredSession_ExcludedFromListing proves the
// listing's "every active session" claim is actually true: a session
// past sessionTTL is excluded even though nothing has presented its
// token since expiry to trigger requireAuth's own opportunistic
// cleanup (see that method's doc comment) — the row would otherwise
// still sit in the store, unexpired-looking, until someone tried to use
// it.
func TestListSessions_ExpiredSession_ExcludedFromListing(t *testing.T) {
	// Seconds, not milliseconds: a bcrypt login under -race on a slow CI
	// runner can take longer than a short TTL, expiring the fresh session
	// before it's used.
	srv, dispatcher, _ := newTestServer(t, api.WithSessionTTL(2*time.Second))
	dispatcher.DispatchFunc = func(ctx context.Context, conversationID, message, workspaceHint string) (string, error) {
		return "ok", nil
	}

	staleToken, status := login(t, srv.URL, testPassword)
	if status != http.StatusOK {
		t.Fatalf("login (stale) status = %d", status)
	}

	time.Sleep(2500 * time.Millisecond) // past sessionTTL, staleToken's session is now expired

	freshToken, status := login(t, srv.URL, testPassword)
	if status != http.StatusOK {
		t.Fatalf("login (fresh) status = %d", status)
	}

	resp, sessions := listSessions(t, srv.URL, freshToken)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if len(sessions) != 1 {
		t.Fatalf("got %d sessions, want 1 (the expired one must be excluded): %+v", len(sessions), sessions)
	}
	if !sessions[0].Current {
		t.Fatalf("sessions[0] = %+v, want the fresh (current) session, not the expired one", sessions[0])
	}

	_ = staleToken // exists only to have created the now-expired session
}

func TestRevokeSession_NoToken_ReturnsUnauthorized(t *testing.T) {
	srv, _, _ := newTestServer(t)
	resp := authedRequest(t, http.MethodDelete, srv.URL+"/api/v1/sessions/does-not-exist", "", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

func TestRevokeSession_UnknownID_ReturnsNotFound(t *testing.T) {
	srv, _, _ := newTestServer(t)
	token, _ := login(t, srv.URL, testPassword)

	resp := authedRequest(t, http.MethodDelete, srv.URL+"/api/v1/sessions/does-not-exist", token, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}

// TestRevokeSession_ByID_InvalidatesThatTokenButNotOthers proves
// DELETE /api/v1/sessions/{id} (LOOM-47) revokes exactly the named
// session — generalizing /logout (which only ever revoked the
// presented token) to revoking any session the caller learned about via
// GET /api/v1/sessions, e.g. signing another device out remotely.
func TestRevokeSession_ByID_InvalidatesThatTokenButNotOthers(t *testing.T) {
	srv, dispatcher, _ := newTestServer(t)
	dispatcher.DispatchFunc = func(ctx context.Context, conversationID, message, workspaceHint string) (string, error) {
		return "ok", nil
	}

	tokenA, _ := login(t, srv.URL, testPassword)
	tokenB, _ := login(t, srv.URL, testPassword)

	_, sessions := listSessions(t, srv.URL, tokenA)
	var targetID string
	for _, s := range sessions {
		if !s.Current {
			targetID = s.ID
		}
	}
	if targetID == "" {
		t.Fatalf("could not find tokenB's session id among %+v", sessions)
	}

	revokeResp := authedRequest(t, http.MethodDelete, srv.URL+"/api/v1/sessions/"+targetID, tokenA, nil)
	defer revokeResp.Body.Close()
	if revokeResp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke status = %d, want %d", revokeResp.StatusCode, http.StatusNoContent)
	}

	// tokenB (revoked by id) can no longer authenticate...
	body, _ := json.Marshal(map[string]string{"conversation_id": "c1", "message": "hi"})
	respB := authedRequest(t, http.MethodPost, srv.URL+"/api/v1/dispatch?wait=true", tokenB, body)
	defer respB.Body.Close()
	if respB.StatusCode != http.StatusUnauthorized {
		t.Fatalf("dispatch with revoked tokenB status = %d, want %d", respB.StatusCode, http.StatusUnauthorized)
	}

	// ...but tokenA (the caller, untouched) still can.
	respA := authedRequest(t, http.MethodPost, srv.URL+"/api/v1/dispatch?wait=true", tokenA, body)
	defer respA.Body.Close()
	if respA.StatusCode != http.StatusOK {
		t.Fatalf("dispatch with tokenA status = %d, want %d", respA.StatusCode, http.StatusOK)
	}

	// Revoking the same id again is a 404, not a repeatable success.
	resp2 := authedRequest(t, http.MethodDelete, srv.URL+"/api/v1/sessions/"+targetID, tokenA, nil)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("re-revoke status = %d, want %d", resp2.StatusCode, http.StatusNotFound)
	}
}

// TestRevokeSession_CurrentSession_BehavesLikeLogout proves revoking
// the session backing the request's own token is allowed and has the
// same effect as /logout — no special-cased self-protection.
func TestRevokeSession_CurrentSession_BehavesLikeLogout(t *testing.T) {
	srv, _, _ := newTestServer(t)
	token, _ := login(t, srv.URL, testPassword)

	_, sessions := listSessions(t, srv.URL, token)
	if len(sessions) != 1 || !sessions[0].Current {
		t.Fatalf("sessions = %+v, want exactly one, marked current", sessions)
	}

	resp := authedRequest(t, http.MethodDelete, srv.URL+"/api/v1/sessions/"+sessions[0].ID, token, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke-self status = %d, want %d", resp.StatusCode, http.StatusNoContent)
	}

	body, _ := json.Marshal(map[string]string{"conversation_id": "c1", "message": "hi"})
	after := authedRequest(t, http.MethodPost, srv.URL+"/api/v1/dispatch?wait=true", token, body)
	defer after.Body.Close()
	if after.StatusCode != http.StatusUnauthorized {
		t.Fatalf("dispatch after self-revoke status = %d, want %d", after.StatusCode, http.StatusUnauthorized)
	}
}

func TestSession_ExpiresAfterTTL(t *testing.T) {
	srv, dispatcher, _ := newTestServer(t, api.WithSessionTTL(50*time.Millisecond))
	dispatcher.DispatchFunc = func(ctx context.Context, conversationID, message, workspaceHint string) (string, error) {
		return "ok", nil
	}
	token, _ := login(t, srv.URL, testPassword)

	time.Sleep(100 * time.Millisecond)

	body, _ := json.Marshal(map[string]string{"conversation_id": "c1", "message": "hi"})
	resp := authedRequest(t, http.MethodPost, srv.URL+"/api/v1/dispatch?wait=true", token, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status after TTL expiry = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

func TestSession_SlidingExpiration_ActivityExtendsSession(t *testing.T) {
	srv, dispatcher, _ := newTestServer(t, api.WithSessionTTL(2*time.Second))
	dispatcher.DispatchFunc = func(ctx context.Context, conversationID, message, workspaceHint string) (string, error) {
		return "ok", nil
	}
	token, _ := login(t, srv.URL, testPassword)
	body, _ := json.Marshal(map[string]string{"conversation_id": "c1", "message": "hi"})

	// Two requests spaced less than the TTL apart, each one should reset
	// the clock — proving activity keeps the session alive well past
	// what a single fixed expiration from login time would allow.
	for i := 0; i < 3; i++ {
		time.Sleep(1200 * time.Millisecond)
		resp := authedRequest(t, http.MethodPost, srv.URL+"/api/v1/dispatch?wait=true", token, body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d status = %d, want %d (sliding expiration should have kept the session alive)", i, resp.StatusCode, http.StatusOK)
		}
	}
}

func TestLogin_RepeatedFailures_TriggersBackoffWith429AndRetryAfter(t *testing.T) {
	srv, _, _ := newTestServer(t, api.WithLoginBackoff(200*time.Millisecond, 30*time.Second))

	_, status := login(t, srv.URL, "wrong")
	if status != http.StatusUnauthorized {
		t.Fatalf("first wrong attempt status = %d, want %d", status, http.StatusUnauthorized)
	}

	// Immediately retrying (well within the backoff window) must be
	// throttled — 429, not another 401, and it must not even have
	// re-checked the password.
	resp := authedRequest(t, http.MethodPost, srv.URL+"/api/v1/login", "", mustJSON(t, map[string]string{"password": "wrong"}))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("throttled attempt status = %d, want %d", resp.StatusCode, http.StatusTooManyRequests)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("throttled response has no Retry-After header")
	}
	var body struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode throttled body: %v", err)
	}
	if body.Code != "rate_limited" || body.Error == "" {
		t.Errorf("throttled body = %+v, want code rate_limited and an error", body)
	}
}

func TestLogin_SuccessAfterFailure_ResetsThrottle(t *testing.T) {
	srv, _, _ := newTestServer(t, api.WithLoginBackoff(50*time.Millisecond, 30*time.Second))

	_, status := login(t, srv.URL, "wrong")
	if status != http.StatusUnauthorized {
		t.Fatalf("wrong attempt status = %d, want %d", status, http.StatusUnauthorized)
	}
	time.Sleep(60 * time.Millisecond) // wait out the backoff so the correct attempt below isn't itself throttled

	token, status := login(t, srv.URL, testPassword)
	if status != http.StatusOK || token == "" {
		t.Fatalf("correct-password login after one prior failure: status = %d token = %q", status, token)
	}

	// A fresh wrong attempt right after a success must go back to a
	// plain 401 (base delay), not stay throttled from before the reset.
	_, status = login(t, srv.URL, "wrong-again")
	if status != http.StatusUnauthorized {
		t.Fatalf("wrong attempt right after a success: status = %d, want %d (throttle should have reset)", status, http.StatusUnauthorized)
	}
}

// TestLogin_NeverPermanentlyLocksOut is the core property the backoff
// design (over a hard lockout) exists for: no matter how many times an
// attacker fails, the real password still works once the (bounded) max
// delay has passed — the legitimate user is slowed down, never
// permanently denied.
func TestLogin_NeverPermanentlyLocksOut(t *testing.T) {
	srv, _, _ := newTestServer(t, api.WithLoginBackoff(10*time.Millisecond, 60*time.Millisecond))

	for i := 0; i < 10; i++ {
		_, status := login(t, srv.URL, "wrong")
		if status != http.StatusUnauthorized && status != http.StatusTooManyRequests {
			t.Fatalf("failure %d status = %d, want 401 or 429", i, status)
		}
		time.Sleep(15 * time.Millisecond) // stay ahead of the (capped) backoff so each attempt actually reaches the password check
	}

	time.Sleep(70 * time.Millisecond) // wait out the capped max delay
	token, status := login(t, srv.URL, testPassword)
	if status != http.StatusOK || token == "" {
		t.Fatalf("correct password after 10 failures + waiting out max delay: status = %d token = %q, want a successful login", status, token)
	}
}

// TestLogin_ThrottleIsGlobalNotPerClient proves the deliberate design
// choice: failures are shared server-wide, not scoped to a claimed
// source IP — a client can't dodge the throttle by presenting a
// different X-Forwarded-For on each request (this server doesn't even
// read that header, on purpose — see throttle.go).
func TestLogin_ThrottleIsGlobalNotPerClient(t *testing.T) {
	srv, _, _ := newTestServer(t, api.WithLoginBackoff(200*time.Millisecond, 30*time.Second))

	req1, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/login", bytes.NewReader(mustJSON(t, map[string]string{"password": "wrong"})))
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set("X-Forwarded-For", "203.0.113.1")
	resp1, err := http.DefaultClient.Do(req1)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp1.Body.Close()
	if resp1.StatusCode != http.StatusUnauthorized {
		t.Fatalf("first client's attempt status = %d, want %d", resp1.StatusCode, http.StatusUnauthorized)
	}

	req2, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/login", bytes.NewReader(mustJSON(t, map[string]string{"password": "wrong"})))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-Forwarded-For", "203.0.113.99") // a different claimed source
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second (differently-IP-claiming) client's attempt status = %d, want %d (throttle is global)", resp2.StatusCode, http.StatusTooManyRequests)
	}
}

func TestListWorkspaces_NoToken_ReturnsUnauthorized(t *testing.T) {
	srv, _, _ := newTestServer(t)
	resp := authedRequest(t, http.MethodGet, srv.URL+"/api/v1/workspaces", "", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

func TestListWorkspaces_ValidToken_ReturnsWorkspacesSortedByName(t *testing.T) {
	srv, _, store := newTestServer(t)
	createTestWorkspace(t, store, "zeta", registry.WorkspaceStatusIdle)
	createTestWorkspace(t, store, "alpha", registry.WorkspaceStatusActive)

	token, status := login(t, srv.URL, testPassword)
	if status != http.StatusOK {
		t.Fatalf("login status = %d", status)
	}

	resp := authedRequest(t, http.MethodGet, srv.URL+"/api/v1/workspaces", token, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	var out struct {
		Workspaces []struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			TargetID string `json:"target_id"`
			Status   string `json:"status"`
		} `json:"workspaces"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Workspaces) != 2 {
		t.Fatalf("got %d workspaces, want 2", len(out.Workspaces))
	}
	if out.Workspaces[0].Name != "alpha" || out.Workspaces[1].Name != "zeta" {
		t.Fatalf("workspace order = [%q, %q], want [alpha, zeta] (sorted by name)", out.Workspaces[0].Name, out.Workspaces[1].Name)
	}
	if out.Workspaces[0].ID != "ws-alpha" || out.Workspaces[0].TargetID != "target-alpha" || out.Workspaces[0].Status != "active" {
		t.Fatalf("unexpected workspace fields: %+v", out.Workspaces[0])
	}
}

func TestListWorkspaces_ValidToken_ReturnsWorkspaceMetadata(t *testing.T) {
	srv, _, store := newTestServer(t)

	target := &registry.Target{ID: "target-meta", Name: "target-meta", Kind: registry.TargetKindLocal}
	if err := store.CreateTarget(context.Background(), target); err != nil {
		t.Fatalf("CreateTarget: %v", err)
	}

	lastUsed := time.Now().UTC().Truncate(time.Millisecond)
	ws := &registry.Workspace{
		ID:             "ws-meta",
		Name:           "meta",
		Path:           "/fixture/meta",
		TargetID:       target.ID,
		Status:         registry.WorkspaceStatusIdle,
		Tags:           []string{"go", "api"},
		Description:    "metadata workspace",
		Capabilities:   []string{"mcp-git", "mcp-filesystem"},
		RollingSummary: "last did some work",
		IsDynamic:      true,
		LastUsedAt:     &lastUsed,
	}
	if err := store.CreateWorkspace(context.Background(), ws); err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}

	// A second workspace with no optional metadata proves last_used_at is
	// omitted when null and empty slices/strings are returned as their zero
	// values.
	createTestWorkspace(t, store, "plain", registry.WorkspaceStatusActive)

	token, status := login(t, srv.URL, testPassword)
	if status != http.StatusOK {
		t.Fatalf("login status = %d", status)
	}

	resp := authedRequest(t, http.MethodGet, srv.URL+"/api/v1/workspaces", token, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	var out struct {
		Workspaces []struct {
			ID             string     `json:"id"`
			Name           string     `json:"name"`
			TargetID       string     `json:"target_id"`
			Status         string     `json:"status"`
			Tags           []string   `json:"tags"`
			Description    string     `json:"description"`
			RollingSummary string     `json:"rolling_summary"`
			IsDynamic      *bool      `json:"is_dynamic"`
			LastUsedAt     *time.Time `json:"last_used_at"`
		} `json:"workspaces"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Workspaces) != 2 {
		t.Fatalf("got %d workspaces, want 2", len(out.Workspaces))
	}

	meta := out.Workspaces[0]
	if meta.ID != "ws-meta" || meta.Name != "meta" || meta.TargetID != target.ID || meta.Status != "idle" {
		t.Fatalf("unexpected metadata workspace identity: %+v", meta)
	}
	if !sliceEq(meta.Tags, []string{"go", "api"}) {
		t.Fatalf("tags = %v, want [go api]", meta.Tags)
	}
	if meta.Description != "metadata workspace" {
		t.Fatalf("description = %q, want %q", meta.Description, "metadata workspace")
	}
	if meta.RollingSummary != "last did some work" {
		t.Fatalf("rolling_summary = %q, want %q", meta.RollingSummary, "last did some work")
	}
	// is_dynamic is no longer part of the API (freeze review item 2):
	// every workspace is dynamic.
	if meta.IsDynamic != nil {
		t.Fatalf("is_dynamic = %v, want it gone", *meta.IsDynamic)
	}
	if meta.LastUsedAt == nil || !meta.LastUsedAt.Equal(lastUsed) {
		t.Fatalf("last_used_at = %v, want %v", meta.LastUsedAt, lastUsed)
	}

	plain := out.Workspaces[1]
	if plain.ID != "ws-plain" {
		t.Fatalf("unexpected plain workspace identity: %+v", plain)
	}
	if plain.LastUsedAt != nil {
		t.Fatalf("plain.last_used_at = %v, want nil (omitted when null)", plain.LastUsedAt)
	}
	if len(plain.Tags) != 0 || plain.Description != "" || plain.RollingSummary != "" {
		t.Fatalf("plain workspace has unexpected metadata: %+v", plain)
	}

	// last_used_at must actually be omitted from the JSON when null, not
	// serialized as null, so a strict client that distinguishes absent vs
	// null fields stays compatible.
	var raw struct {
		Workspaces []map[string]any `json:"workspaces"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("raw decode: %v", err)
	}
	if _, ok := raw.Workspaces[1]["last_used_at"]; ok {
		t.Fatal("plain workspace JSON contains last_used_at; want it omitted when null")
	}

	// tags must serialize as [] even when empty — the contract says they
	// are always present, never null. capabilities is no longer part of
	// the API (freeze review: nothing ever filled it).
	metaTags, ok := raw.Workspaces[0]["tags"].([]any)
	if !ok {
		t.Fatalf("meta workspace tags raw type = %T, want []", raw.Workspaces[0]["tags"])
	}
	if len(metaTags) != 2 || metaTags[0] != "go" || metaTags[1] != "api" {
		t.Fatalf("meta workspace tags raw value = %v, want [go api]", metaTags)
	}
	plainTags, ok := raw.Workspaces[1]["tags"].([]any)
	if !ok {
		t.Fatalf("plain workspace tags raw type = %T, want []", raw.Workspaces[1]["tags"])
	}
	if len(plainTags) != 0 {
		t.Fatalf("plain workspace tags raw value = %v, want []", plainTags)
	}
	if _, ok := raw.Workspaces[0]["capabilities"]; ok {
		t.Fatal("workspace JSON has capabilities; want it gone")
	}
}

func sliceEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestListWorkspaces_NoWorkspaces_ReturnsEmptyList(t *testing.T) {
	srv, _, _ := newTestServer(t)
	token, _ := login(t, srv.URL, testPassword)

	resp := authedRequest(t, http.MethodGet, srv.URL+"/api/v1/workspaces", token, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	var out struct {
		Workspaces []struct{} `json:"workspaces"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Workspaces == nil {
		t.Fatal("workspaces field decoded as null, want an empty array")
	}
	if len(out.Workspaces) != 0 {
		t.Fatalf("got %d workspaces, want 0", len(out.Workspaces))
	}
}

func TestListConversations_NoToken_ReturnsUnauthorized(t *testing.T) {
	srv, _, _ := newTestServer(t)
	resp := authedRequest(t, http.MethodGet, srv.URL+"/api/v1/conversations", "", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

type conversationSummary struct {
	ConversationID string `json:"conversation_id"`
	WorkspaceID    string `json:"workspace_id"`
	Status         string `json:"status"`
	UpdatedAt      string `json:"updated_at"`
	Preview        string `json:"preview"`
}

func TestListConversations_ValidToken_ReturnsOneSummaryPerConversationSortedByRecency(t *testing.T) {
	srv, _, store := newTestServer(t)
	ws := createTestWorkspace(t, store, "ws1", registry.WorkspaceStatusIdle)

	// conv-old is created first, then conv-new; but conv-old's task is
	// updated last, so recency-sorting must reflect the update, not
	// creation order.
	createTestTask(t, store, "task-old-1", ws.ID, "conv-old", registry.TaskStatusRunning)
	createTestMessage(t, store, "msg-old-1", "conv-old", "task-old-1", registry.MessageRoleUser, "first message in conv-old")
	time.Sleep(10 * time.Millisecond)
	createTestTask(t, store, "task-new-1", ws.ID, "conv-new", registry.TaskStatusRunning)
	createTestMessage(t, store, "msg-new-1", "conv-new", "task-new-1", registry.MessageRoleUser, "first message in conv-new")
	time.Sleep(10 * time.Millisecond)

	taskOld1, err := store.GetTask(context.Background(), "task-old-1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	taskOld1.Status = registry.TaskStatusCompleted
	if err := store.UpdateTask(context.Background(), taskOld1); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}

	token, status := login(t, srv.URL, testPassword)
	if status != http.StatusOK {
		t.Fatalf("login status = %d", status)
	}

	resp := authedRequest(t, http.MethodGet, srv.URL+"/api/v1/conversations", token, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	var out struct {
		Conversations []conversationSummary `json:"conversations"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Conversations) != 2 {
		t.Fatalf("got %d conversations, want 2", len(out.Conversations))
	}
	if out.Conversations[0].ConversationID != "conv-old" || out.Conversations[1].ConversationID != "conv-new" {
		t.Fatalf("order = [%q, %q], want [conv-old, conv-new] (most-recently-updated first)",
			out.Conversations[0].ConversationID, out.Conversations[1].ConversationID)
	}
	if out.Conversations[0].Status != "completed" || out.Conversations[0].WorkspaceID != ws.ID {
		t.Fatalf("conv-old summary = %+v, want status=completed workspace_id=%s", out.Conversations[0], ws.ID)
	}
	if out.Conversations[1].Status != "running" {
		t.Fatalf("conv-new summary = %+v, want status=running", out.Conversations[1])
	}
	if out.Conversations[0].Preview != "first message in conv-old" {
		t.Fatalf("conv-old preview = %q, want %q", out.Conversations[0].Preview, "first message in conv-old")
	}
	if out.Conversations[1].Preview != "first message in conv-new" {
		t.Fatalf("conv-new preview = %q, want %q", out.Conversations[1].Preview, "first message in conv-new")
	}
}

// TestListConversations_Preview_TruncatesLongFirstMessageAndUsesEarliestNotLatest
// proves the preview (LOOM-45) is the conversation's very first message
// — not its most recent one, which the rest of the summary row already
// reflects via status/updated_at — and that a long first message is
// truncated rather than bloating the listing response.
func TestListConversations_Preview_TruncatesLongFirstMessageAndUsesEarliestNotLatest(t *testing.T) {
	srv, _, store := newTestServer(t)
	ws := createTestWorkspace(t, store, "ws1", registry.WorkspaceStatusIdle)
	createTestTask(t, store, "task-1", ws.ID, "conv-1", registry.TaskStatusRunning)

	long := strings.Repeat("x", 250)
	createTestMessage(t, store, "msg-1", "conv-1", "task-1", registry.MessageRoleUser, long)
	createTestMessage(t, store, "msg-2", "conv-1", "task-1", registry.MessageRoleAssistant, "a much later, different reply")

	token, _ := login(t, srv.URL, testPassword)
	resp := authedRequest(t, http.MethodGet, srv.URL+"/api/v1/conversations", token, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	var out struct {
		Conversations []conversationSummary `json:"conversations"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Conversations) != 1 {
		t.Fatalf("got %d conversations, want 1", len(out.Conversations))
	}
	want := strings.Repeat("x", 200) + "…"
	if out.Conversations[0].Preview != want {
		t.Fatalf("preview = %q, want %q (truncated to 200 runes)", out.Conversations[0].Preview, want)
	}
}

// listConversations is the GET /conversations round trip the LOOM-62
// tests share.
func listConversations(t *testing.T, srvURL string) []conversationSummary {
	t.Helper()
	token, _ := login(t, srvURL, testPassword)
	resp := authedRequest(t, http.MethodGet, srvURL+"/api/v1/conversations", token, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	var out struct {
		Conversations []conversationSummary `json:"conversations"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out.Conversations
}

// TestListConversations_MessagesOnlyNoTasks_IsListed: a conversation made
// only of answer_directly turns has messages but no tasks (LOOM-62). It
// is listed as completed — nothing is running or waiting on the user —
// with no workspace, its first message as preview, and its last message
// as updated_at.
func TestListConversations_MessagesOnlyNoTasks_IsListed(t *testing.T) {
	srv, _, store := newTestServer(t)
	createTestMessage(t, store, "m1", "conv-direct-only", "", registry.MessageRoleUser, "what's up")
	time.Sleep(10 * time.Millisecond)
	createTestMessage(t, store, "m2", "conv-direct-only", "", registry.MessageRoleAssistant, "not much")

	msgs, err := store.ListMessagesByConversation(context.Background(), "conv-direct-only")
	if err != nil {
		t.Fatalf("ListMessagesByConversation: %v", err)
	}

	got := listConversations(t, srv.URL)
	if len(got) != 1 {
		t.Fatalf("got %d conversations, want 1: %+v", len(got), got)
	}
	c := got[0]
	if c.ConversationID != "conv-direct-only" || c.Preview != "what's up" || c.Status != "completed" || c.WorkspaceID != "" {
		t.Fatalf("summary = %+v, want conv-direct-only, preview %q, status completed, no workspace", c, "what's up")
	}
	updatedAt, err := time.Parse(time.RFC3339Nano, c.UpdatedAt)
	if err != nil {
		t.Fatalf("updated_at %q: %v", c.UpdatedAt, err)
	}
	if !updatedAt.Equal(msgs[1].CreatedAt) {
		t.Fatalf("updated_at = %v, want the last message's created_at %v", c.UpdatedAt, msgs[1].CreatedAt)
	}
}

// TestListConversations_RecencyCountsMessagesAsWellAsTasks: ordering is by
// a conversation's latest activity, task update or logged message
// alike — so a direct-answer-only conversation, and a task conversation
// that later got a direct answer, both sort by their newest turn.
func TestListConversations_RecencyCountsMessagesAsWellAsTasks(t *testing.T) {
	srv, _, store := newTestServer(t)
	ws := createTestWorkspace(t, store, "ws1", registry.WorkspaceStatusIdle)

	createTestTask(t, store, "task-a", ws.ID, "conv-task", registry.TaskStatusCompleted)
	createTestMessage(t, store, "a1", "conv-task", "task-a", registry.MessageRoleUser, "task work")
	time.Sleep(10 * time.Millisecond)
	createTestMessage(t, store, "b1", "conv-direct", "", registry.MessageRoleUser, "quick question")
	time.Sleep(10 * time.Millisecond)

	got := listConversations(t, srv.URL)
	if len(got) != 2 || got[0].ConversationID != "conv-direct" || got[1].ConversationID != "conv-task" {
		t.Fatalf("order = %+v, want [conv-direct, conv-task]", got)
	}

	// A later direct-answer turn in the task conversation (no new task
	// row) must move it back to the top, keeping its task's status and
	// workspace.
	createTestMessage(t, store, "a2", "conv-task", "", registry.MessageRoleUser, "follow-up answered directly")
	got = listConversations(t, srv.URL)
	if len(got) != 2 || got[0].ConversationID != "conv-task" {
		t.Fatalf("order after follow-up = %+v, want conv-task first", got)
	}
	if got[0].Status != "completed" || got[0].WorkspaceID != ws.ID || got[0].Preview != "task work" {
		t.Fatalf("conv-task summary = %+v, want its task's status/workspace and its first message as preview", got[0])
	}
}

func TestListConversations_None_ReturnsEmptyList(t *testing.T) {
	srv, _, _ := newTestServer(t)
	token, _ := login(t, srv.URL, testPassword)

	resp := authedRequest(t, http.MethodGet, srv.URL+"/api/v1/conversations", token, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	var out struct {
		Conversations []conversationSummary `json:"conversations"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Conversations == nil {
		t.Fatal("conversations field decoded as null, want an empty array")
	}
	if len(out.Conversations) != 0 {
		t.Fatalf("got %d conversations, want 0", len(out.Conversations))
	}
}

func TestGetConversation_NoToken_ReturnsUnauthorized(t *testing.T) {
	srv, _, _ := newTestServer(t)
	resp := authedRequest(t, http.MethodGet, srv.URL+"/api/v1/conversations/conv-1", "", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

func TestGetConversation_Unknown_ReturnsNotFound(t *testing.T) {
	srv, _, _ := newTestServer(t)
	token, _ := login(t, srv.URL, testPassword)

	resp := authedRequest(t, http.MethodGet, srv.URL+"/api/v1/conversations/no-such-conversation", token, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}

func TestGetConversation_ValidToken_ReturnsChronologicalTaskHistory(t *testing.T) {
	srv, _, store := newTestServer(t)
	wsA := createTestWorkspace(t, store, "ws-a", registry.WorkspaceStatusIdle)
	wsB := createTestWorkspace(t, store, "ws-b", registry.WorkspaceStatusIdle)

	// The same conversation spans two workspaces over time (LOOM-13
	// continuation / a later message routed elsewhere) — history must
	// include both tasks, oldest first.
	createTestTask(t, store, "task-1", wsA.ID, "conv-multi", registry.TaskStatusCompleted)
	time.Sleep(10 * time.Millisecond)
	createTestTask(t, store, "task-2", wsB.ID, "conv-multi", registry.TaskStatusRunning)
	// An unrelated conversation must not leak into this one's history.
	createTestTask(t, store, "task-other", wsA.ID, "conv-other", registry.TaskStatusRunning)

	token, _ := login(t, srv.URL, testPassword)
	resp := authedRequest(t, http.MethodGet, srv.URL+"/api/v1/conversations/conv-multi", token, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	var out struct {
		ConversationID string `json:"conversation_id"`
		Tasks          []struct {
			ID          string `json:"id"`
			WorkspaceID string `json:"workspace_id"`
			Status      string `json:"status"`
		} `json:"tasks"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.ConversationID != "conv-multi" {
		t.Fatalf("conversation_id = %q, want conv-multi", out.ConversationID)
	}
	if len(out.Tasks) != 2 {
		t.Fatalf("got %d tasks, want 2", len(out.Tasks))
	}
	if out.Tasks[0].ID != "task-1" || out.Tasks[1].ID != "task-2" {
		t.Fatalf("task order = [%q, %q], want [task-1, task-2] (chronological)", out.Tasks[0].ID, out.Tasks[1].ID)
	}
	if out.Tasks[0].WorkspaceID != wsA.ID || out.Tasks[1].WorkspaceID != wsB.ID {
		t.Fatalf("workspace ids = [%q, %q], want [%q, %q]", out.Tasks[0].WorkspaceID, out.Tasks[1].WorkspaceID, wsA.ID, wsB.ID)
	}
}

func TestGetConversation_ValidToken_IncludesMessagesOldestFirst(t *testing.T) {
	srv, _, store := newTestServer(t)
	ws := createTestWorkspace(t, store, "ws-a", registry.WorkspaceStatusIdle)
	task := createTestTask(t, store, "task-1", ws.ID, "conv-msgs", registry.TaskStatusCompleted)
	createTestMessage(t, store, "m1", "conv-msgs", task.ID, registry.MessageRoleUser, "hi there")
	createTestMessage(t, store, "m2", "conv-msgs", task.ID, registry.MessageRoleAssistant, "hello!")
	// An unrelated conversation must not leak in.
	createTestMessage(t, store, "m-other", "conv-other", "", registry.MessageRoleUser, "unrelated")

	token, _ := login(t, srv.URL, testPassword)
	resp := authedRequest(t, http.MethodGet, srv.URL+"/api/v1/conversations/conv-msgs", token, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	var out struct {
		Messages []struct {
			ID      string `json:"id"`
			Role    string `json:"role"`
			Content string `json:"content"`
			TaskID  string `json:"task_id"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Messages) != 2 {
		t.Fatalf("got %d messages, want 2: %+v", len(out.Messages), out.Messages)
	}
	if out.Messages[0].ID != "m1" || out.Messages[0].Role != "user" || out.Messages[0].Content != "hi there" || out.Messages[0].TaskID != task.ID {
		t.Fatalf("messages[0] = %+v, want the user message first", out.Messages[0])
	}
	if out.Messages[1].ID != "m2" || out.Messages[1].Role != "assistant" || out.Messages[1].Content != "hello!" {
		t.Fatalf("messages[1] = %+v, want the assistant message second", out.Messages[1])
	}
}

func TestGetConversation_MessagesOnlyNoTasks_ReturnsOK(t *testing.T) {
	srv, _, store := newTestServer(t)
	createTestMessage(t, store, "m1", "conv-direct-only", "", registry.MessageRoleUser, "what's up")
	createTestMessage(t, store, "m2", "conv-direct-only", "", registry.MessageRoleAssistant, "not much")

	token, _ := login(t, srv.URL, testPassword)
	resp := authedRequest(t, http.MethodGet, srv.URL+"/api/v1/conversations/conv-direct-only", token, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d (a message-only, task-less conversation must not 404)", resp.StatusCode, http.StatusOK)
	}

	var out struct {
		Tasks    []any `json:"tasks"`
		Messages []any `json:"messages"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Tasks) != 0 {
		t.Fatalf("got %d tasks, want 0", len(out.Tasks))
	}
	if len(out.Messages) != 2 {
		t.Fatalf("got %d messages, want 2", len(out.Messages))
	}
}

func TestAttachInfo_NoToken_ReturnsUnauthorized(t *testing.T) {
	srv, _, _ := newTestServer(t)
	resp := authedRequest(t, http.MethodGet, srv.URL+"/api/v1/tasks/task-1/attach-info", "", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

func TestAttachInfo_UnknownTask_ReturnsNotFound(t *testing.T) {
	srv, _, _ := newTestServer(t)
	token, _ := login(t, srv.URL, testPassword)

	resp := authedRequest(t, http.MethodGet, srv.URL+"/api/v1/tasks/no-such-task/attach-info", token, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}

func TestAttachInfo_ValidToken_ReturnsTargetAndSessionInfo(t *testing.T) {
	srv, _, store := newTestServer(t)

	target := &registry.Target{
		ID: "remote-1", Name: "remote-1", Kind: registry.TargetKindRemote,
		Host: "jet01.example.com", User: "orski",
	}
	if err := store.CreateTarget(context.Background(), target); err != nil {
		t.Fatalf("CreateTarget: %v", err)
	}
	ws := &registry.Workspace{
		ID: "ws-remote", Name: "ws-remote", Path: "/srv/app", TargetID: target.ID,
		Status: registry.WorkspaceStatusActive,
	}
	if err := store.CreateWorkspace(context.Background(), ws); err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	task := &registry.Task{
		ID: "task-attach", WorkspaceID: ws.ID, Kind: registry.TaskKindAgent, AgentType: "claude-code",
		TmuxSession: "loomux-task-attach", Status: registry.TaskStatusRunning, ConversationID: "conv-attach",
	}
	if err := store.CreateTask(context.Background(), task); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	token, _ := login(t, srv.URL, testPassword)
	resp := authedRequest(t, http.MethodGet, srv.URL+"/api/v1/tasks/task-attach/attach-info", token, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	var out struct {
		TaskID      string `json:"task_id"`
		TmuxSession string `json:"tmux_session"`
		Target      struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Kind string `json:"kind"`
			Host string `json:"host"`
			User string `json:"user"`
		} `json:"target"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.TaskID != "task-attach" || out.TmuxSession != "loomux-task-attach" {
		t.Fatalf("task_id/tmux_session = %q/%q, want task-attach/loomux-task-attach", out.TaskID, out.TmuxSession)
	}
	if out.Target.ID != "remote-1" || out.Target.Kind != "remote" || out.Target.Host != "jet01.example.com" || out.Target.User != "orski" {
		t.Fatalf("target = %+v, want {id:remote-1 kind:remote host:jet01.example.com user:orski}", out.Target)
	}
}

// sseEvent is one parsed Server-Sent Event: its "event:" line and the
// concatenated body of its "data:" line(s).
type sseEvent struct {
	Event string
	Data  string
}

// readSSEEvent reads lines from r until one complete SSE event has been
// assembled (a blank line terminates it), skipping heartbeat comment
// lines (starting with ':'). Blocks until an event arrives, the reader
// errors (e.g. the connection closes), or eventCh's caller times out.
func readSSEEvent(t *testing.T, r *bufio.Reader) (sseEvent, error) {
	t.Helper()
	var ev sseEvent
	var data []string
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return sseEvent{}, err
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if ev.Event != "" || len(data) > 0 {
				ev.Data = strings.Join(data, "\n")
				return ev, nil
			}
			// Blank line with nothing accumulated yet: keep reading.
		case strings.HasPrefix(line, ":"):
			// Heartbeat comment — ignore.
		case strings.HasPrefix(line, "event:"):
			ev.Event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
}

// readSSEEventWithTimeout runs readSSEEvent on a goroutine and fails the
// test if no complete event arrives within d — so a broken stream hangs
// the test for at most d, not forever.
func readSSEEventWithTimeout(t *testing.T, r *bufio.Reader, d time.Duration) sseEvent {
	t.Helper()
	type result struct {
		ev  sseEvent
		err error
	}
	ch := make(chan result, 1)
	go func() {
		ev, err := readSSEEvent(t, r)
		ch <- result{ev, err}
	}()
	select {
	case res := <-ch:
		if res.err != nil {
			t.Fatalf("readSSEEvent: %v", res.err)
		}
		return res.ev
	case <-time.After(d):
		t.Fatal("timed out waiting for SSE event")
		return sseEvent{}
	}
}

func TestStream_NoToken_ReturnsUnauthorized(t *testing.T) {
	srv, _, _ := newTestServer(t)
	resp := authedRequest(t, http.MethodGet, srv.URL+"/api/v1/conversations/conv-1/stream", "", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

func TestStream_EmitsUpdatesAsTaskStatusChanges(t *testing.T) {
	srv, _, store := newTestServer(t, api.WithStreamPollInterval(10*time.Millisecond))
	ws := createTestWorkspace(t, store, "ws-stream", registry.WorkspaceStatusIdle)
	token, _ := login(t, srv.URL, testPassword)

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/conversations/conv-stream/stream", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	reader := bufio.NewReader(resp.Body)

	// No task exists yet for this conversation — create one and expect
	// an event describing it.
	task := createTestTask(t, store, "task-stream-1", ws.ID, "conv-stream", registry.TaskStatusRunning)

	ev := readSSEEventWithTimeout(t, reader, 2*time.Second)
	if ev.Event != "task_update" {
		t.Fatalf("event type = %q, want task_update", ev.Event)
	}
	var payload struct {
		TaskID      string `json:"task_id"`
		WorkspaceID string `json:"workspace_id"`
		Status      string `json:"status"`
	}
	if err := json.Unmarshal([]byte(ev.Data), &payload); err != nil {
		t.Fatalf("unmarshal event data %q: %v", ev.Data, err)
	}
	if payload.TaskID != task.ID || payload.WorkspaceID != ws.ID || payload.Status != "running" {
		t.Fatalf("event payload = %+v, want {task_id:%s workspace_id:%s status:running}", payload, task.ID, ws.ID)
	}

	// Now transition the task's status — expect a second event
	// reflecting the change.
	got, err := store.GetTask(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	got.Status = registry.TaskStatusCompleted
	if err := store.UpdateTask(context.Background(), got); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}

	ev2 := readSSEEventWithTimeout(t, reader, 2*time.Second)
	var payload2 struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(ev2.Data), &payload2); err != nil {
		t.Fatalf("unmarshal event data %q: %v", ev2.Data, err)
	}
	if payload2.Status != "completed" {
		t.Fatalf("second event status = %q, want completed", payload2.Status)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return data
}

func TestStaticFileServing(t *testing.T) {
	dir := t.TempDir()
	writeFile := func(rel, content string) {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", rel, err)
		}
	}
	writeFile("index.html", "<html>spa shell</html>")
	writeFile("assets/app.js", "console.log('hi')")

	httpSrv, _, _ := newTestServer(t, api.WithStaticDir(dir))

	get := func(t *testing.T, path string) (*http.Response, string) {
		t.Helper()
		resp, err := http.Get(httpSrv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		return resp, string(body)
	}

	t.Run("serves a real static file as-is", func(t *testing.T) {
		resp, body := get(t, "/assets/app.js")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if body != "console.log('hi')" {
			t.Errorf("body = %q, want the raw asset content", body)
		}
	})

	t.Run("unknown client-side route falls back to index.html", func(t *testing.T) {
		resp, body := get(t, "/conversations/abc123")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200 (SPA fallback)", resp.StatusCode)
		}
		if body != "<html>spa shell</html>" {
			t.Errorf("body = %q, want index.html content (SPA fallback)", body)
		}
	})

	// LOOM-158: a missing file the page asked for is a 404, not the SPA
	// shell; only a path that can be a client-side route falls back.
	for _, p := range []string{"/assets/app-old.js", "/assets/gone", "/assets/", "/favicon.ico", "/conversations/x/app.css"} {
		t.Run("missing file "+p+" is 404", func(t *testing.T) {
			resp, body := get(t, p)
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", resp.StatusCode)
			}
			if strings.Contains(body, "spa shell") {
				t.Errorf("body = %q, want no SPA shell", body)
			}
		})
	}

	t.Run("root path serves index.html", func(t *testing.T) {
		resp, body := get(t, "/")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if body != "<html>spa shell</html>" {
			t.Errorf("body = %q, want index.html content", body)
		}
	})

	t.Run("api paths are unaffected by static serving", func(t *testing.T) {
		resp, body := get(t, "/api/v1/version")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if strings.Contains(body, "spa shell") {
			t.Errorf("body = %q, want the JSON version response, not static content", body)
		}
	})

	t.Run("unsupported api version is still rejected, not served as static", func(t *testing.T) {
		resp, body := get(t, "/api/v2/whatever")
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}
		if strings.Contains(body, "spa shell") {
			t.Errorf("body = %q, want the unsupported-version JSON response, not static content", body)
		}
	})

	// Regression test: ServeHTTP's routing check used to be a bare
	// strings.HasPrefix(r.URL.Path, "/api/") (trailing slash required),
	// so the exact path "/api" (no trailing slash) didn't match it and
	// fell through to the static handler — returning the SPA shell (200)
	// for a path that should be treated as an API path, exactly like
	// "/api/v2/whatever" above.
	t.Run("bare /api path (no trailing slash) is still treated as an API path, not static", func(t *testing.T) {
		resp, body := get(t, "/api")
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 (unsupported API path)", resp.StatusCode)
		}
		if strings.Contains(body, "spa shell") {
			t.Errorf("body = %q, want the unsupported-API-path JSON response, not static content", body)
		}
	})

	t.Run("path traversal falls back to the SPA shell, never escapes the static dir", func(t *testing.T) {
		resp, body := get(t, "/../../etc/passwd")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200 (SPA fallback)", resp.StatusCode)
		}
		if body != "<html>spa shell</html>" {
			t.Errorf("body = %q, want index.html content (SPA fallback), not an escaped file", body)
		}
	})

	t.Run("url-encoded path traversal also falls back to the SPA shell", func(t *testing.T) {
		resp, body := get(t, "/%2e%2e/%2e%2e/etc/passwd")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200 (SPA fallback)", resp.StatusCode)
		}
		if body != "<html>spa shell</html>" {
			t.Errorf("body = %q, want index.html content (SPA fallback), not an escaped file", body)
		}
	})
}

func TestStaticFileServing_DisabledByDefault(t *testing.T) {
	httpSrv, _, _ := newTestServer(t)

	resp, err := http.Get(httpSrv.URL + "/conversations/abc123")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (static serving not configured)", resp.StatusCode)
	}
}

// After a deploy, a browser must not keep an old index.html pointing at
// asset hashes that no longer exist; the hashed assets themselves never
// change, so they can be cached for good.
func TestStaticFileServing_CacheHeaders(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html></html>"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "assets", "app-abc123.js"), []byte("x"), 0o644)
	httpSrv, _, _ := newTestServer(t, api.WithStaticDir(dir))
	for path, want := range map[string]string{
		"/":                     "no-cache",
		"/index.html":           "no-cache",
		"/conversations/abc":    "no-cache",
		"/assets/app-abc123.js": "public, max-age=31536000, immutable",
	} {
		resp, err := http.Get(httpSrv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if got := resp.Header.Get("Cache-Control"); got != want {
			t.Errorf("GET %s Cache-Control = %q, want %q", path, got, want)
		}
	}
}
