package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Loomux/server/router/llmrouter/routereval"
)

func runArgs(t *testing.T, env map[string]string, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := run(context.Background(), args, func(k string) string { return env[k] }, &out, &errOut)
	return code, out.String(), errOut.String()
}

// Without --live nothing leaves the process: the whole set runs against
// the fake, passes, and prices a live run.
func TestRun_OfflineDefault(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "r.json")
	code, stdout, stderr := runArgs(t, nil, "run", "--model", "claude-haiku-5-5", "--runs", "3", "--out", out)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	for _, want := range []string{"offline (fake endpoint", "Accuracy | 100.0%", "Live estimate for claude-haiku-5-5, 3 runs per case: 90 calls",
		"at $0.10/$0.50 per MTok (built-in price table, unverified"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("output lacks %q:\n%s", want, stdout)
		}
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var rep routereval.Report
	if err := json.Unmarshal(data, &rep); err != nil || !rep.Offline || rep.Summary.Calls != 90 {
		t.Fatalf("report: %v %+v", err, rep.Summary)
	}
}

func TestRun_LiveGuards(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		args []string
		want string
	}{
		{"no budget", map[string]string{"ROUTEREVAL_API_KEY": "k"}, []string{"--model", "claude-haiku-5-5", "--live"}, "--budget-usd"},
		{"no key", nil, []string{"--model", "claude-haiku-5-5", "--live", "--budget-usd", "1"}, "ROUTEREVAL_API_KEY is not set"},
		{"no price", map[string]string{"ROUTEREVAL_API_KEY": "k"}, []string{"--model", "mystery", "--live", "--budget-usd", "1"}, "--price-in"},
		{"over budget", map[string]string{"ROUTEREVAL_API_KEY": "k"}, []string{"--model", "claude-sonnet-5-5", "--runs", "50", "--live", "--budget-usd", "0.01"}, "over the budget"},
		{"bad base URL", map[string]string{"ROUTEREVAL_API_KEY": "k"}, []string{"--model", "claude-haiku-5-5", "--base-url", "https://api.anthropic.com", "--live", "--budget-usd", "1"}, "/v1"},
		{"negative price", nil, []string{"--model", "claude-haiku-5-5", "--price-in", "-1", "--price-out", "5"}, "--price-in"},
		{"half a price", nil, []string{"--model", "claude-haiku-5-5", "--price-in", "1"}, "--price-out"},
		{"other half", nil, []string{"--model", "mystery", "--price-out", "5"}, "--price-in"},
		{"NaN budget", nil, []string{"--model", "claude-haiku-5-5", "--live", "--budget-usd", "NaN"}, "--budget-usd"},
		{"anthropic offline", nil, []string{"--model", "claude-haiku-5-5", "--provider", "anthropic"}, "--live"},
		{"no model", nil, nil, "--model is required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, _, stderr := runArgs(t, tc.env, append([]string{"run"}, tc.args...)...)
			if code == 0 || !strings.Contains(stderr, tc.want) {
				t.Errorf("exit %d, stderr %q: want a failure naming %q", code, stderr, tc.want)
			}
		})
	}
}

func TestCompare(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.json"), filepath.Join(dir, "b.json")
	for _, m := range []struct{ model, path string }{{"claude-haiku-4-5", a}, {"claude-haiku-5-5", b}} {
		if code, _, stderr := runArgs(t, nil, "run", "--model", m.model, "--runs", "1", "--cases", "codex", "--out", m.path); code != 0 {
			t.Fatalf("run %s: %s", m.model, stderr)
		}
	}
	code, stdout, stderr := runArgs(t, nil, "compare", a, b)
	if code != 0 || !strings.Contains(stdout, "claude-haiku-4-5 vs claude-haiku-5-5") || !strings.Contains(stdout, "No case changed.") {
		t.Fatalf("exit %d %s\n%s", code, stderr, stdout)
	}
}
