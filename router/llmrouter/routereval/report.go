package routereval

import (
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"time"
)

// Report is one Run's results. It holds no key and no request text,
// only the model's answers in brief.
type Report struct {
	Model       string    `json:"model"`
	Provider    string    `json:"provider"`
	Endpoint    string    `json:"endpoint"`
	Offline     bool      `json:"offline"`
	RunsPerCase int       `json:"runs_per_case"`
	Price       *Price    `json:"price,omitempty"`
	Fixtures    string    `json:"fixtures"`
	Started     time.Time `json:"started"`
	Finished    time.Time `json:"finished"`

	Cases   []CaseResult `json:"cases"`
	Summary Summary      `json:"summary"`
}

// CaseResult is one case over its runs.
type CaseResult struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Category string `json:"category"`
	Runs     int    `json:"runs"`
	Passed   int    `json:"passed"`
	// Wrong counts answers that weren't the expected one, or weren't
	// usable after the corrective retry; Errors, calls the endpoint
	// didn't answer (HTTP errors, timeouts).
	Wrong  int `json:"wrong"`
	Errors int `json:"errors"`
	// Raw is each run's answer in brief; Failures what was wrong.
	Raw      []string `json:"raw"`
	Failures []string `json:"failures,omitempty"`
	// ErrorClasses is each error's class (errorClass).
	ErrorClasses []string `json:"error_classes,omitempty"`

	LatencyMS        []int64 `json:"latency_ms"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	// HTTPAttempts counts requests sent, SDK retries and corrective
	// retries included; Answered, those answered 2xx.
	HTTPAttempts int     `json:"http_attempts"`
	Answered     int     `json:"answered"`
	CostUSD      float64 `json:"cost_usd"`

	lastOutcome string
}

const (
	outcomePass  = "pass"
	outcomeWrong = "WRONG"
	outcomeError = "ERROR"
)

func (c *CaseResult) record(outcome, view, problem string, elapsed time.Duration, met *meter) {
	c.Runs++
	c.lastOutcome = outcome
	switch outcome {
	case outcomePass:
		c.Passed++
	case outcomeWrong:
		c.Wrong++
	case outcomeError:
		c.Errors++
		c.ErrorClasses = append(c.ErrorClasses, problem)
	}
	c.Raw = append(c.Raw, view)
	if problem != "" {
		c.Failures = append(c.Failures, fmt.Sprintf("run %d: %s", c.Runs, problem))
	}
	if elapsed > 0 && outcome != outcomeError {
		c.LatencyMS = append(c.LatencyMS, elapsed.Milliseconds())
	}
	met.mu.Lock()
	defer met.mu.Unlock()
	c.PromptTokens += met.prompt
	c.CompletionTokens += met.completion
	c.HTTPAttempts += met.attempts
	c.Answered += met.answered
}

// Summary is the run in totals.
type Summary struct {
	Calls  int `json:"calls"`
	Passed int `json:"passed"`
	Wrong  int `json:"wrong"`
	Errors int `json:"errors"`
	// Accuracy is Passed/Calls; AnsweredAccuracy leaves the calls the
	// endpoint didn't answer out, separating model quality from
	// availability.
	Accuracy         float64 `json:"accuracy"`
	AnsweredAccuracy float64 `json:"answered_accuracy"`
	DecideAccuracy   float64 `json:"decide_accuracy"`
	RelayAccuracy    float64 `json:"relay_accuracy"`
	// Latency is per answered call, its corrective retry included.
	LatencyP50MS int64 `json:"latency_p50_ms"`
	LatencyP95MS int64 `json:"latency_p95_ms"`
	LatencyMaxMS int64 `json:"latency_max_ms"`

	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	HTTPAttempts     int   `json:"http_attempts"`
	// CorrectiveRetries counts answers beyond one per call: the model's
	// first answer was refused and it was asked again.
	CorrectiveRetries int `json:"corrective_retries"`
	// CostUSD and CostPer1kCalls are at Report.Price; unset without one.
	CostUSD        float64 `json:"cost_usd"`
	CostPer1kCalls float64 `json:"cost_per_1k_calls"`

	ErrorClasses map[string]int `json:"error_classes,omitempty"`
	StoppedEarly string         `json:"stopped_early,omitempty"`
}

func (r *Report) summarize() {
	s := &r.Summary
	var lat []int64
	var decideN, decideOK, relayN, relayOK int
	for _, c := range r.Cases {
		s.Calls += c.Runs
		s.Passed += c.Passed
		s.Wrong += c.Wrong
		s.Errors += c.Errors
		s.PromptTokens += c.PromptTokens
		s.CompletionTokens += c.CompletionTokens
		s.HTTPAttempts += c.HTTPAttempts
		s.CorrectiveRetries += max(0, c.Answered-(c.Runs-c.Errors))
		s.CostUSD += c.CostUSD
		lat = append(lat, c.LatencyMS...)
		if c.Kind == "decide" {
			decideN, decideOK = decideN+c.Runs, decideOK+c.Passed
		} else {
			relayN, relayOK = relayN+c.Runs, relayOK+c.Passed
		}
		for _, class := range c.ErrorClasses {
			if s.ErrorClasses == nil {
				s.ErrorClasses = map[string]int{}
			}
			s.ErrorClasses[class]++
		}
	}
	s.Accuracy = ratio(s.Passed, s.Calls)
	s.AnsweredAccuracy = ratio(s.Passed, s.Calls-s.Errors)
	s.DecideAccuracy = ratio(decideOK, decideN)
	s.RelayAccuracy = ratio(relayOK, relayN)
	s.LatencyP50MS, s.LatencyP95MS = percentile(lat, 50), percentile(lat, 95)
	if len(lat) > 0 {
		s.LatencyMaxMS = slices.Max(lat)
	}
	if r.Price == nil {
		s.CostUSD = 0
	} else if s.Calls > 0 {
		s.CostPer1kCalls = s.CostUSD / float64(s.Calls) * 1000
	}
}

func ratio(n, d int) float64 {
	if d <= 0 {
		return 0
	}
	return float64(n) / float64(d)
}

// percentile is the nearest-rank p-th percentile of xs, 0 if empty.
func percentile(xs []int64, p int) int64 {
	if len(xs) == 0 {
		return 0
	}
	s := slices.Clone(xs)
	slices.Sort(s)
	rank := (p*len(s) + 99) / 100
	return s[max(0, rank-1)]
}

// WriteMarkdown writes r as a Markdown report.
func (r *Report) WriteMarkdown(w io.Writer) {
	s := r.Summary
	mode := "live"
	if r.Offline {
		mode = "offline (fake endpoint: checks the harness, not the model)"
	}
	fmt.Fprintf(w, "# Router tier evaluation: %s\n\n", r.Model)
	fmt.Fprintf(w, "- Endpoint: %s (provider %s), %s\n", r.Endpoint, r.Provider, mode)
	fmt.Fprintf(w, "- %d calls (%d runs per case), %s\n", s.Calls, r.RunsPerCase, r.Finished.Sub(r.Started).Round(time.Second))
	if s.StoppedEarly != "" {
		fmt.Fprintf(w, "- **Stopped early:** %s\n", s.StoppedEarly)
	}
	fmt.Fprintf(w, "\n| Metric | Value |\n|---|---|\n")
	fmt.Fprintf(w, "| Accuracy | %.1f%% (%d/%d) |\n", 100*s.Accuracy, s.Passed, s.Calls)
	fmt.Fprintf(w, "| Accuracy, answered calls only | %.1f%% |\n", 100*s.AnsweredAccuracy)
	fmt.Fprintf(w, "| Decide / Relay accuracy | %.1f%% / %.1f%% |\n", 100*s.DecideAccuracy, 100*s.RelayAccuracy)
	fmt.Fprintf(w, "| Wrong / Errors | %d / %d |\n", s.Wrong, s.Errors)
	fmt.Fprintf(w, "| Latency p50 / p95 / max | %d / %d / %d ms |\n", s.LatencyP50MS, s.LatencyP95MS, s.LatencyMaxMS)
	fmt.Fprintf(w, "| Tokens in / out | %d / %d |\n", s.PromptTokens, s.CompletionTokens)
	fmt.Fprintf(w, "| HTTP attempts / corrective retries | %d / %d |\n", s.HTTPAttempts, s.CorrectiveRetries)
	if r.Price != nil {
		fmt.Fprintf(w, "| Cost (at $%.2f / $%.2f per MTok) | $%.4f ($%.3f per 1k calls) |\n",
			r.Price.InputPerMTok, r.Price.OutputPerMTok, s.CostUSD, s.CostPer1kCalls)
	} else {
		fmt.Fprintf(w, "| Cost | unknown (no price for this model) |\n")
	}
	if len(s.ErrorClasses) > 0 {
		fmt.Fprintf(w, "\nErrors by class:\n\n")
		for _, k := range sortedKeys(s.ErrorClasses) {
			fmt.Fprintf(w, "- %s: %d\n", k, s.ErrorClasses[k])
		}
	}
	fmt.Fprintf(w, "\n| Case | Kind | Passed | Answers |\n|---|---|---|---|\n")
	for _, c := range r.Cases {
		fmt.Fprintf(w, "| %s | %s | %d/%d | %s |\n", c.Name, c.Kind, c.Passed, c.Runs, mdCell(strings.Join(c.Raw, " · ")))
	}
	var failures []string
	for _, c := range r.Cases {
		for _, f := range c.Failures {
			failures = append(failures, fmt.Sprintf("- %s %s", c.Name, f))
		}
	}
	if len(failures) > 0 {
		fmt.Fprintf(w, "\nFailures:\n\n%s\n", strings.Join(failures, "\n"))
	}
}

func mdCell(s string) string { return strings.ReplaceAll(strings.ReplaceAll(s, "|", `\|`), "\n", " ") }

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
