package llmrouter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Loomux/server/router"
)

// fakeProvider builds a fake endpoint's answers in one provider's wire
// format, so the same scenarios run against every provider (LOOM-186).
type fakeProvider struct {
	provider Provider
	toolCall func(t *testing.T, name string, args any) http.HandlerFunc
	text     func(t *testing.T, text string) http.HandlerFunc
	refusal  func(t *testing.T) http.HandlerFunc
	// error answers with an HTTP error status in the provider's shape.
	error func(status int) http.HandlerFunc
}

func (p fakeProvider) tier(srv *httptest.Server, model string) Tier {
	return Tier{Provider: p.provider, BaseURL: srv.URL, APIKey: "test-key", Model: model}
}

var fakeProviders = []fakeProvider{
	{
		provider: ProviderOpenAI,
		toolCall: toolCallHandler,
		text:     textHandler,
		refusal: func(t *testing.T) http.HandlerFunc {
			return jsonHandler(t, http.StatusOK, map[string]any{
				"id": "chatcmpl-test", "object": "chat.completion", "created": 1, "model": "test-model",
				"choices": []any{map[string]any{"index": 0, "finish_reason": "stop",
					"message": map[string]any{"role": "assistant", "content": nil, "refusal": "I can't help with that."}}},
			})
		},
		error: errorHandler,
	},
	{
		provider: ProviderAnthropic,
		toolCall: anthropicToolCallHandler,
		text:     anthropicTextHandler,
		refusal:  anthropicRefusalHandler,
		error:    anthropicErrorHandler,
	},
}

// eachProvider runs f once per provider as a subtest.
func eachProvider(t *testing.T, f func(t *testing.T, p fakeProvider)) {
	for _, p := range fakeProviders {
		t.Run(string(p.provider), func(t *testing.T) { f(t, p) })
	}
}

func anthropicMessage(t *testing.T, stopReason string, content []any, extra map[string]any) http.HandlerFunc {
	t.Helper()
	msg := map[string]any{
		"id": "msg_test", "type": "message", "role": "assistant", "model": "test-model",
		"content": content, "stop_reason": stopReason, "stop_sequence": nil,
		"usage": map[string]any{"input_tokens": 100, "output_tokens": 20, "cache_read_input_tokens": 50, "cache_creation_input_tokens": 0},
	}
	for k, v := range extra {
		msg[k] = v
	}
	h := jsonHandler(t, http.StatusOK, msg)
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" || r.Header.Get("X-Api-Key") != "test-key" {
			t.Errorf("request to %s with key %q, want /v1/messages with the tier's key", r.URL.Path, r.Header.Get("X-Api-Key"))
			http.NotFound(w, r)
			return
		}
		h(w, r)
	}
}

func anthropicToolCallHandler(t *testing.T, name string, args any) http.HandlerFunc {
	t.Helper()
	return anthropicMessage(t, "tool_use", []any{
		map[string]any{"type": "thinking", "thinking": "", "signature": "sig"},
		map[string]any{"type": "tool_use", "id": "toolu_1", "name": name, "input": args},
	}, nil)
}

func anthropicTextHandler(t *testing.T, text string) http.HandlerFunc {
	t.Helper()
	return anthropicMessage(t, "end_turn", []any{map[string]any{"type": "text", "text": text}}, nil)
}

func anthropicRefusalHandler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return anthropicMessage(t, "refusal", []any{}, map[string]any{
		"stop_details": map[string]any{"type": "refusal", "category": "cyber", "explanation": "declined"},
	})
}

func anthropicErrorHandler(status int) http.HandlerFunc {
	errType := map[int]string{400: "invalid_request_error", 429: "rate_limit_error", 529: "overloaded_error"}[status]
	if errType == "" {
		errType = "api_error"
	}
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"` + errType + `","message":"boom"}}`))
	}
}

// retryAfterZero has the SDKs retry an error at once instead of backing
// off.
func retryAfterZero(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "0")
		h(w, r)
	}
}

func TestProvider_Decide_ToolCall(t *testing.T) {
	eachProvider(t, func(t *testing.T, p fakeProvider) {
		srv, count := newFakeServer(t, p.toolCall(t, decideToolName, map[string]any{"action": "use_workspace", "workspace_id": "ws-1", "agent_type": "claude-code"}))
		m, err := New(Config{Primary: p.tier(srv, "test-model")}, []string{"claude-code"})
		if err != nil {
			t.Fatal(err)
		}
		dec, err := m.Decide(context.Background(), "fix the bug", []router.WorkspaceSnapshot{{ID: "ws-1"}}, nil)
		if err != nil {
			t.Fatalf("Decide: %v", err)
		}
		if dec.Action != router.ActionUseWorkspace || dec.WorkspaceID != "ws-1" || dec.Model != "test-model" || dec.Tier != "primary" {
			t.Errorf("Decide = %+v", dec)
		}
		if *count != 1 {
			t.Errorf("called %d times, want 1", *count)
		}
	})
}

func TestProvider_Relay_ToolCall(t *testing.T) {
	eachProvider(t, func(t *testing.T, p fakeProvider) {
		srv, _ := newFakeServer(t, p.toolCall(t, relayToolName, map[string]any{"reply": "Fixed it.", "done": true}))
		m, err := New(Config{Primary: p.tier(srv, "test-model")}, nil)
		if err != nil {
			t.Fatal(err)
		}
		res, err := m.Relay(context.Background(), router.RelayInput{Captured: "output"})
		if err != nil {
			t.Fatalf("Relay: %v", err)
		}
		if res.Reply != "Fixed it." || !res.Done {
			t.Errorf("Relay = %+v", res)
		}
	})
}

// A reply without the tool call is retried once on the same tier, saying
// so; the retry's call is used.
func TestProvider_NoToolCall_RetriedOnce(t *testing.T) {
	eachProvider(t, func(t *testing.T, p fakeProvider) {
		h, bodies := sequenceHandler(
			p.text(t, "Sure, I'd route that to ws-1."),
			p.toolCall(t, decideToolName, map[string]any{"action": "answer_directly", "direct_answer": "hello"}),
		)
		srv, count := newFakeServer(t, h)
		m, err := New(Config{Primary: p.tier(srv, "test-model")}, nil)
		if err != nil {
			t.Fatal(err)
		}
		dec, err := m.Decide(context.Background(), "hi", nil, nil)
		if err != nil {
			t.Fatalf("Decide: %v", err)
		}
		if dec.DirectAnswer != "hello" || *count != 2 {
			t.Errorf("Decide = %+v after %d calls, want hello after 2", dec, *count)
		}
		if b := bodies(); len(b) != 2 || !strings.Contains(b[1], "did not call "+decideToolName) {
			t.Errorf("the retry doesn't say what was wrong: %v", b)
		}
	})
}

// Two replies without the tool call surface an error rather than a
// decision that isn't schema-valid.
func TestProvider_NoToolCallTwice_Errors(t *testing.T) {
	eachProvider(t, func(t *testing.T, p fakeProvider) {
		srv, count := newFakeServer(t, p.text(t, "no tool for you"))
		m, err := New(Config{Primary: p.tier(srv, "test-model")}, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, call := range []func() error{
			func() error { _, err := m.Decide(context.Background(), "hi", nil, nil); return err },
			func() error { _, err := m.Relay(context.Background(), router.RelayInput{Captured: "x"}); return err },
		} {
			atomic.StoreInt32(count, 0)
			err := call()
			if err == nil || !strings.Contains(err.Error(), "did not call") {
				t.Errorf("err = %v, want a did-not-call error", err)
			}
			if *count != 2 {
				t.Errorf("called %d times, want 2", *count)
			}
		}
	})
}

// A refusal isn't asked again on the same tier: it escalates at once,
// and doesn't count against the primary's breaker.
func TestProvider_Refusal_Escalates(t *testing.T) {
	eachProvider(t, func(t *testing.T, p fakeProvider) {
		primarySrv, primaryCount := newFakeServer(t, p.refusal(t))
		escalationSrv, escalationCount := newFakeServer(t, p.toolCall(t, decideToolName, map[string]any{"action": "answer_directly", "direct_answer": "from escalation"}))
		escalation := p.tier(escalationSrv, "escalation-model")
		m, err := New(Config{Primary: p.tier(primarySrv, "test-model"), Escalation: &escalation}, nil)
		if err != nil {
			t.Fatal(err)
		}
		dec, err := m.Decide(context.Background(), "hi", nil, nil)
		if err != nil || dec.DirectAnswer != "from escalation" {
			t.Fatalf("Decide = %+v, %v", dec, err)
		}
		if *primaryCount != 1 || *escalationCount != 1 {
			t.Errorf("primary called %d times, escalation %d; want 1 and 1", *primaryCount, *escalationCount)
		}
		if m.primaryBreaker.failures != 0 {
			t.Errorf("a refusal counted against the breaker")
		}

		m2, err := New(Config{Primary: p.tier(primarySrv, "test-model")}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := m2.Decide(context.Background(), "hi", nil, nil); err == nil || !strings.Contains(err.Error(), "refused") {
			t.Errorf("without escalation err = %v, want a refusal error", err)
		}
	})
}

// A 429 is retried by the SDK and the retry's answer used.
func TestProvider_RateLimited_Retried(t *testing.T) {
	eachProvider(t, func(t *testing.T, p fakeProvider) {
		h, _ := sequenceHandler(
			retryAfterZero(p.error(http.StatusTooManyRequests)),
			p.toolCall(t, decideToolName, map[string]any{"action": "answer_directly", "direct_answer": "ok"}),
		)
		srv, count := newFakeServer(t, h)
		m, err := New(Config{Primary: p.tier(srv, "test-model")}, nil)
		if err != nil {
			t.Fatal(err)
		}
		dec, err := m.Decide(context.Background(), "hi", nil, nil)
		if err != nil || dec.DirectAnswer != "ok" {
			t.Fatalf("Decide = %+v, %v", dec, err)
		}
		if *count != 2 {
			t.Errorf("called %d times, want 2 (one 429, one retry)", *count)
		}
	})
}

// A 400 isn't retried: it fails the tier at once and counts as the
// tier being unavailable.
func TestProvider_BadRequest_NotRetried(t *testing.T) {
	eachProvider(t, func(t *testing.T, p fakeProvider) {
		srv, count := newFakeServer(t, p.error(http.StatusBadRequest))
		m, err := New(Config{Primary: p.tier(srv, "test-model")}, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, err = m.Decide(context.Background(), "hi", nil, nil)
		if err == nil || !strings.Contains(err.Error(), "400") {
			t.Fatalf("err = %v, want a 400 failure", err)
		}
		if *count != 1 {
			t.Errorf("called %d times, want 1", *count)
		}
	})
}

// anthropicRequest is the part of a Messages API request these tests
// check.
type anthropicRequest struct {
	Model     string `json:"model"`
	MaxTokens int    `json:"max_tokens"`
	System    []struct {
		Text         string         `json:"text"`
		CacheControl map[string]any `json:"cache_control"`
	} `json:"system"`
	Messages []struct {
		Role    string           `json:"role"`
		Content []map[string]any `json:"content"`
	} `json:"messages"`
	Tools []struct {
		Name        string         `json:"name"`
		Strict      bool           `json:"strict"`
		InputSchema map[string]any `json:"input_schema"`
	} `json:"tools"`
	ToolChoice   map[string]any `json:"tool_choice"`
	OutputConfig *struct {
		Effort string `json:"effort"`
	} `json:"output_config"`
}

func decodeAnthropicRequest(t *testing.T, body string) anthropicRequest {
	t.Helper()
	var req anthropicRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("decode request: %v\n%s", err, body)
	}
	return req
}

// The Anthropic request: tool_choice auto (current models reject a
// forced one) with a strict, closed schema; the system prompt, which
// says to call the tool, cached; low effort where the model takes it.
func TestAnthropic_RequestShape(t *testing.T) {
	for _, tc := range []struct {
		model      string
		wantEffort string
	}{
		{"claude-sonnet-5-5", "low"},
		{"claude-opus-5-5", "low"},
		{"claude-haiku-4-5", ""},
	} {
		t.Run(tc.model, func(t *testing.T) {
			h, bodies := sequenceHandler(anthropicToolCallHandler(t, decideToolName, map[string]any{"action": "answer_directly", "direct_answer": "ok"}))
			srv, _ := newFakeServer(t, h)
			tier := Tier{Provider: ProviderAnthropic, BaseURL: srv.URL + "/v1/", APIKey: "test-key", Model: tc.model}
			m, err := New(Config{Primary: tier}, []string{"claude-code"})
			if err != nil {
				t.Fatal(err)
			}
			targets := []router.TargetSnapshot{{ID: "t-1", Name: "box"}}
			if _, err := m.Decide(context.Background(), "hi", []router.WorkspaceSnapshot{{ID: "ws-1"}}, targets); err != nil {
				t.Fatalf("Decide: %v", err)
			}
			req := decodeAnthropicRequest(t, bodies()[0])

			if req.Model != tc.model || req.MaxTokens != anthropicMaxTokens {
				t.Errorf("model %q max_tokens %d", req.Model, req.MaxTokens)
			}
			if req.ToolChoice["type"] != "auto" || req.ToolChoice["disable_parallel_tool_use"] != true {
				t.Errorf("tool_choice = %v, want auto with parallel tool use off", req.ToolChoice)
			}
			if len(req.Tools) != 1 || req.Tools[0].Name != decideToolName || !req.Tools[0].Strict {
				t.Fatalf("tools = %+v, want one strict %s", req.Tools, decideToolName)
			}
			schema := req.Tools[0].InputSchema
			if schema["additionalProperties"] != false {
				t.Errorf("top-level schema isn't closed: %v", schema)
			}
			nw := schema["properties"].(map[string]any)["new_workspace"].(map[string]any)
			if nw["additionalProperties"] != false {
				t.Errorf("new_workspace isn't closed: %v", nw)
			}
			if raw, _ := json.Marshal(schema); strings.Contains(string(raw), `"pattern"`) {
				t.Errorf("schema keeps a pattern strict mode may not compile: %s", raw)
			}
			if len(req.System) != 1 || req.System[0].CacheControl["type"] != "ephemeral" ||
				!strings.Contains(req.System[0].Text, "Always respond by calling the "+decideToolName+" tool") {
				t.Errorf("system = %+v, want one cached block that says to call the tool", req.System)
			}
			gotEffort := ""
			if req.OutputConfig != nil {
				gotEffort = req.OutputConfig.Effort
			}
			if gotEffort != tc.wantEffort {
				t.Errorf("effort = %q, want %q", gotEffort, tc.wantEffort)
			}
		})
	}
}

// The corrective retry replays the rejected reply unchanged (its
// thinking block included) and answers its tool call with an error
// result naming the problem.
func TestAnthropic_CorrectiveRetry_AnswersToolCall(t *testing.T) {
	h, bodies := sequenceHandler(
		anthropicToolCallHandler(t, decideToolName, map[string]any{"action": "use_workspace", "workspace_id": "ws-invented", "agent_type": "claude-code"}),
		anthropicToolCallHandler(t, decideToolName, map[string]any{"action": "use_workspace", "workspace_id": "ws-1", "agent_type": "claude-code"}),
	)
	srv, _ := newFakeServer(t, h)
	m, err := New(Config{Primary: Tier{Provider: ProviderAnthropic, BaseURL: srv.URL, APIKey: "test-key", Model: "claude-sonnet-5-5"}}, []string{"claude-code"})
	if err != nil {
		t.Fatal(err)
	}
	dec, err := m.Decide(context.Background(), "fix it", []router.WorkspaceSnapshot{{ID: "ws-1"}}, nil)
	if err != nil || dec.WorkspaceID != "ws-1" {
		t.Fatalf("Decide = %+v, %v", dec, err)
	}
	b := bodies()
	if len(b) != 2 {
		t.Fatalf("%d requests, want 2", len(b))
	}
	req := decodeAnthropicRequest(t, b[1])
	if len(req.Messages) != 3 {
		t.Fatalf("retry has %d messages, want user, assistant, user", len(req.Messages))
	}
	assistant, result := req.Messages[1], req.Messages[2]
	if assistant.Role != "assistant" || len(assistant.Content) != 2 ||
		assistant.Content[0]["type"] != "thinking" || assistant.Content[1]["id"] != "toolu_1" {
		t.Errorf("rejected reply not replayed as it came: %+v", assistant)
	}
	if result.Role != "user" || len(result.Content) != 1 || result.Content[0]["type"] != "tool_result" ||
		result.Content[0]["tool_use_id"] != "toolu_1" || result.Content[0]["is_error"] != true {
		t.Fatalf("correction isn't an error tool_result: %+v", result)
	}
	if raw, _ := json.Marshal(result.Content[0]["content"]); !strings.Contains(string(raw), "ws-invented") {
		t.Errorf("correction doesn't say what was wrong: %s", raw)
	}
}

// Prompt tokens include cache reads and writes.
func TestAnthropic_TokenUsage(t *testing.T) {
	srv, _ := newFakeServer(t, anthropicToolCallHandler(t, relayToolName, map[string]any{"reply": "r", "done": false}))
	ex := newAnthropicExchange(Tier{Provider: ProviderAnthropic, BaseURL: srv.URL, APIKey: "test-key", Model: "claude-haiku-4-5"}, "sys", "user", relayTool())
	a, err := ex.ask(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !a.called || a.prompt != 150 || a.completion != 20 || a.total != 170 {
		t.Errorf("answer = %+v, want called with 150 prompt / 20 completion / 170 total", a)
	}
}

func TestSupportsEffort(t *testing.T) {
	for model, want := range map[string]bool{
		"claude-haiku-4-5":            false,
		"claude-haiku-4-5-20251001":   false,
		"claude-sonnet-4-5":           false,
		"claude-opus-4-1":             false,
		"claude-3-5-haiku-latest":     false,
		"claude-sonnet-4-6":           true,
		"claude-sonnet-5-5":           true,
		"claude-opus-5-5":             true,
		"claude-fable-5-1":            true,
		"claude-haiku-5-5":            true,
		"anthropic.claude-haiku-4-5":  false,
		"anthropic.claude-sonnet-5-5": true,
	} {
		if got := supportsEffort(model); got != want {
			t.Errorf("supportsEffort(%q) = %v, want %v", model, got, want)
		}
	}
}

func TestStrictSchema_LeavesInputAlone(t *testing.T) {
	in := decideTool([]string{"claude-code"}, []string{"ws-1"}, []string{"t-1"}, false).schema
	before, _ := json.Marshal(in)
	_ = anthropicInputSchema(in)
	after, _ := json.Marshal(in)
	if string(before) != string(after) {
		t.Errorf("anthropicInputSchema modified the shared schema")
	}
}

func TestParseProvider(t *testing.T) {
	for in, want := range map[string]Provider{"": ProviderOpenAI, "openai": ProviderOpenAI, " Anthropic ": ProviderAnthropic} {
		if got, err := ParseProvider(in); err != nil || got != want {
			t.Errorf("ParseProvider(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := ParseProvider("gemini"); !errors.Is(err, ErrConfigInvalid) {
		t.Errorf("ParseProvider(gemini) err = %v, want ErrConfigInvalid", err)
	}
}

func TestTierValidate(t *testing.T) {
	for _, tc := range []struct {
		tier Tier
		ok   bool
	}{
		{Tier{BaseURL: "http://x", APIKey: "k", Model: "m"}, true},
		{Tier{APIKey: "k", Model: "m"}, false},
		{Tier{Provider: ProviderAnthropic, APIKey: "k", Model: "m"}, true},
		{Tier{Provider: ProviderAnthropic, Model: "m"}, false},
		{Tier{Provider: ProviderAnthropic, APIKey: "k"}, false},
		{Tier{Provider: "gemini", BaseURL: "http://x", APIKey: "k", Model: "m"}, false},
	} {
		err := tc.tier.Validate()
		if (err == nil) != tc.ok || (err != nil && !errors.Is(err, ErrConfigInvalid)) {
			t.Errorf("%+v.Validate() = %v, want ok=%v", tc.tier, err, tc.ok)
		}
	}
}
