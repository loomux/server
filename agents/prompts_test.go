package agents

import (
	"os"
	"reflect"
	"testing"

	"github.com/Loomux/server/registry"
)

// The claude-* fixtures are panes captured from Claude Code 2.1.289 on a
// real target (2026-10-04), except claude-idle, which is an idle pane
// with a reply that quotes a prompt's words. The codex-* fixtures are
// reconstructed from Codex's prompt layout, not captured.
func TestDetectPrompt(t *testing.T) {
	cases := []struct {
		fixture string
		want    *registry.Attention
	}{
		{"claude-permission.txt", &registry.Attention{
			Kind: registry.AttentionPermission, Title: "Bash command",
			Detail: "Create empty file a.txt\ntouch /tmp/lx97/a.txt", Question: "Do you want to proceed?",
			Options: []registry.AttentionOption{
				{Label: "Yes"},
				{Label: "Yes, and always allow access to /tmp/lx97 from this project"},
				{Label: "Yes, and switch to auto mode · auto mode handles these prompts for you"},
				{Label: "No"},
			},
		}},
		// The same prompt in a narrower pane, its tip wrapped onto a
		// second line: the tip's tail isn't detail.
		{"claude-permission-wrapped.txt", &registry.Attention{
			Kind: registry.AttentionPermission, Title: "Bash command",
			Detail: "Create empty file a.txt\ntouch /tmp/lx97/a.txt", Question: "Do you want to proceed?",
			Options: []registry.AttentionOption{
				{Label: "Yes"},
				{Label: "Yes, and always allow access to /tmp/lx97 from this project"},
				{Label: "Yes, and switch to auto mode · auto mode handles these prompts for you"},
				{Label: "No"},
			},
		}},
		{"claude-question.txt", &registry.Attention{
			Kind: registry.AttentionQuestion, Title: "Color", Question: "Do you prefer red or blue?",
			Options: []registry.AttentionOption{
				{Label: "Red", Description: "You prefer red."},
				{Label: "Blue", Description: "You prefer blue."},
				{Label: "Type something."},
				{Label: "Chat about this"},
			},
		}},
		{"claude-trust.txt", &registry.Attention{
			Kind: registry.AttentionTrust, Title: "Trust this folder?", Detail: "/tmp/lx97",
			Question: "Do you trust the files in this folder?",
			Options:  []registry.AttentionOption{{Label: "No, exit"}, {Label: "Yes, I trust this folder"}},
		}},
		{"claude-login.txt", &registry.Attention{Kind: registry.AttentionLogin, Title: "Sign-in required", Detail: "Select login method:"}},
		{"claude-onboarding.txt", &registry.Attention{Kind: registry.AttentionLogin, Title: "Sign-in required", Detail: "Let's get started."}},
		{"claude-idle.txt", nil},
		{"codex-approval.txt", &registry.Attention{
			Kind: registry.AttentionPermission, Question: "Would you like to run the following command?",
			Detail: "Reason: Needs network access to fetch the page\n$ curl -sS https://example.com -o page.html",
			Options: []registry.AttentionOption{
				{Label: "Yes, proceed"},
				{Label: "Yes, and don't ask again for this command"},
				{Label: "No, and tell Codex what to do differently"},
			},
		}},
		{"codex-login.txt", &registry.Attention{Kind: registry.AttentionLogin, Title: "Sign-in required",
			Detail: "Sign in with ChatGPT to use Codex as part of your paid plan"}},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			screen, err := os.ReadFile("testdata/" + tc.fixture)
			if err != nil {
				t.Fatal(err)
			}
			got := DetectPrompt(string(screen))
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("DetectPrompt =\n%#v\nwant\n%#v", got, tc.want)
			}
		})
	}
}

func TestDetectPromptIgnoresAWorkingAgent(t *testing.T) {
	screen := "● Running the tests\n\n✻ Cooking… (12s · esc to interrupt)\n\n────────────────────\n❯ \n────────────────────\n  ⏵⏵ auto mode on"
	if got := DetectPrompt(screen); got != nil {
		t.Errorf("DetectPrompt = %+v, want nil", got)
	}
}

// A wrapped tip's tail is one line at most, and a tip that ends its
// sentence has none: a lower-case description after it is detail.
func TestSplitHeaderTipContinuationBounded(t *testing.T) {
	rule := "────────────────────────────────────────"
	cases := []struct {
		name  string
		lines []string
	}{
		{"wrapped tip", []string{rule, " Bash command", " Tip: auto mode handles these prompts for you — choose \"switch to auto mode\"",
			" below", " git status of the repo", " Do you want to proceed?"}},
		{"finished tip", []string{rule, " Bash command", " Tip: auto mode handles these prompts for you.",
			" git status of the repo", " Do you want to proceed?"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q, header := splitHeader(tc.lines)
			want := []string{"Bash command", "git status of the repo"}
			if q != "Do you want to proceed?" || !reflect.DeepEqual(header, want) {
				t.Errorf("splitHeader = %q, %q; want the question and %q", q, header, want)
			}
		})
	}
}

// LOOM-109: Claude Code's status line while it compacts, and not the
// note a finished compaction leaves. Reconstructed fixtures.
func TestDetectCompaction(t *testing.T) {
	for fixture, want := range map[string]bool{
		"claude-compacting.txt": true,
		"claude-compacted.txt":  false,
		// #223 review: the agent editing this very code shows the words
		// in a diff above its status line.
		"claude-compacting-quoted.txt": false,
		"claude-idle.txt":              false,
	} {
		screen, err := os.ReadFile("testdata/" + fixture)
		if err != nil {
			t.Fatal(err)
		}
		if got := DetectCompaction(string(screen)); got != want {
			t.Errorf("%s: DetectCompaction = %v, want %v", fixture, got, want)
		}
	}
}

// LOOM-109: an agent's usage-limit error, with when it resets. These
// fixtures are reconstructed from the CLIs' documented messages, not
// captured: no account here was at its limit.
func TestDetectPrompt_UsageLimit(t *testing.T) {
	cases := []struct {
		fixture, detail, resets string
	}{
		{"claude-usage-limit.txt", "5-hour limit reached ∙ resets 5pm (Europe/Istanbul)", "5pm (Europe/Istanbul)"},
		{"claude-usage-limit-old.txt", "Claude usage limit reached. Your limit will reset at 5pm (Europe/Istanbul).", "5pm (Europe/Istanbul)"},
		{"codex-usage-limit.txt", "You've hit your usage limit. Upgrade to Pro (https://openai.com/chatgpt/pricing) or try again in 2 days 3 hours 4 minutes.", "in 2 days 3 hours 4 minutes"},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			screen, err := os.ReadFile("testdata/" + tc.fixture)
			if err != nil {
				t.Fatal(err)
			}
			got := DetectPrompt(string(screen))
			if got == nil || got.Kind != registry.AttentionUsageLimit {
				t.Fatalf("DetectPrompt = %+v, want a usage limit", got)
			}
			if got.Detail != tc.detail || got.ResetsAt != tc.resets {
				t.Errorf("detail %q resets %q, want %q and %q", got.Detail, got.ResetsAt, tc.detail, tc.resets)
			}
		})
	}
	// A warning that the limit is near, and a reply that quotes the
	// words, are not a limit reached.
	for _, fixture := range []string{"claude-usage-warning.txt", "claude-usage-quoted.txt", "codex-usage-quoted.txt", "claude-usage-quoted-reset.txt"} {
		screen, err := os.ReadFile("testdata/" + fixture)
		if err != nil {
			t.Fatal(err)
		}
		if got := DetectPrompt(string(screen)); got != nil {
			t.Errorf("%s: DetectPrompt = %+v, want nil", fixture, got)
		}
	}
	// The older form carries the reset as a Unix time.
	got := DetectPrompt("❯ hi\n\nClaude AI usage limit reached|1760000000\n")
	if got == nil || got.Kind != registry.AttentionUsageLimit || got.ResetsAt != "Oct 9 08:53 UTC" || got.Detail != "Claude AI usage limit reached" {
		t.Errorf("epoch form: %+v", got)
	}
}
