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

func newTestServer(t *testing.T, opts ...api.Option) (*httptest.Server, *fakeDispatcher) {
	t.Helper()
	hash, err := api.HashPassword(testPassword)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	dispatcher := &fakeDispatcher{}
	store := newTestStore(t)
	server := api.NewServer(dispatcher, store, []byte(hash), opts...)
	httpSrv := httptest.NewServer(server)
	t.Cleanup(httpSrv.Close)
	return httpSrv, dispatcher
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
	srv, _ := newTestServer(t)
	token, status := login(t, srv.URL, testPassword)
	if status != http.StatusOK {
		t.Fatalf("login status = %d, want %d", status, http.StatusOK)
	}
	if token == "" {
		t.Fatal("login returned an empty token")
	}
}

func TestLogin_WrongPassword_ReturnsUnauthorized(t *testing.T) {
	srv, _ := newTestServer(t)
	_, status := login(t, srv.URL, "wrong-password")
	if status != http.StatusUnauthorized {
		t.Fatalf("login status = %d, want %d", status, http.StatusUnauthorized)
	}
}

func TestLogin_MalformedBody_ReturnsBadRequest(t *testing.T) {
	srv, _ := newTestServer(t)
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
	srv, _ := newTestServer(t)
	body, _ := json.Marshal(map[string]string{"conversation_id": "c1", "message": "hi"})
	resp := authedRequest(t, http.MethodPost, srv.URL+"/api/v1/dispatch", "", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

func TestDispatch_InvalidToken_ReturnsUnauthorized(t *testing.T) {
	srv, _ := newTestServer(t)
	body, _ := json.Marshal(map[string]string{"conversation_id": "c1", "message": "hi"})
	resp := authedRequest(t, http.MethodPost, srv.URL+"/api/v1/dispatch", "not-a-real-token", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

func TestDispatch_ValidToken_CallsDispatcherAndReturnsReply(t *testing.T) {
	srv, dispatcher := newTestServer(t)
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
	srv, _ := newTestServer(t)
	token, _ := login(t, srv.URL, testPassword)

	body, _ := json.Marshal(map[string]string{"conversation_id": "", "message": ""})
	resp := authedRequest(t, http.MethodPost, srv.URL+"/api/v1/dispatch", token, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
}

func TestLogout_RevokesToken(t *testing.T) {
	srv, dispatcher := newTestServer(t)
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
	srv, dispatcher := newTestServer(t, api.WithSessionTTL(50*time.Millisecond))
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
	srv, dispatcher := newTestServer(t, api.WithSessionTTL(150*time.Millisecond))
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
