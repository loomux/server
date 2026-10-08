package llmrouter

import (
	"context"
	"os"
	"testing"

	"github.com/Loomux/server/router"
)

// TestAnthropic_Live runs Decide and Relay against Anthropic's real API
// (LOOM-186). It is skipped unless LOOMUX_ROUTER_LIVE_ANTHROPIC_API_KEY
// is set, so CI never calls out; LOOMUX_ROUTER_LIVE_ANTHROPIC_MODEL picks
// the model (default claude-haiku-4-5) and
// LOOMUX_ROUTER_LIVE_ANTHROPIC_BASE_URL an alternative endpoint.
//
//	LOOMUX_ROUTER_LIVE_ANTHROPIC_API_KEY=… LOOMUX_ROUTER_LIVE_ANTHROPIC_MODEL=claude-sonnet-5-5 \
//	  go test ./router/llmrouter/ -run Anthropic_Live -v
func TestAnthropic_Live(t *testing.T) {
	key := os.Getenv("LOOMUX_ROUTER_LIVE_ANTHROPIC_API_KEY")
	if key == "" {
		t.Skip("LOOMUX_ROUTER_LIVE_ANTHROPIC_API_KEY not set")
	}
	model := os.Getenv("LOOMUX_ROUTER_LIVE_ANTHROPIC_MODEL")
	if model == "" {
		model = "claude-haiku-4-5"
	}
	tier := Tier{Provider: ProviderAnthropic, BaseURL: os.Getenv("LOOMUX_ROUTER_LIVE_ANTHROPIC_BASE_URL"), APIKey: key, Model: model}
	m, err := New(Config{Primary: tier}, []string{"claude-code", "codex"}, WithPrimaryTimeout(defaultEscalationTimeout))
	if err != nil {
		t.Fatal(err)
	}

	workspaces := []router.WorkspaceSnapshot{{ID: "ws-api", Name: "api", Status: "idle", Description: "The Go API server", TargetName: "box"}}
	targets := []router.TargetSnapshot{{ID: "t-box", Name: "box", Kind: "local", Agents: map[string]bool{"claude-code": true}}}
	dec, err := m.Decide(context.Background(), "In the api workspace, fix the failing unit test in handlers_test.go", workspaces, targets)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	t.Logf("Decide: %+v", dec)
	if dec.Action != router.ActionUseWorkspace || dec.WorkspaceID != "ws-api" {
		t.Errorf("Decide = %+v, want use_workspace ws-api", dec)
	}

	res, err := m.Relay(context.Background(), router.RelayInput{
		AgentType:   "claude-code",
		UserMessage: "fix the failing unit test in handlers_test.go",
		Captured:    "● Fixed the nil map in handlers.go:42. ✓ go test ./... — all 37 tests pass.\n> ",
	})
	if err != nil {
		t.Fatalf("Relay: %v", err)
	}
	t.Logf("Relay: %+v", res)
	if res.Reply == "" || !res.Done {
		t.Errorf("Relay = %+v, want a reply and done", res)
	}
}
