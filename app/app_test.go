package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Loomux/server/completion"
	"github.com/Loomux/server/router"
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

	reply, err := app.Dispatch(context.Background(), "conv-1", "hi", "")
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

	codex, ok := agentTypes["codex"]
	if !ok {
		t.Fatal(`DefaultAgentTypes()["codex"] missing`)
	}
	if codex.LaunchTemplate != "codex" {
		t.Errorf("codex LaunchTemplate = %q, want %q", codex.LaunchTemplate, "codex")
	}
	if codex.Tier != completion.TierMarker {
		t.Errorf("codex Tier = %v, want TierMarker (mirrors claude-code's wiring level — see LOOM-22)", codex.Tier)
	}

	// LOOM-71: both real agents are probed for before launch, and carry an
	// install recipe with the login step the CLI needs afterwards.
	for name, want := range map[string]struct{ binary, install, login string }{
		"claude-code": {"claude", "curl -fsSL https://claude.ai/install.sh | bash", "claude"},
		"codex":       {"codex", "npm install -g @openai/codex", "codex login"},
	} {
		at := agentTypes[name]
		if at.Binary != want.binary {
			t.Errorf("%s Binary = %q, want %q", name, at.Binary, want.binary)
		}
		if at.Install == nil || at.Install.Command != want.install || !strings.Contains(at.Install.Login, want.login) {
			t.Errorf("%s Install = %+v, want command %q and a login step mentioning %q", name, at.Install, want.install, want.login)
		}
	}
	// LOOM-79: both real agents run their version check (against the
	// probed absolute path — see router.launchAgent).
	for name, cmd := range map[string]string{"claude-code": "claude --version", "codex": "codex --version"} {
		vc := agentTypes[name].VersionCheck
		if vc == nil || vc.Command != cmd || vc.Parse == nil || vc.Min == "" {
			t.Errorf("%s VersionCheck = %+v, want %q with a parser and a minimum", name, vc, cmd)
			continue
		}
	}
	if v, err := agentTypes["claude-code"].VersionCheck.Parse("2.1.287 (Claude Code)"); err != nil || router.CheckVersionRange(v, agentTypes["claude-code"].VersionCheck.Min, agentTypes["claude-code"].VersionCheck.Max) != nil {
		t.Errorf("claude-code 2.1.287 (observed on a real target) fails its own version check: %q %v", v, err)
	}
	if v, err := agentTypes["codex"].VersionCheck.Parse("codex-cli 0.150.1"); err != nil || router.CheckVersionRange(v, agentTypes["codex"].VersionCheck.Min, agentTypes["codex"].VersionCheck.Max) != nil {
		t.Errorf("codex 0.150.1 (observed on a real target) fails its own version check: %q %v", v, err)
	}
	if shell.Binary != "" || shell.Install != nil {
		t.Errorf(`"" entry is probed or installable (%+v); it isn't a real agent`, shell)
	}

	// LOOM-75: a TierMarker agent-type waits for a marker, so Loomux must
	// inject what writes it — nothing on the target does.
	for name, at := range map[string]router.AgentType{"claude-code": claude, "codex": codex} {
		if len(at.CompletionHookArgs) == 0 {
			t.Errorf("%s CompletionHookArgs empty: its marker would never be written", name)
		}
		if at.VersionCheck == nil {
			t.Errorf("%s VersionCheck nil: a CLI too old for the hook flag wouldn't fail fast", name)
		}
	}
}

func TestDispatchableAgentTypeNames_ExcludesEmptyKey(t *testing.T) {
	names := dispatchableAgentTypeNames(DefaultAgentTypes())
	for _, n := range names {
		if n == "" {
			t.Fatal(`dispatchableAgentTypeNames included "" — must never offer it to the router model`)
		}
	}
	want := []string{"claude-code", "codex"}
	if len(names) != len(want) {
		t.Fatalf("dispatchableAgentTypeNames = %v, want (in any order) %v", names, want)
	}
	got := map[string]bool{}
	for _, n := range names {
		got[n] = true
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("dispatchableAgentTypeNames = %v, missing %q", names, w)
		}
	}
}
