package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/Loomux/server/api"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/registry/sqlite"
)

const testPassword = "correct-horse-battery-staple"

type fakeDispatcher struct {
	DispatchFunc func(ctx context.Context, conversationID, message string) (string, error)
}

func (f *fakeDispatcher) Dispatch(ctx context.Context, conversationID, message string) (string, error) {
	return f.DispatchFunc(ctx, conversationID, message)
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

func newTestServer(t *testing.T, opts ...api.Option) (*httptest.Server, *fakeDispatcher, registry.Store) {
	t.Helper()
	hash, err := api.HashPassword(testPassword)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	dispatcher := &fakeDispatcher{}
	store := newTestStore(t)
	server := api.NewServer(dispatcher, store, store, []byte(hash), opts...)
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
	resp := authedRequest(t, http.MethodPost, srv.URL+"/api/v1/dispatch", "", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

func TestDispatch_InvalidToken_ReturnsUnauthorized(t *testing.T) {
	srv, _, _ := newTestServer(t)
	body, _ := json.Marshal(map[string]string{"conversation_id": "c1", "message": "hi"})
	resp := authedRequest(t, http.MethodPost, srv.URL+"/api/v1/dispatch", "not-a-real-token", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

func TestDispatch_ValidToken_CallsDispatcherAndReturnsReply(t *testing.T) {
	srv, dispatcher, _ := newTestServer(t)
	dispatcher.DispatchFunc = func(ctx context.Context, conversationID, message string) (string, error) {
		if conversationID != "c1" || message != "hello" {
			t.Fatalf("Dispatch called with (%q, %q), want (c1, hello)", conversationID, message)
		}
		return "the reply", nil
	}

	token, status := login(t, srv.URL, testPassword)
	if status != http.StatusOK {
		t.Fatalf("login status = %d", status)
	}

	body, _ := json.Marshal(map[string]string{"conversation_id": "c1", "message": "hello"})
	resp := authedRequest(t, http.MethodPost, srv.URL+"/api/v1/dispatch", token, body)
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

func TestDispatch_MissingFields_ReturnsBadRequest(t *testing.T) {
	srv, _, _ := newTestServer(t)
	token, _ := login(t, srv.URL, testPassword)

	body, _ := json.Marshal(map[string]string{"conversation_id": "", "message": ""})
	resp := authedRequest(t, http.MethodPost, srv.URL+"/api/v1/dispatch", token, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
}

func TestLogout_RevokesToken(t *testing.T) {
	srv, dispatcher, _ := newTestServer(t)
	dispatcher.DispatchFunc = func(ctx context.Context, conversationID, message string) (string, error) {
		return "ok", nil
	}
	token, _ := login(t, srv.URL, testPassword)

	logoutResp := authedRequest(t, http.MethodPost, srv.URL+"/api/v1/logout", token, nil)
	defer logoutResp.Body.Close()
	if logoutResp.StatusCode != http.StatusNoContent {
		t.Fatalf("logout status = %d, want %d", logoutResp.StatusCode, http.StatusNoContent)
	}

	body, _ := json.Marshal(map[string]string{"conversation_id": "c1", "message": "hi"})
	resp := authedRequest(t, http.MethodPost, srv.URL+"/api/v1/dispatch", token, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("dispatch after logout status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

func TestSession_ExpiresAfterTTL(t *testing.T) {
	srv, dispatcher, _ := newTestServer(t, api.WithSessionTTL(50*time.Millisecond))
	dispatcher.DispatchFunc = func(ctx context.Context, conversationID, message string) (string, error) {
		return "ok", nil
	}
	token, _ := login(t, srv.URL, testPassword)

	time.Sleep(100 * time.Millisecond)

	body, _ := json.Marshal(map[string]string{"conversation_id": "c1", "message": "hi"})
	resp := authedRequest(t, http.MethodPost, srv.URL+"/api/v1/dispatch", token, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status after TTL expiry = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

func TestSession_SlidingExpiration_ActivityExtendsSession(t *testing.T) {
	srv, dispatcher, _ := newTestServer(t, api.WithSessionTTL(150*time.Millisecond))
	dispatcher.DispatchFunc = func(ctx context.Context, conversationID, message string) (string, error) {
		return "ok", nil
	}
	token, _ := login(t, srv.URL, testPassword)
	body, _ := json.Marshal(map[string]string{"conversation_id": "c1", "message": "hi"})

	// Two requests spaced less than the TTL apart, each one should reset
	// the clock — proving activity keeps the session alive well past
	// what a single fixed expiration from login time would allow.
	for i := 0; i < 3; i++ {
		time.Sleep(80 * time.Millisecond)
		resp := authedRequest(t, http.MethodPost, srv.URL+"/api/v1/dispatch", token, body)
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

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return data
}
