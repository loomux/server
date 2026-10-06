package router

import (
	"regexp"
	"testing"

	"github.com/Loomux/server/registry"
)

// LOOM-109 review: a turn's end counts as a usage limit only from the
// final screen of a turn without a reply, or a reply that is the limit
// line alone. The detector here stands in for agents.DetectPrompt, which
// requires the CLI's ⎿ mark.
func TestUsageLimitAtEnd(t *testing.T) {
	marked := regexp.MustCompile(`(?m)^\s*⎿\s*(?:5-hour limit reached|Claude (?:AI )?usage limit reached)`)
	detect := func(screen string) *registry.Attention {
		if marked.MatchString(screen) {
			return &registry.Attention{Kind: registry.AttentionUsageLimit}
		}
		return nil
	}
	limitScreen := "❯ hi\n\n  ⎿  5-hour limit reached ∙ resets 5pm\n"
	cases := []struct {
		name, screen, reply string
		want                bool
	}{
		{"no reply, limit on screen", limitScreen, "", true},
		{"no reply, no limit", "❯ hi\n\n● done\n", "", false},
		{"the reply is the CLI's limit message", "", "Claude usage limit reached. Your limit will reset at 5pm (Europe/Istanbul).", true},
		{"the reply is the old epoch form", "", "Claude AI usage limit reached|1760000000", true},
		{"a one-line answer that starts with a limit phrase", "", "5-hour limit reached ∙ resets 5pm", false},
		{"a reply quoting it among other text", "", "The provider said:\n5-hour limit reached ∙ resets 5pm\nso wait.", false},
		{"a reply, whatever the screen shows", limitScreen, "Fixed the test.", false},
	}
	for _, tc := range cases {
		if got := usageLimitAtEnd(detect, tc.screen, tc.reply) != nil; got != tc.want {
			t.Errorf("%s: usage limit = %v, want %v", tc.name, got, tc.want)
		}
	}
}
