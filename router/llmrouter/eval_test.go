//go:build routereval

// Routing evals (LOOM-88/LOOM-87): real routing-model calls against
// fixed fixtures, to check the model actually uses what the prompt now
// tells it. Not part of the normal test run — they need the router
// model's credentials and cost a few cents:
//
//	LOOMUX_ROUTER_PRIMARY_BASE_URL=… LOOMUX_ROUTER_PRIMARY_API_KEY=… LOOMUX_ROUTER_PRIMARY_MODEL=… \
//	  go test -tags routereval ./router/llmrouter/ -run Eval -v
//
// Each case runs evalRuns times on the primary tier alone and must pass
// every time. A decision is scored after router.ApplyAffinity — what the
// router actually does with it — and the raw decision is logged too.
// ROUTEREVAL_OUT, if set, receives a JSON summary (no credentials).
// Calls are paced (ROUTEREVAL_PACE, default 20s) to stay under a
// free-tier tokens-per-minute limit; a call that errors (429, timeout)
// is retried up to twice and counted as an infrastructure error, never
// scored as a wrong decision. A case fails on a wrong decision, or if
// every attempt of a run errored.
package llmrouter

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Loomux/server/agents"
	"github.com/Loomux/server/router"
)

const evalRuns = 3

type evalCase struct {
	name     string
	message  string
	opts     router.DispatchOptions
	check    func(d router.Decision) error
	rawCheck func(d router.Decision) error // optional, on the decision before affinity
}

func evalFixtures() ([]router.WorkspaceSnapshot, []router.TargetSnapshot) {
	now := time.Now()
	h2 := now.Add(-2 * time.Hour)
	d3 := now.Add(-72 * time.Hour)
	workspaces := []router.WorkspaceSnapshot{
		{ID: "ws-api", Name: "api-server", Status: "idle", TargetName: "sc1", LastUsed: &h2,
			Description: "Go HTTP API for the main product", Tags: []string{"go", "backend"},
			Summary: "Added a token-bucket rate limiter to the HTTP middleware; all tests pass."},
		{ID: "ws-billing", Name: "billing", Status: "idle", TargetName: "sc1", LastUsed: &d3,
			Description: "Billing service: invoices and payments", Tags: []string{"go", "billing"},
			Summary: "Added cursor pagination to the /invoices endpoint."},
	}
	targets := []router.TargetSnapshot{
		{ID: "t-jet01", Name: "jet01", Kind: "remote", Agents: map[string]bool{"claude-code": false, "codex": false}},
		{ID: "t-sc1", Name: "sc1", Kind: "remote",
			Agents:        map[string]bool{"claude-code": true, "codex": true},
			AgentVersions: map[string]string{"claude-code": "2.1.4 (Claude Code)", "codex": "codex-cli 0.40.0"}},
	}
	return workspaces, targets
}

func openAPITask(lastReply string) *router.OpenTaskSnapshot {
	return &router.OpenTaskSnapshot{TaskID: "task-1", WorkspaceID: "ws-api", WorkspaceName: "api-server",
		AgentType: "claude-code", Status: "awaiting-input", LastReply: lastReply}
}

func wantAction(d router.Decision, a router.DecisionAction) error {
	if d.Action != a {
		return fmt.Errorf("action %s, want %s", d.Action, a)
	}
	return nil
}

func wantWorkspace(id, agent string) func(router.Decision) error {
	return func(d router.Decision) error {
		if err := wantAction(d, router.ActionUseWorkspace); err != nil {
			return err
		}
		if d.WorkspaceID != id {
			return fmt.Errorf("workspace %s, want %s", d.WorkspaceID, id)
		}
		if agent != "" && d.AgentType != agent {
			return fmt.Errorf("agent %s, want %s", d.AgentType, agent)
		}
		return nil
	}
}

func wantCommand(target, contains string) func(router.Decision) error {
	return func(d router.Decision) error {
		if err := wantAction(d, router.ActionRunCommand); err != nil {
			return err
		}
		if d.TargetID != target {
			return fmt.Errorf("target %s, want %s", d.TargetID, target)
		}
		if contains != "" && !strings.Contains(d.Command, contains) {
			return fmt.Errorf("command %q, want it to contain %q", d.Command, contains)
		}
		return nil
	}
}

func evalCases() []evalCase {
	return []evalCase{
		{
			name:    "01-provision-where-an-agent-is-installed",
			message: "start a new empty python project called scratch-py",
			check: func(d router.Decision) error {
				if err := wantAction(d, router.ActionProvisionWorkspace); err != nil {
					return err
				}
				if d.NewWorkspace.TargetID != "t-sc1" {
					return fmt.Errorf("target %s, want t-sc1 (jet01 has no agents)", d.NewWorkspace.TargetID)
				}
				return nil
			},
		},
		{name: "02-disk-free-is-a-command", message: "how much disk is free on jet01?", check: wantCommand("t-jet01", "df")},
		{name: "03-codex-by-name", message: "use codex to add a README in api-server", check: wantWorkspace("ws-api", "codex")},
		{name: "04-claude-by-name", message: "ask claude to fix the failing test in api-server", check: wantWorkspace("ws-api", "claude-code")},
		{name: "05-reuse-matching-workspace", message: "the rate limiter lets bursts through, can you tighten it?", check: wantWorkspace("ws-api", "")},
		{
			name:    "06-yes-continues-open-task",
			message: "yes, go ahead",
			opts: router.DispatchOptions{
				History: []router.ConversationTurn{
					{Role: "user", Content: "write the migration for the new sessions table in api-server"},
					{Role: "assistant", Content: "I've drafted the migration. Should I also update the tests?"},
				},
				OpenTask: openAPITask("I've drafted the migration. Should I also update the tests?"),
			},
			check: wantWorkspace("ws-api", "claude-code"),
			rawCheck: func(d router.Decision) error {
				if d.LeaveOpenTask {
					return fmt.Errorf("leave_open_task set for an answer to the agent's question")
				}
				return nil
			},
		},
		{
			name:    "07-unrelated-question-leaves-open-task",
			message: "what's the uptime on jet01?",
			opts: router.DispatchOptions{
				History: []router.ConversationTurn{
					{Role: "user", Content: "write the migration for the new sessions table in api-server"},
					{Role: "assistant", Content: "I've drafted the migration. Should I also update the tests?"},
				},
				OpenTask: openAPITask("I've drafted the migration. Should I also update the tests?"),
			},
			check: wantCommand("t-jet01", ""),
		},
		{
			name:    "08-option-2-continues-open-task",
			message: "do option 2",
			opts: router.DispatchOptions{
				History: []router.ConversationTurn{
					{Role: "user", Content: "the /search endpoint in api-server is slow, fix it"},
					{Role: "assistant", Content: "I can (1) add an index on title, (2) rewrite the query to avoid the join, or (3) cache results for 60s. Which do you want?"},
				},
				OpenTask: openAPITask("I can (1) add an index on title, (2) rewrite the query to avoid the join, or (3) cache results for 60s. Which do you want?"),
			},
			check: wantWorkspace("ws-api", "claude-code"),
		},
		{
			name:    "09-same-command-elsewhere",
			message: "now the same on sc1",
			opts: router.DispatchOptions{History: []router.ConversationTurn{
				{Role: "user", Content: "run df -h on jet01"},
				{Role: "assistant", Content: "jet01: / is 71% full (34G of 48G used); /data is 12% full."},
			}},
			check: wantCommand("t-sc1", "df -h"),
		},
		{
			name:    "10-follow-up-goes-to-last-workspace",
			message: "and add tests for that too",
			opts: router.DispatchOptions{
				History: []router.ConversationTurn{
					{Role: "user", Content: "add pagination to the invoices endpoint in billing"},
					{Role: "assistant", Content: "Done: added cursor pagination to /invoices, with next_cursor in the response."},
				},
				LastWorkspaceID: "ws-billing", LastWorkspaceName: "billing",
			},
			check: wantWorkspace("ws-billing", ""),
		},
	}
}

type evalResult struct {
	Case        string   `json:"case"`
	Passed      int      `json:"passed"`
	Runs        int      `json:"runs"`
	InfraErrors int      `json:"infra_errors"`
	Failures    []string `json:"failures,omitempty"`
	Raw         []string `json:"raw_decisions"`
}

func TestEvalRouting(t *testing.T) {
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Skipf("router model not configured: %v", err)
	}
	cfg.Escalation = nil // score the primary tier on its own
	descriptions := map[string]string{
		"claude-code": agents.ClaudeCode().Description,
		"codex":       agents.Codex().Description,
	}
	m, err := New(cfg, []string{"claude-code", "codex"}, WithAgentDescriptions(descriptions))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	workspaces, targets := evalFixtures()
	pace := 20 * time.Second
	if raw := os.Getenv("ROUTEREVAL_PACE"); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil {
			pace = d
		}
	}
	decide := func(c evalCase, res *evalResult) (router.Decision, error) {
		var err error
		for attempt := 0; attempt < 3; attempt++ {
			time.Sleep(pace)
			o := c.opts
			var d router.Decision
			d, err = m.Decide(context.Background(), c.message, workspaces, targets, func(do *router.DispatchOptions) { *do = o })
			if err == nil {
				return d, nil
			}
			res.InfraErrors++
		}
		return router.Decision{}, err
	}

	only := os.Getenv("ROUTEREVAL_CASES") // comma-separated name prefixes, for iterating on a few
	var results []evalResult
	for _, c := range evalCases() {
		if only != "" && !matchesAny(c.name, strings.Split(only, ",")) {
			continue
		}
		res := evalResult{Case: c.name, Runs: evalRuns}
		for run := range evalRuns {
			raw, err := decide(c, &res)
			if err != nil {
				res.Failures = append(res.Failures, fmt.Sprintf("run %d: every attempt errored: %s", run+1, errClass(err)))
				res.Raw = append(res.Raw, "error")
				continue
			}
			res.Raw = append(res.Raw, describeDecision(raw))
			effective, _ := router.ApplyAffinity(raw, c.opts.OpenTask)
			err = c.check(effective)
			if err == nil && c.rawCheck != nil {
				err = c.rawCheck(raw)
			}
			if err != nil {
				res.Failures = append(res.Failures, fmt.Sprintf("run %d: %v", run+1, err))
				continue
			}
			res.Passed++
		}
		t.Logf("%-44s %d/%d  (infra errors %d)  raw: %s", c.name, res.Passed, res.Runs, res.InfraErrors, strings.Join(res.Raw, " | "))
		for _, f := range res.Failures {
			t.Logf("    %s", f)
		}
		results = append(results, res)
	}

	if out := os.Getenv("ROUTEREVAL_OUT"); out != "" {
		data, _ := json.MarshalIndent(map[string]any{"model": cfg.Primary.Model, "runs_per_case": evalRuns, "results": results}, "", "  ")
		if err := os.WriteFile(out, data, 0o644); err != nil {
			t.Errorf("write %s: %v", out, err)
		}
	}
	for _, r := range results {
		if r.Passed != r.Runs {
			t.Errorf("%s: %d/%d", r.Case, r.Passed, r.Runs)
		}
	}
}

// describeDecision is a one-line, credential-free view of a decision.
func describeDecision(d router.Decision) string {
	s := string(d.Action)
	switch d.Action {
	case router.ActionAnswerDirectly:
		a := []rune(d.DirectAnswer)
		if len(a) > 60 {
			a = append(a[:60], '…')
		}
		s += fmt.Sprintf(" %q", string(a))
	case router.ActionUseWorkspace:
		s += fmt.Sprintf(" %s/%s", d.WorkspaceID, d.AgentType)
	case router.ActionProvisionWorkspace:
		s += fmt.Sprintf(" %s on %s/%s", d.NewWorkspace.Name, d.NewWorkspace.TargetID, d.AgentType)
	case router.ActionRunCommand:
		s += fmt.Sprintf(" %q on %s", d.Command, d.TargetID)
	}
	if d.LeaveOpenTask {
		s += " (leave_open_task)"
	}
	return s
}

// errClass keeps an infrastructure error short and free of provider
// account details.
func errClass(err error) string {
	switch msg := err.Error(); {
	case strings.Contains(msg, "429"):
		return "rate limited (429)"
	case strings.Contains(msg, "deadline exceeded"):
		return "timed out"
	default:
		if len(msg) > 120 {
			msg = msg[:120] + "…"
		}
		return msg
	}
}

func matchesAny(name string, prefixes []string) bool {
	for _, p := range prefixes {
		if p != "" && strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// TestEvalRelay checks the relay's done signal against the real model:
// a turn ending in a question to the user is never done. Scored as the
// router acts on it — done only if the model says so and the reply
// doesn't ask anything (router.AsksUser) — with the raw signal logged.
func TestEvalRelay(t *testing.T) {
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Skipf("router model not configured: %v", err)
	}
	cfg.Escalation = nil
	m, err := New(cfg, []string{"claude-code"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pace := 20 * time.Second
	if raw := os.Getenv("ROUTEREVAL_PACE"); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil {
			pace = d
		}
	}
	cases := []struct {
		name     string
		captured string
		wantDone bool
		// LOOM-112: the turn's message and the summary before it, and
		// words of which the reply must contain at least one (lowercase).
		message, summary string
		replyHasAny      []string
		anyDone          bool // done isn't scored: either reading is fair
	}{
		{"asks-a-question", "> ask me whether to create hello.txt, then wait\n\n● Should I create hello.txt?\n\n> ", false, "", "", nil, false},
		{"offers-options", "> the search endpoint is slow\n\n● I found three options: (1) add an index on title, (2) rewrite the query, (3) cache results. Which do you want?\n\n> ", false, "", "", nil, false},
		{"finished", "> create hello.txt containing hi\n\n● Write(hello.txt)\n  ⎿  Wrote 1 line to hello.txt\n\n● Created hello.txt containing \"hi\".\n\n> ", true, "", "", nil, false},
		// The output alone says nothing was done; with the message, the
		// reply must say the asked-for deploy didn't happen.
		{"work-not-shown", "> deploy\n\n● Bash(ssh deploy@prod ./deploy.sh)\n  ⎿  ssh: connect to host prod port 22: Connection timed out\n\n● I couldn't reach the prod host.\n\n> ", false,
			"deploy the api to prod", "Added a rate limiter to the HTTP middleware; all tests pass.",
			[]string{"couldn't", "could not", "can't", "cannot", "unable", "timed out", "not deployed", "failed"}, true},
		// A terse answer only makes sense with the question.
		{"answers-the-message", "> which port\n\n● 8080.\n\n> ", true,
			"which port does the api server listen on?", "", []string{"8080"}, false},
	}
	failed := 0
	for _, c := range cases {
		passed := 0
		var got []string
		for range evalRuns {
			time.Sleep(pace)
			res, err := m.Relay(context.Background(), router.RelayInput{
				Captured: c.captured, UserMessage: c.message, PreviousSummary: c.summary, AgentType: "claude-code"})
			if err != nil {
				got = append(got, "error: "+errClass(err))
				continue
			}
			effective := res.Done && !router.AsksUser(res.Reply)
			got = append(got, fmt.Sprintf("done=%v effective=%v reply=%q", res.Done, effective, res.Reply))
			replyOK := len(c.replyHasAny) == 0
			for _, w := range c.replyHasAny {
				replyOK = replyOK || strings.Contains(strings.ToLower(res.Reply), w)
			}
			if (c.anyDone || effective == c.wantDone) && replyOK {
				passed++
			}
		}
		t.Logf("relay %-18s %d/%d  %s", c.name, passed, evalRuns, strings.Join(got, " | "))
		if passed != evalRuns {
			failed++
		}
	}
	if failed > 0 {
		t.Errorf("%d relay cases below %d/%d", failed, evalRuns, evalRuns)
	}
}
