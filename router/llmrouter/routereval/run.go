package routereval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Loomux/server/credentials"
	"github.com/Loomux/server/router"
	"github.com/Loomux/server/router/llmrouter"
)

// Options configures a Run.
type Options struct {
	// Tier is the endpoint scored, on its own: no escalation behind it.
	Tier llmrouter.Tier
	// Runs is how often each case runs (default 1).
	Runs int
	// Pace is the wait before each call, for a rate limit.
	Pace time.Duration
	// Timeout bounds one call, its corrective retry included; default
	// 45s, the escalation tier's own.
	Timeout time.Duration
	// Price prices the tokens; zero looks the model up (PriceFor), and
	// an unknown model's cost isn't reported.
	Price Price
	// BudgetUSD, if positive, stops the run before a call that could
	// take the spend past it (judged by the dearest call so far).
	BudgetUSD float64
	// MaxCalls, if positive, stops the run after that many calls.
	MaxCalls int
	// Cases, if set, runs only the cases whose names start with one of
	// these.
	Cases []string
	// HTTPClient carries the requests; nil is the SDKs' default. An
	// offline run passes a fake's (llmroutertest.AnthropicCompat.Client).
	HTTPClient *http.Client
	// Offline marks the report as a run against a fake.
	Offline bool
	// Log, if set, gets one line per call as it completes.
	Log func(format string, args ...any)

	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error
}

// Run scores o.Tier on every case of fx, o.Runs times each, and reports
// what it found. Calls run one at a time, so latency is the endpoint's
// own. It stops early, with what it has, when ctx ends or the budget or
// call cap is reached (Summary.StoppedEarly says why). An error means
// the run couldn't start.
func Run(ctx context.Context, fx *Fixtures, o Options) (*Report, error) {
	if err := o.Tier.Validate(); err != nil {
		return nil, err
	}
	if o.Runs <= 0 {
		o.Runs = 1
	}
	if o.Timeout <= 0 {
		o.Timeout = 45 * time.Second
	}
	if o.now == nil {
		o.now = time.Now
	}
	if o.sleep == nil {
		o.sleep = sleepCtx
	}
	price, priced := o.Price, o.Price != (Price{})
	if !priced {
		price, priced = PriceFor(o.Tier.Model)
	}
	// Errors are scrubbed of the key before they reach the report.
	credentials.AddSystemSecret(o.Tier.APIKey)

	start := o.now()
	rep := &Report{Model: o.Tier.Model, Provider: string(providerOf(o.Tier)), Endpoint: endpointOf(o.Tier.BaseURL),
		Offline: o.Offline, RunsPerCase: o.Runs, Started: start, Fixtures: fx.Description}
	if priced {
		rep.Price = &price
	}
	agentNames, descriptions := fx.World.agentTypes()
	workspaces, targets := fx.World.snapshots(start)

	r := &runner{o: o, price: price, priced: priced, rep: rep, agentNames: agentNames, descriptions: descriptions}
	for _, c := range fx.Decide {
		if !r.selected(c.Name) {
			continue
		}
		res := r.caseResult(c.Name, "decide", c.Category)
		opts := c.options(fx.World)
		for range o.Runs {
			if !r.next(ctx) {
				break
			}
			r.call(ctx, res, func(ctx context.Context, m *llmrouter.Model) (string, error) {
				d, err := m.Decide(ctx, c.Message, workspaces, targets, func(do *router.DispatchOptions) { *do = opts })
				if err != nil {
					return "", err
				}
				return describeDecision(d), scoreDecision(c, d, opts.OpenTask)
			})
		}
	}
	for _, c := range fx.Relay {
		if !r.selected(c.Name) {
			continue
		}
		res := r.caseResult(c.Name, "relay", "relay")
		for range o.Runs {
			if !r.next(ctx) {
				break
			}
			r.call(ctx, res, func(ctx context.Context, m *llmrouter.Model) (string, error) {
				out, err := m.Relay(ctx, router.RelayInput{Captured: c.Captured, UserMessage: c.UserMessage,
					PreviousSummary: c.PreviousSummary, AgentType: c.AgentType})
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("done=%v reply=%q", out.Done, clip(out.Reply, 80)), scoreRelay(c, out)
			})
		}
	}
	rep.Finished = o.now()
	rep.summarize()
	return rep, nil
}

type runner struct {
	o            Options
	price        Price
	priced       bool
	rep          *Report
	agentNames   []string
	descriptions map[string]string
	calls        int
	spent        float64
	dearest      float64
}

func (r *runner) selected(name string) bool {
	if len(r.o.Cases) == 0 {
		return true
	}
	for _, p := range r.o.Cases {
		if p != "" && strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

func (r *runner) caseResult(name, kind, category string) *CaseResult {
	r.rep.Cases = append(r.rep.Cases, CaseResult{Name: name, Kind: kind, Category: category})
	return &r.rep.Cases[len(r.rep.Cases)-1]
}

// next reports whether another call may run, pacing it, and records why
// not.
func (r *runner) next(ctx context.Context) bool {
	if r.rep.Summary.StoppedEarly != "" {
		return false
	}
	switch {
	case ctx.Err() != nil:
		r.rep.Summary.StoppedEarly = "interrupted"
	case r.o.MaxCalls > 0 && r.calls >= r.o.MaxCalls:
		r.rep.Summary.StoppedEarly = fmt.Sprintf("call cap (%d) reached", r.o.MaxCalls)
	case r.o.BudgetUSD > 0 && r.priced && r.spent+r.dearest > r.o.BudgetUSD:
		r.rep.Summary.StoppedEarly = fmt.Sprintf("budget: $%.4f spent, the next call could pass $%.2f", r.spent, r.o.BudgetUSD)
	case r.o.Pace > 0 && r.o.sleep(ctx, r.o.Pace) != nil:
		r.rep.Summary.StoppedEarly = "interrupted"
	}
	return r.rep.Summary.StoppedEarly == ""
}

// call runs one Decide or Relay on a fresh Model — no circuit-breaker
// state carried between calls — through a meter that sees every HTTP
// attempt, and records the outcome in res. do returns a one-line view
// of the answer and whether it was right (nil) or wrong.
func (r *runner) call(ctx context.Context, res *CaseResult, do func(context.Context, *llmrouter.Model) (string, error)) {
	r.calls++
	met := &meter{next: r.o.HTTPClient}
	m, err := llmrouter.New(llmrouter.Config{Primary: r.o.Tier}, r.agentNames,
		llmrouter.WithAgentDescriptions(r.descriptions), llmrouter.WithPrimaryTimeout(r.o.Timeout),
		llmrouter.WithHTTPClient(met.client()))
	if err != nil {
		res.record(outcomeError, "", "config: "+err.Error(), 0, met)
		return
	}
	start := r.o.now()
	view, err := do(ctx, m)
	elapsed := r.o.now().Sub(start)
	cost := 0.0
	if r.priced {
		cost = r.price.Cost(met.prompt, met.completion)
		r.spent += cost
		r.dearest = max(r.dearest, cost)
	}
	res.CostUSD += cost
	var wrong *wrongAnswer
	switch {
	case err == nil:
		res.record(outcomePass, view, "", elapsed, met)
	case errors.As(err, &wrong):
		res.record(outcomeWrong, view, wrong.Error(), elapsed, met)
	case met.lastStatus/100 == 2 && ctx.Err() == nil && !errors.Is(err, context.DeadlineExceeded):
		// The endpoint answered, but nothing usable came back even
		// after the corrective retry: the model's fault, not the
		// endpoint's.
		res.record(outcomeWrong, "unusable", scrub(err), elapsed, met)
	default:
		res.record(outcomeError, "error", errorClass(err, met.lastStatus), elapsed, met)
	}
	if r.o.Log != nil {
		last := res.Raw[len(res.Raw)-1]
		r.o.Log("%-44s %-5s %6dms  %s", res.Name, res.lastOutcome, elapsed.Milliseconds(), clip(last, 100))
	}
}

// wrongAnswer is a usable answer that isn't the expected one.
type wrongAnswer struct{ msg string }

func (e *wrongAnswer) Error() string { return e.msg }

func wrongf(format string, args ...any) error { return &wrongAnswer{fmt.Sprintf(format, args...)} }

// scoreDecision checks d as the router would act on it.
func scoreDecision(c DecideCase, d router.Decision, open *router.OpenTaskSnapshot) error {
	e := c.Expect
	if e.LeaveOpenTask != nil && d.LeaveOpenTask != *e.LeaveOpenTask {
		return wrongf("leave_open_task %v, want %v", d.LeaveOpenTask, *e.LeaveOpenTask)
	}
	eff, _ := router.ApplyAffinity(d, open)
	switch {
	case eff.Action != e.Action:
		return wrongf("action %s, want %s", eff.Action, e.Action)
	case e.WorkspaceID != "" && eff.WorkspaceID != e.WorkspaceID:
		return wrongf("workspace %s, want %s", eff.WorkspaceID, e.WorkspaceID)
	case e.AgentType != "" && eff.AgentType != e.AgentType:
		return wrongf("agent %s, want %s", eff.AgentType, e.AgentType)
	case e.TargetID != "" && eff.TargetID != e.TargetID:
		return wrongf("target %s, want %s", eff.TargetID, e.TargetID)
	case len(e.CommandAny) > 0 && !containsAny(eff.Command, e.CommandAny):
		return wrongf("command %q, want one of %q", eff.Command, e.CommandAny)
	case len(e.ProvisionTargetAny) > 0 && !containsExact(e.ProvisionTargetAny, eff.NewWorkspace.TargetID):
		return wrongf("new workspace on %s, want one of %v", eff.NewWorkspace.TargetID, e.ProvisionTargetAny)
	case e.ProvisionKind != "" && eff.NewWorkspace.Kind != e.ProvisionKind:
		return wrongf("provision kind %s, want %s", eff.NewWorkspace.Kind, e.ProvisionKind)
	case e.GitRemote != "" && eff.NewWorkspace.GitRemote != e.GitRemote:
		return wrongf("git remote %q, want %q", eff.NewWorkspace.GitRemote, e.GitRemote)
	}
	return nil
}

// scoreRelay checks a relay as the router would act on it: done only if
// the model says so and the reply asks the user nothing.
func scoreRelay(c RelayCase, out router.RelayResult) error {
	done := out.Done && !router.AsksUser(out.Reply)
	if c.ExpectDone != nil && done != *c.ExpectDone {
		return wrongf("done %v (model said %v), want %v", done, out.Done, *c.ExpectDone)
	}
	if len(c.ReplyHasAny) > 0 && !containsAny(out.Reply, c.ReplyHasAny) {
		return wrongf("reply %q has none of %q", clip(out.Reply, 80), c.ReplyHasAny)
	}
	return nil
}

func containsAny(s string, subs []string) bool {
	s = strings.ToLower(s)
	for _, sub := range subs {
		if strings.Contains(s, strings.ToLower(sub)) {
			return true
		}
	}
	return false
}

func containsExact(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

// describeDecision is a one-line view of d.
func describeDecision(d router.Decision) string {
	s := string(d.Action)
	switch d.Action {
	case router.ActionAnswerDirectly:
		s += fmt.Sprintf(" %q", clip(d.DirectAnswer, 60))
	case router.ActionUseWorkspace:
		s += fmt.Sprintf(" %s/%s", d.WorkspaceID, d.AgentType)
	case router.ActionProvisionWorkspace:
		s += fmt.Sprintf(" %s(%s) on %s/%s", d.NewWorkspace.Name, d.NewWorkspace.Kind, d.NewWorkspace.TargetID, d.AgentType)
	case router.ActionRunCommand:
		s += fmt.Sprintf(" %q on %s", clip(d.Command, 60), d.TargetID)
	}
	if d.LeaveOpenTask {
		s += " (leave_open_task)"
	}
	return s
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// errorClass reduces an endpoint failure to something to count: its
// HTTP status and, for the ones a trial expects, what it likely means.
func errorClass(err error, status int) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "deadline exceeded"):
		return "timeout"
	case status == http.StatusBadRequest:
		msg := strings.ToLower(err.Error())
		if strings.Contains(msg, "tool_choice") {
			return "400 tool_choice not supported"
		}
		return "400 bad request: " + clip(scrub(err), 160)
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return fmt.Sprintf("%d key rejected", status)
	case status == http.StatusNotFound:
		return "404 (base URL or model id)"
	case status == http.StatusTooManyRequests:
		return "429 rate limited"
	case status != 0:
		return fmt.Sprintf("%d", status)
	}
	return clip(scrub(err), 160)
}

func scrub(err error) string { return credentials.RedactValues(err.Error(), nil) }

func providerOf(t llmrouter.Tier) llmrouter.Provider {
	p, _ := llmrouter.ParseProvider(string(t.Provider))
	return p
}

// endpointOf is a base URL without anything secret in it.
func endpointOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Scheme + "://" + u.Host + u.Path
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// meter is the transport under one call: it counts HTTP attempts (the
// SDK's own retries and the corrective retry included), keeps the last
// status, and adds up the token usage each answer reports, in either
// protocol's shape.
type meter struct {
	next *http.Client

	mu                 sync.Mutex
	attempts, answered int
	lastStatus         int
	prompt, completion int64
}

func (m *meter) client() *http.Client {
	base := http.DefaultTransport
	if m.next != nil && m.next.Transport != nil {
		base = m.next.Transport
	}
	return &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		resp, err := base.RoundTrip(req)
		m.mu.Lock()
		defer m.mu.Unlock()
		m.attempts++
		if err != nil {
			return nil, err
		}
		m.lastStatus = resp.StatusCode
		if resp.StatusCode/100 != 2 {
			return resp, nil
		}
		m.answered++
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(body))
		if readErr == nil {
			m.addUsage(body)
		}
		return resp, nil
	})}
}

func (m *meter) addUsage(body []byte) {
	var u struct {
		Usage struct {
			// Chat Completions.
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			// Messages API.
			InputTokens              int64 `json:"input_tokens"`
			OutputTokens             int64 `json:"output_tokens"`
			CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(body, &u) != nil {
		return
	}
	// Cache reads and writes are priced as plain input here, an
	// approximation (reads cost a tenth, 5-minute writes a quarter
	// more); the OpenAI-compatible endpoint doesn't cache at all.
	m.prompt += u.Usage.PromptTokens + u.Usage.InputTokens + u.Usage.CacheReadInputTokens + u.Usage.CacheCreationInputTokens
	m.completion += u.Usage.CompletionTokens + u.Usage.OutputTokens
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
