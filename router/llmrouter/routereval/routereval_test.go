package routereval

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Loomux/server/router/llmrouter"
	"github.com/Loomux/server/router/llmrouter/llmroutertest"
)

// liveTier is configured exactly as the live trial's escalation tier;
// the fake's client keeps every request in the process.
func liveTier(model string) llmrouter.Tier {
	return llmrouter.Tier{Provider: llmrouter.ProviderOpenAI, BaseURL: "https://api.anthropic.com/v1", APIKey: "sk-ant-test-0000", Model: model}
}

func fixtures(t *testing.T) *Fixtures {
	t.Helper()
	fx, err := DefaultFixtures()
	if err != nil {
		t.Fatalf("DefaultFixtures: %v", err)
	}
	return fx
}

func offline(t *testing.T, fake *llmroutertest.AnthropicCompat, model string, o Options) *Report {
	t.Helper()
	o.Tier, o.HTTPClient, o.Offline = liveTier(model), fake.Client(), true
	rep, err := Run(context.Background(), fixtures(t), o)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return rep
}

func TestDefaultFixtures(t *testing.T) {
	fx := fixtures(t)
	if len(fx.Decide) < 20 || len(fx.Relay) < 5 {
		t.Errorf("%d decide and %d relay cases, want a representative set", len(fx.Decide), len(fx.Relay))
	}
	cats := map[string]bool{}
	for _, c := range fx.Decide {
		cats[c.Category] = true
	}
	for _, want := range []string{"provision", "command", "agent-by-name", "reuse-workspace", "answer-directly", "open-task", "follow-up", "hard"} {
		if !cats[want] {
			t.Errorf("no %q case", want)
		}
	}
}

func TestFixtures_Validate(t *testing.T) {
	_, err := parseFixtures([]byte(`{"world":{"workspaces":[{"id":"ws-a"}]},
		"decide":[{"name":"a","message":"m","expect":{"action":"use_workspace","workspace_id":"ws-nope"}},
		          {"name":"a","message":"m","expect":{"action":"fly"}}],
		"relay":[{"name":"r"}]}`))
	if err == nil {
		t.Fatal("bad fixtures accepted")
	}
	for _, want := range []string{"ws-nope", `"a" is empty or repeated`, `unknown action "fly"`, "r: nothing captured"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q doesn't mention %q", err, want)
		}
	}
}

// Offline, every case answered with its expected decision scores 100%:
// the harness's requests, parsing and scoring agree with the fixtures.
func TestRun_OfflineOracleScoresEverything(t *testing.T) {
	fx := fixtures(t)
	fake := llmroutertest.NewAnthropicCompat("sk-ant-test-0000", Oracle(fx))
	defer fake.Close()
	rep := offline(t, fake, "claude-haiku-5-5", Options{Runs: 2})
	s := rep.Summary
	want := 2 * (len(fx.Decide) + len(fx.Relay))
	if s.Calls != want || s.Passed != want {
		for _, c := range rep.Cases {
			if c.Passed != c.Runs {
				t.Logf("%s: %v %v", c.Name, c.Raw, c.Failures)
			}
		}
		t.Fatalf("passed %d of %d calls, want all %d", s.Passed, s.Calls, want)
	}
	if s.Accuracy != 1 || s.HTTPAttempts != want || s.CorrectiveRetries != 0 {
		t.Errorf("summary = %+v", s)
	}
	if s.PromptTokens == 0 || s.CompletionTokens == 0 {
		t.Errorf("no token usage metered: %+v", s)
	}
	if rep.Price == nil || s.CostUSD <= 0 || s.CostPer1kCalls <= 0 {
		t.Errorf("no cost for a priced model: price %v, %+v", rep.Price, s)
	}
	for _, r := range fake.Requests() {
		if r.Path != "/v1/chat/completions" || !r.Authorized || r.Model != "claude-haiku-5-5" || !r.Forced() {
			t.Fatalf("request %+v", r)
		}
	}
	data, _ := json.Marshal(rep)
	if bytes.Contains(data, []byte("sk-ant-test-0000")) {
		t.Error("the report carries the key")
	}
}

// A wrong answer is scored wrong; an unusable one (no tool call, even
// after the corrective retry) too; an HTTP error is an error, by class.
func TestRun_ScoresWrongUnusableAndErrors(t *testing.T) {
	fx := fixtures(t)
	oracle := Oracle(fx)
	fake := llmroutertest.NewAnthropicCompat("sk-ant-test-0000", func(r llmroutertest.Request) llmroutertest.Reply {
		switch {
		case strings.Contains(r.User, "how much disk is free on the nas?"):
			return llmroutertest.Reply{ToolArgs: map[string]any{"action": "run_command", "target_id": "t-gpu01", "command": "df -h"}}
		case strings.Contains(r.User, "what's the uptime on gpu01"):
			return llmroutertest.Reply{Text: "uptime is a command"}
		case strings.Contains(r.User, "is anything using the gpu"):
			return llmroutertest.Reply{Status: 500, ErrMessage: "overloaded"}
		}
		return oracle(r)
	})
	defer fake.Close()
	rep := offline(t, fake, "claude-haiku-5-5", Options{Cases: []string{"disk-free", "uptime", "gpu-status", "codex-by-name"}})
	got := map[string]CaseResult{}
	for _, c := range rep.Cases {
		got[c.Name] = c
	}
	if c := got["disk-free-is-a-command"]; c.Wrong != 1 || !strings.Contains(strings.Join(c.Failures, ""), "target t-gpu01, want t-nas") {
		t.Errorf("wrong target: %+v", c)
	}
	if c := got["uptime-is-a-command"]; c.Wrong != 1 || c.Answered != 2 {
		t.Errorf("no tool call twice: %+v", c)
	}
	if c := got["gpu-status-is-a-command"]; c.Errors != 1 || c.HTTPAttempts != 3 || len(c.LatencyMS) != 0 {
		t.Errorf("500 after the SDK's retries: %+v", c)
	}
	if c := got["codex-by-name"]; c.Passed != 1 {
		t.Errorf("codex-by-name: %+v", c)
	}
	s := rep.Summary
	if s.Calls != 4 || s.Passed != 1 || s.Wrong != 2 || s.Errors != 1 || s.ErrorClasses["500"] != 1 || s.CorrectiveRetries != 1 {
		t.Errorf("summary = %+v", s)
	}
	if s.AnsweredAccuracy != 1.0/3 {
		t.Errorf("answered accuracy = %v, want 1/3", s.AnsweredAccuracy)
	}
}

// A model that takes no forced tool_choice shows up as one error class
// on every call, not as wrong answers.
func TestRun_ForcedToolChoiceRejected(t *testing.T) {
	fx := fixtures(t)
	fake := llmroutertest.NewAnthropicCompat("sk-ant-test-0000", Oracle(fx))
	fake.RejectForcedToolChoice = true
	defer fake.Close()
	rep := offline(t, fake, "claude-sonnet-5-5", Options{Cases: []string{"codex-by-name", "finished"}})
	if s := rep.Summary; s.Errors != 2 || s.ErrorClasses["400 tool_choice not supported"] != 2 || s.Wrong != 0 {
		t.Errorf("summary = %+v", s)
	}
}

func TestRun_BaseURLWithoutV1Refused(t *testing.T) {
	tier := liveTier("claude-haiku-5-5")
	tier.BaseURL = "https://api.anthropic.com"
	if _, err := Run(context.Background(), fixtures(t), Options{Tier: tier}); err == nil {
		t.Fatal("Run started on a base URL that 404s every call")
	}
}

// The budget stops the run before a call that could pass it.
func TestRun_BudgetStopsEarly(t *testing.T) {
	fx := fixtures(t)
	fake := llmroutertest.NewAnthropicCompat("sk-ant-test-0000", func(r llmroutertest.Request) llmroutertest.Reply {
		rep := Oracle(fx)(r)
		rep.PromptTokens, rep.CompletionTokens = 1_000_000, 0 // $1 a call at $1/MTok
		return rep
	})
	defer fake.Close()
	rep := offline(t, fake, "custom-model", Options{Price: Price{InputPerMTok: 1}, BudgetUSD: 3.5})
	if s := rep.Summary; s.Calls != 3 || s.CostUSD != 3 || !strings.HasPrefix(s.StoppedEarly, "budget") {
		t.Errorf("summary = %+v", s)
	}
}

// The budget is a soft cap, judged by the dearest call so far: a call
// dearer than any before it can take the spend past it, by at most its
// own cost. The provider's spend limit is the hard cap.
func TestRun_BudgetIsSoft(t *testing.T) {
	fx := fixtures(t)
	var n atomic.Int32
	fake := llmroutertest.NewAnthropicCompat("sk-ant-test-0000", func(r llmroutertest.Request) llmroutertest.Reply {
		rep := Oracle(fx)(r)
		rep.PromptTokens, rep.CompletionTokens = 1_000_000, 0 // $1 a call at $1/MTok
		if n.Add(1) == 3 {
			rep.PromptTokens = 3_000_000 // the third costs $3
		}
		return rep
	})
	defer fake.Close()
	rep := offline(t, fake, "custom-model", Options{Price: Price{InputPerMTok: 1}, BudgetUSD: 3.5})
	// $1, $1, then the third starts at $2 spent + $1 dearest ≤ $3.5 and
	// ends at $5: past the budget by $1.5, less than the call's own $3.
	if s := rep.Summary; s.Calls != 3 || s.CostUSD != 5 || !strings.HasPrefix(s.StoppedEarly, "budget") {
		t.Errorf("summary = %+v", s)
	}
}

// A negative, NaN or infinite price or budget would defeat the budget
// (a negative price lowers the spend, NaN never compares over it): Run
// refuses to start.
func TestRun_RefusesBadPriceOrBudget(t *testing.T) {
	fx := fixtures(t)
	fake := llmroutertest.NewAnthropicCompat("sk-ant-test-0000", Oracle(fx))
	defer fake.Close()
	for name, o := range map[string]Options{
		"negative input price": {Price: Price{InputPerMTok: -1, OutputPerMTok: 5}},
		"NaN output price":     {Price: Price{InputPerMTok: 1, OutputPerMTok: math.NaN()}},
		"infinite input price": {Price: Price{InputPerMTok: math.Inf(1)}},
		"negative budget":      {BudgetUSD: -1},
		"NaN budget":           {BudgetUSD: math.NaN()},
		"infinite budget":      {BudgetUSD: math.Inf(1)},
	} {
		o.Tier, o.HTTPClient, o.Offline, o.MaxCalls = liveTier("claude-haiku-5-5"), fake.Client(), true, 1
		if _, err := Run(context.Background(), fx, o); err == nil {
			t.Errorf("%s: Run started", name)
		}
	}
}

func TestRun_MaxCallsAndPace(t *testing.T) {
	fx := fixtures(t)
	fake := llmroutertest.NewAnthropicCompat("sk-ant-test-0000", Oracle(fx))
	defer fake.Close()
	var slept []time.Duration
	rep := offline(t, fake, "claude-haiku-5-5", Options{MaxCalls: 2, Pace: time.Second,
		sleep: func(ctx context.Context, d time.Duration) error { slept = append(slept, d); return nil }})
	if rep.Summary.Calls != 2 || len(slept) != 2 || !strings.HasPrefix(rep.Summary.StoppedEarly, "call cap") {
		t.Errorf("calls %d, slept %v, stopped %q", rep.Summary.Calls, slept, rep.Summary.StoppedEarly)
	}
}

// An unpriced model is still scored; its cost is reported as unknown.
func TestRun_UnknownPrice(t *testing.T) {
	fx := fixtures(t)
	fake := llmroutertest.NewAnthropicCompat("sk-ant-test-0000", Oracle(fx))
	defer fake.Close()
	rep := offline(t, fake, "some-other-model", Options{Cases: []string{"finished"}})
	var md bytes.Buffer
	rep.WriteMarkdown(&md)
	if rep.Price != nil || rep.Summary.CostUSD != 0 || !strings.Contains(md.String(), "unknown (no price") {
		t.Errorf("price %v cost %v\n%s", rep.Price, rep.Summary.CostUSD, md.String())
	}
}

// A budget needs a price to count against: without one, Run refuses to
// start rather than run with the budget never checked.
func TestRun_BudgetNeedsAPrice(t *testing.T) {
	fx := fixtures(t)
	fake := llmroutertest.NewAnthropicCompat("sk-ant-test-0000", Oracle(fx))
	defer fake.Close()
	o := Options{Tier: liveTier("some-other-model"), HTTPClient: fake.Client(), Offline: true, MaxCalls: 1, BudgetUSD: 0.01}
	if _, err := Run(context.Background(), fx, o); err == nil {
		t.Fatal("Run started with a budget and no price")
	}
}

func TestRun_RateLimitedThenAnswered(t *testing.T) {
	fx := fixtures(t)
	fake := llmroutertest.NewAnthropicCompat("sk-ant-test-0000", Oracle(fx))
	fake.RateLimitFirst = 1
	defer fake.Close()
	rep := offline(t, fake, "claude-haiku-5-5", Options{Cases: []string{"finished"}})
	if s := rep.Summary; s.Passed != 1 || s.HTTPAttempts != 2 || s.Errors != 0 {
		t.Errorf("summary = %+v", s)
	}
}

func TestPercentile(t *testing.T) {
	xs := []int64{50, 10, 40, 20, 30, 100, 90, 80, 70, 60}
	if p := percentile(xs, 50); p != 50 {
		t.Errorf("p50 = %d", p)
	}
	if p := percentile(xs, 95); p != 100 {
		t.Errorf("p95 = %d", p)
	}
	if p := percentile(nil, 50); p != 0 {
		t.Errorf("empty p50 = %d", p)
	}
}

func TestEstimateLive(t *testing.T) {
	rep := &Report{RunsPerCase: 1, Summary: Summary{Calls: 10, PromptTokens: 30_000}}
	e := EstimateLive(rep, Price{InputPerMTok: 1, OutputPerMTok: 5}, 3)
	if e.Calls != 30 || e.PromptTokensPerCall != 4000 {
		t.Fatalf("estimate = %+v", e)
	}
	// 4000 in at $1 + 1024 out at $5 = $0.00912 a call.
	if want := 30 * 0.00912; e.ExpectedUSD < want-1e-9 || e.ExpectedUSD > want+1e-9 || e.WorstCaseUSD != e.ExpectedUSD*2.5 {
		t.Errorf("estimate = %+v, want expected $%.4f", e, want)
	}
}

func TestWriteComparison(t *testing.T) {
	price := &Price{InputPerMTok: 1, OutputPerMTok: 5}
	base := &Report{Model: "claude-haiku-4-5", Price: price, RunsPerCase: 1,
		Cases:   []CaseResult{{Name: "a", Runs: 1, Passed: 1, Raw: []string{"ok"}}, {Name: "b", Runs: 1, Passed: 0, Raw: []string{"x"}}},
		Summary: Summary{Calls: 2, Accuracy: 0.5, LatencyP50MS: 900, CostPer1kCalls: 9}}
	cand := &Report{Model: "claude-haiku-5-5", Price: &Price{InputPerMTok: 0.1, OutputPerMTok: 0.5}, RunsPerCase: 1, Offline: true,
		Cases:   []CaseResult{{Name: "a", Runs: 1, Passed: 1, Raw: []string{"ok"}}, {Name: "b", Runs: 1, Passed: 1, Raw: []string{"use_workspace ws-api/codex"}}},
		Summary: Summary{Calls: 2, Accuracy: 1, LatencyP50MS: 700, CostPer1kCalls: 0.9}}
	var out bytes.Buffer
	WriteComparison(&out, base, cand)
	md := out.String()
	for _, want := range []string{"| Accuracy | 50.0% | 100.0% | +50.0 pts |", "| Latency p50 | 900 ms | 700 ms | -200 ms |",
		"| b | 0/1 | 1/1 | use_workspace ws-api/codex |", "claude-haiku-5-5 is an offline run", "| Cost per 1k calls | $9.000 | $0.900 | -8.100 |"} {
		if !strings.Contains(md, want) {
			t.Errorf("comparison lacks %q:\n%s", want, md)
		}
	}
	if strings.Contains(md, "| a |") {
		t.Errorf("an unchanged case is listed:\n%s", md)
	}
}

// The readiness run's F11 and F8 (LOOM-258, LOOM-255) as the router
// behaved before: a decision with no routing_text sends the agent the
// message verbatim, routing instructions included, and an answer that
// asks for a yes confirms nothing. Both are scored wrong.
func TestRun_ScoresRoutingTextAndModelConfirmation(t *testing.T) {
	fx := fixtures(t)
	oracle := Oracle(fx)
	fake := llmroutertest.NewAnthropicCompat("sk-ant-test-0000", func(r llmroutertest.Request) llmroutertest.Reply {
		if strings.Contains(r.User, "Message:\nactually, go ahead in billing") {
			return llmroutertest.Reply{ToolArgs: map[string]any{"action": "answer_directly",
				"direct_answer": `Please confirm starting new work in billing on buildbox. Reply "yes" to go ahead.`}}
		}
		reply := oracle(r)
		if args, ok := reply.ToolArgs.(map[string]any); ok {
			delete(args, "routing_text")
		}
		return reply
	})
	defer fake.Close()
	rep := offline(t, fake, "claude-haiku-5-5", Options{Cases: []string{"routing-text-", "no-model-authored-confirmation"}})
	got := map[string]CaseResult{}
	for _, c := range rep.Cases {
		got[c.Name] = c
	}
	for name, want := range map[string]string{
		"routing-text-stays-with-loomux": "want none of",
		"routing-text-new-workspace":     "want none of",
		"no-model-authored-confirmation": "asks for a confirmation no offer records",
	} {
		if c := got[name]; c.Wrong != 1 || !strings.Contains(strings.Join(c.Failures, ""), want) {
			t.Errorf("%s: %+v, want it scored wrong: %s", name, c, want)
		}
	}
}
