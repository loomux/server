package llmrouter

import (
	"context"
	"strings"
	"testing"

	"github.com/Loomux/server/router"
	"github.com/Loomux/server/router/llmrouter/llmroutertest"
)

// compatTier is an escalation tier on Anthropic's OpenAI-compatible
// endpoint, configured exactly as for the live trial (LOOM-187).
func compatTier(model string) Tier {
	return Tier{Provider: ProviderOpenAI, BaseURL: "https://api.anthropic.com/v1", APIKey: "compat-key", Model: model}
}

func compatDecision(r llmroutertest.Request) llmroutertest.Reply {
	return llmroutertest.Reply{ToolArgs: map[string]any{"action": "answer_directly", "direct_answer": "hi"}}
}

func TestAnthropicCompat_Decide(t *testing.T) {
	fake := llmroutertest.NewAnthropicCompat("compat-key", compatDecision)
	defer fake.Close()
	m, err := New(Config{Primary: compatTier("claude-haiku-5-5")}, nil, WithHTTPClient(fake.Client()))
	if err != nil {
		t.Fatal(err)
	}
	d, err := m.Decide(context.Background(), "hello", nil, nil)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if d.Action != router.ActionAnswerDirectly || d.Model != "claude-haiku-5-5" {
		t.Errorf("decision = %+v", d)
	}
	reqs := fake.Requests()
	if len(reqs) != 1 {
		t.Fatalf("%d requests, want 1", len(reqs))
	}
	r := reqs[0]
	if r.Path != "/v1/chat/completions" || !r.Authorized || r.Model != "claude-haiku-5-5" {
		t.Errorf("request to %s authorized=%v model=%q", r.Path, r.Authorized, r.Model)
	}
	if got, _ := r.Body["max_completion_tokens"].(float64); got != anthropicMaxTokens {
		t.Errorf("max_completion_tokens = %v, want %d", r.Body["max_completion_tokens"], anthropicMaxTokens)
	}
	if !r.Forced() || r.ToolName != decideToolName {
		t.Errorf("tool_choice = %v, tool %q: want forced %s", r.ToolChoice, r.ToolName, decideToolName)
	}
	if len(r.Ignored) != 0 {
		t.Errorf("sent fields the endpoint ignores: %v", r.Ignored)
	}
}

// Other OpenAI-protocol endpoints keep the request they had: no token
// bound the router never set before.
func TestOpenAIExchange_NoMaxTokensElsewhere(t *testing.T) {
	ex := newOpenAIExchange(Tier{BaseURL: "https://api.groq.com/openai/v1", APIKey: "k", Model: "m"}, "s", "u", relayTool(), nil)
	if ex.params.MaxCompletionTokens.Valid() {
		t.Errorf("max_completion_tokens set for a non-Anthropic endpoint")
	}
}

// strict is ignored there, so a tool call that doesn't fit the schema
// can come back; it gets the corrective retry like any other.
func TestAnthropicCompat_SchemaViolationRetried(t *testing.T) {
	calls := 0
	fake := llmroutertest.NewAnthropicCompat("compat-key", func(r llmroutertest.Request) llmroutertest.Reply {
		calls++
		if calls == 1 {
			return llmroutertest.Reply{ToolArgs: map[string]any{"action": "use_workspace", "workspace_id": "ws-nope", "agent_type": "claude-code"}}
		}
		return compatDecision(r)
	})
	defer fake.Close()
	m, err := New(Config{Primary: compatTier("claude-haiku-5-5")}, []string{"claude-code"}, WithHTTPClient(fake.Client()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Decide(context.Background(), "hello", nil, nil); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	reqs := fake.Requests()
	if len(reqs) != 2 || !strings.Contains(reqs[1].User, "rejected") {
		t.Fatalf("%d requests (last user turn %q), want a corrective retry", len(reqs), reqs[len(reqs)-1].User)
	}
}

// A model that takes no forced tool_choice fails every call with a 400,
// not retried; with an escalation tier configured, it escalates.
func TestAnthropicCompat_ForcedToolChoiceRejected(t *testing.T) {
	fake := llmroutertest.NewAnthropicCompat("compat-key", compatDecision)
	fake.RejectForcedToolChoice = true
	defer fake.Close()
	m, err := New(Config{Primary: compatTier("claude-sonnet-5-5")}, nil, WithHTTPClient(fake.Client()))
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Decide(context.Background(), "hello", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("Decide = %v, want a 400", err)
	}
	if n := len(fake.Requests()); n != 1 {
		t.Errorf("%d requests, want 1 (a 400 isn't retried)", n)
	}
}

func TestAnthropicCompat_RateLimitRetried(t *testing.T) {
	fake := llmroutertest.NewAnthropicCompat("compat-key", compatDecision)
	fake.RateLimitFirst = 2
	defer fake.Close()
	m, err := New(Config{Primary: compatTier("claude-haiku-5-5")}, nil, WithHTTPClient(fake.Client()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Decide(context.Background(), "hello", nil, nil); err != nil {
		t.Fatalf("Decide after two 429s: %v", err)
	}
	if n := len(fake.Requests()); n != 3 {
		t.Errorf("%d requests, want 3", n)
	}
}

func TestAnthropicCompat_Relay(t *testing.T) {
	fake := llmroutertest.NewAnthropicCompat("compat-key", func(r llmroutertest.Request) llmroutertest.Reply {
		return llmroutertest.Reply{ToolArgs: map[string]any{"reply": "Created hello.txt.", "done": true}}
	})
	defer fake.Close()
	m, err := New(Config{Primary: compatTier("claude-haiku-5-5")}, nil, WithHTTPClient(fake.Client()))
	if err != nil {
		t.Fatal(err)
	}
	res, err := m.Relay(context.Background(), router.RelayInput{Captured: "● Created hello.txt.", AgentType: "claude-code"})
	if err != nil || !res.Done || res.Reply != "Created hello.txt." {
		t.Fatalf("Relay = %+v, %v", res, err)
	}
}
