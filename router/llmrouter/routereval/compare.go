package routereval

import (
	"fmt"
	"io"
	"strings"

	"github.com/Loomux/server/router/llmrouter/llmroutertest"
)

// WriteComparison writes base and cand side by side as Markdown: the
// totals, then every case whose pass count differs.
func WriteComparison(w io.Writer, base, cand *Report) {
	b, c := base.Summary, cand.Summary
	fmt.Fprintf(w, "# Router tier comparison: %s vs %s\n\n", base.Model, cand.Model)
	for _, r := range []*Report{base, cand} {
		if r.Offline {
			fmt.Fprintf(w, "> %s is an offline run: its numbers check the harness, not the model.\n\n", r.Model)
		}
		if r.Summary.StoppedEarly != "" {
			fmt.Fprintf(w, "> %s stopped early (%s): compare with care.\n\n", r.Model, r.Summary.StoppedEarly)
		}
	}
	fmt.Fprintf(w, "| Metric | %s | %s | Change |\n|---|---|---|---|\n", base.Model, cand.Model)
	pct := func(name string, x, y float64) {
		fmt.Fprintf(w, "| %s | %.1f%% | %.1f%% | %+.1f pts |\n", name, 100*x, 100*y, 100*(y-x))
	}
	ms := func(name string, x, y int64) {
		fmt.Fprintf(w, "| %s | %d ms | %d ms | %+d ms |\n", name, x, y, y-x)
	}
	fmt.Fprintf(w, "| Calls | %d | %d | |\n", b.Calls, c.Calls)
	pct("Accuracy", b.Accuracy, c.Accuracy)
	pct("Accuracy, answered only", b.AnsweredAccuracy, c.AnsweredAccuracy)
	pct("Decide accuracy", b.DecideAccuracy, c.DecideAccuracy)
	pct("Relay accuracy", b.RelayAccuracy, c.RelayAccuracy)
	fmt.Fprintf(w, "| Errors | %d | %d | %+d |\n", b.Errors, c.Errors, c.Errors-b.Errors)
	fmt.Fprintf(w, "| Corrective retries | %d | %d | %+d |\n", b.CorrectiveRetries, c.CorrectiveRetries, c.CorrectiveRetries-b.CorrectiveRetries)
	ms("Latency p50", b.LatencyP50MS, c.LatencyP50MS)
	ms("Latency p95", b.LatencyP95MS, c.LatencyP95MS)
	if base.Price != nil && cand.Price != nil {
		fmt.Fprintf(w, "| Cost per 1k calls | $%.3f | $%.3f | %+.3f |\n", b.CostPer1kCalls, c.CostPer1kCalls, c.CostPer1kCalls-b.CostPer1kCalls)
	} else {
		fmt.Fprintf(w, "| Cost per 1k calls | %s | %s | |\n", costCell(base), costCell(cand))
	}

	byName := map[string]CaseResult{}
	for _, r := range base.Cases {
		byName[r.Name] = r
	}
	var rows []string
	for _, r := range cand.Cases {
		o, ok := byName[r.Name]
		if !ok || ratio(o.Passed, o.Runs) == ratio(r.Passed, r.Runs) {
			continue
		}
		rows = append(rows, fmt.Sprintf("| %s | %d/%d | %d/%d | %s |", r.Name, o.Passed, o.Runs, r.Passed, r.Runs,
			mdCell(strings.Join(r.Raw, " · "))))
	}
	if len(rows) == 0 {
		fmt.Fprintf(w, "\nNo case changed.\n")
		return
	}
	fmt.Fprintf(w, "\nCases that changed:\n\n| Case | %s | %s | %s answered |\n|---|---|---|---|\n%s\n",
		base.Model, cand.Model, cand.Model, strings.Join(rows, "\n"))
}

func costCell(r *Report) string {
	if r.Price == nil {
		return "unknown"
	}
	return fmt.Sprintf("$%.3f", r.Summary.CostPer1kCalls)
}

// Oracle answers a fake endpoint's requests with each case's expected
// decision, so an offline run exercises the whole harness — requests,
// parsing, validation, scoring, metering — and should score 100%. A
// request it can't place gets a plain-text reply (no tool call).
func Oracle(fx *Fixtures) func(llmroutertest.Request) llmroutertest.Reply {
	return func(req llmroutertest.Request) llmroutertest.Reply {
		if args, ok := oracleDecide(fx, req.User); ok {
			return llmroutertest.Reply{ToolArgs: args}
		}
		for _, c := range fx.Relay {
			if strings.Contains(req.User, c.Captured) {
				reply := "Done."
				if len(c.ReplyHasAny) > 0 {
					reply = "Result: " + c.ReplyHasAny[0] + "."
				}
				return llmroutertest.Reply{ToolArgs: map[string]any{"reply": reply, "done": c.ExpectDone != nil && *c.ExpectDone}}
			}
		}
		return llmroutertest.Reply{Text: "I can't place this request."}
	}
}

// oracleDecide finds the case whose message the prompt carries — after
// "Message:", as an earlier turn's text is in the history too; the
// longest, as one message can start another — and builds its expected
// decision.
func oracleDecide(fx *Fixtures, prompt string) (map[string]any, bool) {
	var best *DecideCase
	for i, c := range fx.Decide {
		if strings.Contains(prompt, "Message:\n"+c.Message) && (best == nil || len(c.Message) > len(best.Message)) {
			best = &fx.Decide[i]
		}
	}
	if best == nil {
		return nil, false
	}
	e := best.Expect
	agent := e.AgentType
	if agent == "" && len(fx.World.AgentTypes) > 0 {
		agent = fx.World.AgentTypes[0].Name
	}
	if best.OpenTask != nil && e.WorkspaceID == best.OpenTask.WorkspaceID {
		agent = best.OpenTask.AgentType
	}
	args := map[string]any{"action": string(e.Action), "leave_open_task": e.LeaveOpenTask != nil && *e.LeaveOpenTask}
	switch e.Action {
	case "answer_directly":
		args["direct_answer"] = "An answer."
	case "use_workspace":
		args["workspace_id"], args["agent_type"] = e.WorkspaceID, agent
		if best.OpenTask != nil && e.WorkspaceID != best.OpenTask.WorkspaceID {
			args["leave_open_task"] = true
		}
	case "provision_workspace":
		target := ""
		if len(e.ProvisionTargetAny) > 0 {
			target = e.ProvisionTargetAny[0]
		} else if len(fx.World.Targets) > 0 {
			target = fx.World.Targets[0].ID
		}
		kind := string(e.ProvisionKind)
		if kind == "" {
			kind = "empty"
		}
		args["agent_type"] = agent
		args["new_workspace"] = map[string]any{"name": "new-workspace", "target_id": target, "kind": kind, "git_remote": e.GitRemote}
	case "run_command":
		cmd := "true"
		if len(e.CommandAny) > 0 {
			cmd = e.CommandAny[0]
		}
		args["target_id"], args["command"] = e.TargetID, cmd
	}
	return args, true
}

// Estimate is what a live run of the cases an offline report covers
// could cost, priced from the offline run's measured request sizes.
type Estimate struct {
	Calls int `json:"calls"`
	// PromptTokensPerCall is the offline run's mean, raised by a third
	// for tokenizer differences (newer Claude tokenizers count up to
	// ~30% more tokens for the same text).
	PromptTokensPerCall int64 `json:"prompt_tokens_per_call"`
	// CompletionTokensPerCall is an assumed ceiling: a decision is a few
	// hundred tokens, any thinking included.
	CompletionTokensPerCall int64 `json:"completion_tokens_per_call"`
	// ExpectedUSD assumes one answer per call; WorstCaseUSD that every
	// call needs the corrective retry, whose prompt is longer (2.5×).
	// Failed HTTP attempts (429, 5xx) aren't billed.
	ExpectedUSD  float64 `json:"expected_usd"`
	WorstCaseUSD float64 `json:"worst_case_usd"`
}

// assumedCompletionTokens is Estimate's per-answer completion ceiling.
const assumedCompletionTokens = 1024

// EstimateLive prices offline's calls at p, for runs runs per case.
func EstimateLive(offline *Report, p Price, runs int) Estimate {
	perRun := offline.Summary.Calls / max(1, offline.RunsPerCase)
	e := Estimate{Calls: perRun * runs, CompletionTokensPerCall: assumedCompletionTokens}
	if offline.Summary.Calls > 0 {
		e.PromptTokensPerCall = offline.Summary.PromptTokens * 4 / 3 / int64(offline.Summary.Calls)
	}
	one := p.Cost(e.PromptTokensPerCall, e.CompletionTokensPerCall)
	e.ExpectedUSD = one * float64(e.Calls)
	e.WorstCaseUSD = one * float64(e.Calls) * 2.5
	return e
}
