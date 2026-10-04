package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Loomux/server/api"
	"github.com/Loomux/server/app"
	"github.com/Loomux/server/router"
)

// TestIntegration_Dispatch_NoTargets_ClearAnswerNot500 replays LOOM-68
// through the real stack with an empty target table: the router model
// isn't offered provision_workspace or run_command, and even a model
// that picks provision_workspace anyway (as in the incident, with no
// usable target_id) yields a 200 telling the user to register a target —
// not a 500 carrying raw router text.
func TestIntegration_Dispatch_NoTargets_ClearAnswerNot500(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	llmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(body))
		mu.Unlock()
		resp := map[string]any{
			"id": "chatcmpl-test", "object": "chat.completion", "created": 1, "model": "test-model",
			"choices": []map[string]any{{
				"index": 0, "finish_reason": "tool_calls",
				"message": map[string]any{
					"role": "assistant",
					"tool_calls": []map[string]any{{
						"id": "call_1", "type": "function",
						"function": map[string]any{
							"name": "route_decision",
							"arguments": `{"action":"provision_workspace","agent_type":"claude-code",` +
								`"new_workspace":{"name":"verify","target_id":"sc1","kind":"empty"}}`,
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
	hash, err := api.HashPassword("pw")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	server := api.NewServer(realApp.Dispatches(), realApp.Store(), realApp.Store(), realApp.Store(), realApp.Store(), realApp.Store(), realApp.Store(), []byte(hash), api.WithLoginBackoff(0, time.Second))
	httpSrv := httptest.NewServer(server)
	t.Cleanup(httpSrv.Close)

	token, status := doLogin(t, httpSrv.URL, "pw")
	if status != http.StatusOK {
		t.Fatalf("login status = %d", status)
	}

	var listed struct {
		Targets []json.RawMessage `json:"targets"`
	}
	getJSON(t, httpSrv.URL+"/api/v1/targets", token, &listed)
	if len(listed.Targets) != 0 {
		t.Fatalf("precondition: targets = %s, want an empty table", listed.Targets)
	}

	resp := postJSON(t, httpSrv.URL+"/api/v1/dispatch", token, map[string]string{
		"conversation_id": "c1", "message": "set up a workspace on sc1",
	})
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("dispatch status = %d, want 200; body: %s", resp.StatusCode, raw)
	}
	var out struct {
		Reply string `json:"reply"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode reply: %v (%s)", err, raw)
	}
	if out.Reply != router.NoTargetsReply {
		t.Fatalf("reply = %q, want router.NoTargetsReply", out.Reply)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) == 0 {
		t.Fatal("router model was never called")
	}
	for _, b := range bodies {
		if strings.Contains(b, `"provision_workspace","run_command"`) || strings.Contains(b, `"enum":["answer_directly","use_workspace","provision_workspace"`) {
			t.Errorf("router model was offered target actions with no targets registered: %s", b)
		}
	}
}
