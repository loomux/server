package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/Loomux/server/completion"
	"github.com/Loomux/server/router/llmrouter"
)

// fakeRouterServer stands in for the router model's primary tier: it
// always answers as if the model called route_decision with
// action=answer_directly, so Dispatch returns without touching the
// orchestrator/tmux at all — proving the composition wires end-to-end
// without needing a real agent CLI or tmux session.
func fakeRouterServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
							"arguments": `{"action":"answer_directly","direct_answer":"hello from app"}`,
						},
					}},
				},
			}},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func testConfig(t *testing.T, routerBaseURL string) Config {
	t.Helper()
	return Config{
		DBPath: filepath.Join(t.TempDir(), "test.db"),
		Router: llmrouter.Config{
			Primary: llmrouter.Tier{BaseURL: routerBaseURL, APIKey: "test-key", Model: "test-model"},
		},
	}
}

func TestBuild_EndToEnd_AnswerDirectly(t *testing.T) {
	srv := fakeRouterServer(t)

	app, err := Build(testConfig(t, srv.URL))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer app.Close()

	reply, err := app.Dispatch(context.Background(), "conv-1", "hi")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if reply != "hello from app" {
		t.Errorf("Dispatch = %q, want %q", reply, "hello from app")
	}
}

func TestBuild_InvalidDBPath_ReturnsError(t *testing.T) {
	cfg := testConfig(t, "http://example.invalid")
	cfg.DBPath = filepath.Join(t.TempDir(), "nonexistent-dir", "sub", "test.db")

	_, err := Build(cfg)
	if err == nil {
		t.Fatal("Build: want error for a DB path in a nonexistent directory, got nil")
	}
}

func TestBuild_RouterModelConstructionFails(t *testing.T) {
	cfg := testConfig(t, "http://example.invalid")
	cfg.Router.Primary.BaseURL = ""
	cfg.Router.Primary.APIKey = ""
	cfg.Router.Primary.Model = ""

	_, err := Build(cfg)
	if err == nil {
		t.Fatal("Build: want error for incomplete router config, got nil")
	}
}

func TestDefaultAgentTypes(t *testing.T) {
	agentTypes := DefaultAgentTypes()

	claude, ok := agentTypes["claude-code"]
	if !ok {
		t.Fatal(`DefaultAgentTypes()["claude-code"] missing`)
	}
	if claude.LaunchTemplate != "claude" {
		t.Errorf("claude-code LaunchTemplate = %q, want %q", claude.LaunchTemplate, "claude")
	}
	if claude.Tier != completion.TierMarker {
		t.Errorf("claude-code Tier = %v, want TierMarker", claude.Tier)
	}

	shell, ok := agentTypes[""]
	if !ok {
		t.Fatal(`DefaultAgentTypes()[""] missing`)
	}
	if shell.Tier != completion.TierIdle {
		t.Errorf(`"" Tier = %v, want TierIdle`, shell.Tier)
	}
}

func TestDispatchableAgentTypeNames_ExcludesEmptyKey(t *testing.T) {
	names := dispatchableAgentTypeNames(DefaultAgentTypes())
	for _, n := range names {
		if n == "" {
			t.Fatal(`dispatchableAgentTypeNames included "" — must never offer it to the router model`)
		}
	}
	if len(names) != 1 || names[0] != "claude-code" {
		t.Errorf("dispatchableAgentTypeNames = %v, want [claude-code]", names)
	}
}
