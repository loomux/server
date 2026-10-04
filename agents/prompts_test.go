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
