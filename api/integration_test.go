package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/Loomux/server/api"
	"github.com/Loomux/server/app"
	"github.com/Loomux/server/router/llmrouter"
)

func routerConfigFor(baseURL string) llmrouter.Config {
	return llmrouter.Config{
		Primary: llmrouter.Tier{BaseURL: baseURL, APIKey: "test-key", Model: "test-model"},
	}
}

// TestIntegration_RealAppBehindAuth proves the full stack end-to-end:
// a real app.App (real sqlite, a real llmrouter.Model hitting an
// httptest.Server standing in for the LLM vendor — same pattern as
// app_test.go) wrapped by a real api.Server, driven purely over real
// HTTP: login with the wrong password fails, the right password
// succeeds and returns a token, an unauthenticated dispatch attempt is
// refused, an authenticated one reaches the real app.App.Dispatch and
// gets its answer back, and logout actually revokes the token.
func TestIntegration_RealAppBehindAuth(t *testing.T) {
	llmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"id": "chatcmpl-test", "object": "chat.completion", "created": 1, "model": "test-model",
			"choices": []map[string]any{{
				"index": 0, "finish_reason": "tool_calls",
				"message": map[string]any{
					"role": "assistant",
					"tool_calls": []map[string]any{{
						"id": "call_1", "type": "function",
						"function": map[string]any{
							"name":      "route_decision",
							"arguments": `{"action":"answer_directly","direct_answer":"hello from the real stack"}`,
						},
					}},
				},
			}},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(llmSrv.Close)

	realApp, err := app.Build(app.Config{
		DBPath: filepath.Join(t.TempDir(), "test.db"),
		Router: routerConfigFor(llmSrv.URL),
	})
	if err != nil {
		t.Fatalf("app.Build: %v", err)
	}
	t.Cleanup(func() {
		if err := realApp.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	hash, err := api.HashPassword("integration-test-password")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	// A zero login backoff (base 0 => wait() is always 0, regardless of
	// real elapsed time between calls) so the deliberate wrong-then-right
	// sequence below isn't itself throttled by production timing
	// (LOOM-15) — this test isn't exercising the throttle, TestLogin_* in
	// server_test.go does that.
	server := api.NewServer(realApp.Dispatches(), realApp.Store(), realApp.Store(), realApp.Store(), realApp.Store(), realApp.Store(), realApp.Store(), []byte(hash), api.WithLoginBackoff(0, time.Second))
	httpSrv := httptest.NewServer(server)
	t.Cleanup(httpSrv.Close)

	// Wrong password: refused.
	if _, status := doLogin(t, httpSrv.URL, "wrong"); status != http.StatusUnauthorized {
		t.Fatalf("login (wrong password) status = %d, want %d", status, http.StatusUnauthorized)
	}

	// Right password: token issued.
	token, status := doLogin(t, httpSrv.URL, "integration-test-password")
	if status != http.StatusOK || token == "" {
		t.Fatalf("login (correct password) status = %d token = %q", status, token)
	}

	// No token: refused.
	unauthedResp := postJSON(t, httpSrv.URL+"/api/v1/dispatch?wait=true", "", map[string]string{"conversation_id": "c1", "message": "hi"})
	defer unauthedResp.Body.Close()
	if unauthedResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("dispatch without token status = %d, want %d", unauthedResp.StatusCode, http.StatusUnauthorized)
	}

	// With token: reaches the real app.App.Dispatch.
	dispatchResp := postJSON(t, httpSrv.URL+"/api/v1/dispatch?wait=true", token, map[string]string{"conversation_id": "c1", "message": "hi"})
	defer dispatchResp.Body.Close()
	if dispatchResp.StatusCode != http.StatusOK {
		t.Fatalf("dispatch status = %d, want %d", dispatchResp.StatusCode, http.StatusOK)
	}
	var out struct {
		Reply string `json:"reply"`
	}
	if err := json.NewDecoder(dispatchResp.Body).Decode(&out); err != nil {
		t.Fatalf("decode dispatch response: %v", err)
	}
	if out.Reply != "hello from the real stack" {
		t.Fatalf("reply = %q, want %q", out.Reply, "hello from the real stack")
	}

	// Logout, then the same token is refused.
	logoutResp := postJSON(t, httpSrv.URL+"/api/v1/logout", token, nil)
	logoutResp.Body.Close()
	if logoutResp.StatusCode != http.StatusNoContent {
		t.Fatalf("logout status = %d, want %d", logoutResp.StatusCode, http.StatusNoContent)
	}
	afterLogoutResp := postJSON(t, httpSrv.URL+"/api/v1/dispatch?wait=true", token, map[string]string{"conversation_id": "c1", "message": "hi"})
	defer afterLogoutResp.Body.Close()
	if afterLogoutResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("dispatch after logout status = %d, want %d", afterLogoutResp.StatusCode, http.StatusUnauthorized)
	}
}

// TestIntegration_DispatchThenConversationDetail_ShowsMessages proves
// LOOM-31 end-to-end: a real POST /dispatch call against the real stack
// (real sqlite, a fake LLM vendor) results in a subsequent
// GET /conversations/{id} response that includes the turn's actual
// message text, not just task-lifecycle rows.
func TestIntegration_DispatchThenConversationDetail_ShowsMessages(t *testing.T) {
	llmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"id": "chatcmpl-test", "object": "chat.completion", "created": 1, "model": "test-model",
			"choices": []map[string]any{{
				"index": 0, "finish_reason": "tool_calls",
				"message": map[string]any{
					"role": "assistant",
					"tool_calls": []map[string]any{{
						"id": "call_1", "type": "function",
						"function": map[string]any{
							"name":      "route_decision",
							"arguments": `{"action":"answer_directly","direct_answer":"the transcript works"}`,
						},
					}},
				},
			}},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(llmSrv.Close)

	realApp, err := app.Build(app.Config{
		DBPath: filepath.Join(t.TempDir(), "test.db"),
		Router: routerConfigFor(llmSrv.URL),
	})
	if err != nil {
		t.Fatalf("app.Build: %v", err)
	}
	t.Cleanup(func() {
		if err := realApp.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	hash, err := api.HashPassword("integration-test-password")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	server := api.NewServer(realApp.Dispatches(), realApp.Store(), realApp.Store(), realApp.Store(), realApp.Store(), realApp.Store(), realApp.Store(), []byte(hash), api.WithLoginBackoff(0, time.Second))
	httpSrv := httptest.NewServer(server)
	t.Cleanup(httpSrv.Close)

	token, status := doLogin(t, httpSrv.URL, "integration-test-password")
	if status != http.StatusOK {
		t.Fatalf("login status = %d, want %d", status, http.StatusOK)
	}

	dispatchResp := postJSON(t, httpSrv.URL+"/api/v1/dispatch?wait=true", token, map[string]string{"conversation_id": "c-transcript", "message": "hello"})
	dispatchResp.Body.Close()
	if dispatchResp.StatusCode != http.StatusOK {
		t.Fatalf("dispatch status = %d, want %d", dispatchResp.StatusCode, http.StatusOK)
	}

	req, err := http.NewRequest(http.MethodGet, httpSrv.URL+"/api/v1/conversations/c-transcript", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	convResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer convResp.Body.Close()
	if convResp.StatusCode != http.StatusOK {
		t.Fatalf("conversation detail status = %d, want %d", convResp.StatusCode, http.StatusOK)
	}

	var out struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(convResp.Body).Decode(&out); err != nil {
		t.Fatalf("decode conversation detail: %v", err)
	}
	if len(out.Messages) != 2 {
		t.Fatalf("got %d messages, want 2: %+v", len(out.Messages), out.Messages)
	}
	if out.Messages[0].Role != "user" || out.Messages[0].Content != "hello" {
		t.Fatalf("messages[0] = %+v, want role=user content=%q", out.Messages[0], "hello")
	}
	if out.Messages[1].Role != "assistant" || out.Messages[1].Content != "the transcript works" {
		t.Fatalf("messages[1] = %+v, want role=assistant content=%q", out.Messages[1], "the transcript works")
	}
}

// TestIntegration_DirectAnswerOnlyConversation_IsListedAndReadable is
// LOOM-62's reproduction: on the live instance a conversation whose turns
// were all answer_directly had its messages stored, yet
// GET /conversations returned {"conversations":[]} — the listing was
// built from task rows, and a direct answer creates none. Both turns go
// through the real stack; the conversation must then be listed (with its
// preview) and its detail must return every message.
func TestIntegration_DirectAnswerOnlyConversation_IsListedAndReadable(t *testing.T) {
	llmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"id": "chatcmpl-test", "object": "chat.completion", "created": 1, "model": "test-model",
			"choices": []map[string]any{{
				"index": 0, "finish_reason": "tool_calls",
				"message": map[string]any{
					"role": "assistant",
					"tool_calls": []map[string]any{{
						"id": "call_1", "type": "function",
						"function": map[string]any{
							"name":      "route_decision",
							"arguments": `{"action":"answer_directly","direct_answer":"a direct answer"}`,
						},
					}},
				},
			}},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(llmSrv.Close)

	realApp, err := app.Build(app.Config{
		DBPath: filepath.Join(t.TempDir(), "test.db"),
		Router: routerConfigFor(llmSrv.URL),
	})
	if err != nil {
		t.Fatalf("app.Build: %v", err)
	}
	t.Cleanup(func() {
		if err := realApp.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	hash, err := api.HashPassword("integration-test-password")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	server := api.NewServer(realApp.Dispatches(), realApp.Store(), realApp.Store(), realApp.Store(), realApp.Store(), realApp.Store(), realApp.Store(), []byte(hash), api.WithLoginBackoff(0, time.Second))
	httpSrv := httptest.NewServer(server)
	t.Cleanup(httpSrv.Close)

	token, status := doLogin(t, httpSrv.URL, "integration-test-password")
	if status != http.StatusOK {
		t.Fatalf("login status = %d, want %d", status, http.StatusOK)
	}

	// Two user turns, both answered directly — the live incident's shape.
	for _, msg := range []string{"first question", "second question"} {
		resp := postJSON(t, httpSrv.URL+"/api/v1/dispatch?wait=true", token, map[string]string{"conversation_id": "c-direct", "message": msg})
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("dispatch %q status = %d, want %d", msg, resp.StatusCode, http.StatusOK)
		}
	}

	var list struct {
		Conversations []struct {
			ConversationID string `json:"conversation_id"`
			Preview        string `json:"preview"`
		} `json:"conversations"`
	}
	getJSON(t, httpSrv.URL+"/api/v1/conversations", token, &list)
	if len(list.Conversations) != 1 || list.Conversations[0].ConversationID != "c-direct" {
		t.Fatalf("GET /conversations = %+v, want exactly c-direct", list.Conversations)
	}
	if list.Conversations[0].Preview != "first question" {
		t.Fatalf("preview = %q, want %q", list.Conversations[0].Preview, "first question")
	}

	var detail struct {
		Tasks    []any `json:"tasks"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	getJSON(t, httpSrv.URL+"/api/v1/conversations/c-direct", token, &detail)
	if len(detail.Tasks) != 0 {
		t.Fatalf("tasks = %+v, want none for a direct-answer-only conversation", detail.Tasks)
	}
	want := []string{"user:first question", "assistant:a direct answer", "user:second question", "assistant:a direct answer"}
	if len(detail.Messages) != len(want) {
		t.Fatalf("got %d messages, want %d: %+v", len(detail.Messages), len(want), detail.Messages)
	}
	for i, m := range detail.Messages {
		if got := m.Role + ":" + m.Content; got != want[i] {
			t.Errorf("messages[%d] = %q, want %q", i, got, want[i])
		}
	}
}

// getJSON GETs url with token and decodes a 200 response into out.
func getJSON(t *testing.T, url, token string, out any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
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
		t.Fatalf("GET %s status = %d, want %d", url, resp.StatusCode, http.StatusOK)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
}

func doLogin(t *testing.T, baseURL, password string) (token string, status int) {
	t.Helper()
	resp := postJSON(t, baseURL+"/api/v1/login", "", map[string]string{"password": password})
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

func postJSON(t *testing.T, url, token string, body any) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode request body: %v", err)
		}
	}
	req, err := http.NewRequest(http.MethodPost, url, &buf)
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
