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
	server := api.NewServer(dispatcher, store, store, store, store, store, []byte(hash), opts...)
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
			Capabilities   []string   `json:"capabilities"`
			RollingSummary string     `json:"rolling_summary"`
			IsDynamic      bool       `json:"is_dynamic"`
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
	if !sliceEq(meta.Capabilities, []string{"mcp-git", "mcp-filesystem"}) {
		t.Fatalf("capabilities = %v, want [mcp-git mcp-filesystem]", meta.Capabilities)
	}
	if meta.RollingSummary != "last did some work" {
		t.Fatalf("rolling_summary = %q, want %q", meta.RollingSummary, "last did some work")
	}
	if !meta.IsDynamic {
		t.Fatal("is_dynamic = false, want true")
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
	if len(plain.Tags) != 0 || len(plain.Capabilities) != 0 || plain.Description != "" || plain.RollingSummary != "" || plain.IsDynamic {
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

	// tags and capabilities must serialize as [] even when empty — the
	// contract says they are always present, never null.
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
	plainCaps, ok := raw.Workspaces[1]["capabilities"].([]any)
	if !ok {
		t.Fatalf("plain workspace capabilities raw type = %T, want []", raw.Workspaces[1]["capabilities"])
	}
	if len(plainCaps) != 0 {
		t.Fatalf("plain workspace capabilities raw value = %v, want []", plainCaps)
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
}

func TestListConversations_ValidToken_ReturnsOneSummaryPerConversationSortedByRecency(t *testing.T) {
	srv, _, store := newTestServer(t)
	ws := createTestWorkspace(t, store, "ws1", registry.WorkspaceStatusIdle)

	// conv-old is created first, then conv-new; but conv-old's task is
	// updated last, so recency-sorting must reflect the update, not
	// creation order.
	createTestTask(t, store, "task-old-1", ws.ID, "conv-old", registry.TaskStatusRunning)
	time.Sleep(10 * time.Millisecond)
	createTestTask(t, store, "task-new-1", ws.ID, "conv-new", registry.TaskStatusRunning)
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
