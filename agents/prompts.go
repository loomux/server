package agents

import (
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Loomux/server/registry"
)

// promptRegionLines is how much of the bottom of a pane is searched for
// a prompt (LOOM-97). Agent CLIs draw their prompts at the bottom; text
// further up is scrollback, where an agent's own reply could quote a
// prompt's words without being stopped at one.
const promptRegionLines = 40

// promptFooters end every selection prompt Claude Code and Codex draw;
// one of them in the last few lines is what tells a prompt from the
// agent merely working ("esc to interrupt") or idle. Matched
// case-insensitively.
var promptFooters = []string{"esc to cancel", "enter to select", "enter to confirm", "press enter to continue"}

// loginMarkers are what each CLI shows when it isn't signed in: Claude
// Code's onboarding and login-method screens, its expired- or
// invalid-credential errors, and Codex's sign-in screen.
var loginMarkers = []string{
	"Select login method",
	"Let's get started.",
	"Please run /login",
	"Invalid API key",
	"OAuth token has expired",
	"Paste code here if prompted",
	"Sign in with ChatGPT",
	"Provide your own API key",
}

var (
	// numberedOption's first group is everything before the number, so
	// its rune count is the number's column whether or not the cursor is
	// on that line.
	numberedOption = regexp.MustCompile(`^(\s*(?:[❯›>]\s*)?)(\d+)\.\s+(.*\S)\s*$`)
	cursorLine     = regexp.MustCompile(`^(\s*)[❯›>]\s+(\S.*?)\s*$`)
	// shortcutSuffix is the key hint Codex puts after an option ("Yes,
	// proceed (y)").
	shortcutSuffix = regexp.MustCompile(`\s+\((?:[a-z]|esc)\)$`)
)

// DetectPrompt reads the prompt an agent CLI is stopped at off its pane
// (LOOM-97): Claude Code's and Codex's permission prompts, Claude Code's
// AskUserQuestion, the folder-trust dialog, and either CLI asking to be
// signed in. nil means none is showing. Both CLIs draw prompts the same
// way — a heading, a question, a list of options with a cursor, a footer
// with the keys — which is what this parses, so it works for either.
func DetectPrompt(screen string) *registry.Attention {
	lines := bottomLines(screen, promptRegionLines)
	if login := detectLogin(lines); login != nil {
		return login
	}
	if limit := detectUsageLimit(lines); limit != nil {
		return limit
	}
	footer := -1
	for i := len(lines) - 1; i >= 0 && i >= len(lines)-3; i-- {
		if isFooter(lines[i]) {
			footer = i
			break
		}
	}
	if footer < 0 {
		return nil
	}
	options, selected, first := parseOptions(lines[:footer])
	if len(options) == 0 {
		return nil
	}
	a := &registry.Attention{Options: options, Selected: selected}
	question, header := splitHeader(lines[:first])
	a.Question = question
	if len(header) > 0 {
		a.Title = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(header[0]), "☐"))
		a.Detail = strings.Join(header[1:], "\n")
	}
	a.Kind = promptKind(a, lines[:footer])
	if a.Kind == registry.AttentionTrust {
		a.Title = "Trust this folder?"
		a.Question = "Do you trust the files in this folder?"
		a.Detail = trustedPath(header)
	}
	return a
}

// bottomLines is screen's last n lines, trailing blank lines dropped and
// trailing spaces trimmed.
func bottomLines(screen string, n int) []string {
	lines := strings.Split(strings.ReplaceAll(screen, "\r", ""), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t")
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

func detectLogin(lines []string) *registry.Attention {
	// The login screens are short; only the bottom of the pane counts.
	region := lines
	if len(region) > 20 {
		region = region[len(region)-20:]
	}
	for _, l := range region {
		for _, m := range loginMarkers {
			if strings.Contains(l, m) {
				return &registry.Attention{Kind: registry.AttentionLogin, Title: "Sign-in required", Detail: strings.TrimSpace(l)}
			}
		}
	}
	return nil
}

// DetectCompaction reports whether Claude Code is compacting its context
// right now (LOOM-109): its status line, the last line above the input
// box, reads "✻ Compacting conversation…" — a spinner glyph, the words, an
// ellipsis. Only that line, in that shape, counts: tool output and diffs
// above it may quote the words (an agent working on this very file), and
// a finished compaction's "Conversation compacted" note doesn't count.
func DetectCompaction(screen string) bool {
	line, ok := statusLine(bottomLines(screen, compactionRegionLines))
	return ok && compactingStatus.MatchString(line)
}

// compactingStatus is Claude Code's status line while it compacts.
var compactingStatus = regexp.MustCompile(`^\s*[✻✽✶✳✢·*]\s*Compacting conversation…`)

// statusLine is the last non-blank line above Claude Code's input box (a
// rule, then the "❯" prompt line); false when there's no input box.
func statusLine(lines []string) (string, bool) {
	for i := len(lines) - 2; i >= 0; i-- {
		if !isRule(lines[i]) || !strings.HasPrefix(strings.TrimSpace(lines[i+1]), "❯") {
			continue
		}
		for j := i - 1; j >= 0; j-- {
			if strings.TrimSpace(lines[j]) != "" {
				return lines[j], true
			}
		}
		return "", false
	}
	return "", false
}

// compactionRegionLines is how much of the pane's bottom holds the status
// line and the input box under it.
const compactionRegionLines = 12

var (
	// usageLimitLine is an agent's own usage-limit error (LOOM-109):
	// Claude Code's "Claude usage limit reached", "5-hour limit
	// reached", "Weekly limit reached", "You've hit your limit", and
	// Codex's "You've hit your usage limit". It must carry the mark each
	// CLI puts on its own errors, Claude Code's ⎿ and Codex's ■: an
	// agent's reply ("● …", "• …", or a wrapped line) that quotes the
	// words, or a warning that a limit is near, doesn't count.
	usageLimitLine = regexp.MustCompile(`(?i)^\s*[⎿■]\s*(?:claude (?:ai )?usage limit reached|(?:5-hour|session|daily|weekly|opus weekly|sonnet weekly|usage) limit reached|you've hit your (?:session |weekly |usage )?limit)\b(.*)$`)
	// usageLimitEpoch is the older Claude Code form, a line of its own:
	// "Claude AI usage limit reached|1760000000", the reset as a Unix time.
	usageLimitEpoch = regexp.MustCompile(`^\s*(?:[⎿■]\s*)?(Claude AI usage limit reached)\|(\d{10})\s*$`)
	// resetsAt finds when a limit resets in the rest of the line(s).
	resetsAt = []*regexp.Regexp{
		regexp.MustCompile(`(?i)\bresets?(?: at)?\s+([^·∙|]+?)\s*(?:[·∙|]|$)`),
		regexp.MustCompile(`(?i)\bwill reset at\s+(.+?)\.?\s*$`),
		regexp.MustCompile(`(?i)\btry again (in .+?|at .+?)\.?\s*$`),
	}
)

// detectUsageLimit finds an agent's usage-limit error near the bottom of
// its pane, with when the limit resets if the agent said.
func detectUsageLimit(lines []string) *registry.Attention {
	region := lines
	if len(region) > usageLimitRegionLines {
		region = region[len(region)-usageLimitRegionLines:]
	}
	for i := len(region) - 1; i >= 0; i-- {
		if m := usageLimitEpoch.FindStringSubmatch(region[i]); m != nil {
			sec, _ := strconv.ParseInt(m[2], 10, 64)
			return &registry.Attention{Kind: registry.AttentionUsageLimit, Title: "Usage limit reached", Detail: m[1],
				ResetsAt: time.Unix(sec, 0).UTC().Format("Jan 2 15:04 UTC")}
		}
		m := usageLimitLine.FindStringSubmatch(region[i])
		if m == nil {
			continue
		}
		a := &registry.Attention{Kind: registry.AttentionUsageLimit, Title: "Usage limit reached",
			Detail: strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(region[i]), "⎿■"))}
		// The reset is on the line itself, or else the next one.
		a.ResetsAt = findReset(m[1])
		if a.ResetsAt == "" && i+1 < len(region) {
			a.ResetsAt = findReset(region[i+1])
		}
		return a
	}
	return nil
}

// usageLimitRegionLines is how much of the pane's bottom a usage-limit
// error is looked for in: the CLIs stop right after printing one, so it
// sits just above the input box.
const usageLimitRegionLines = 12

func findReset(text string) string {
	for _, re := range resetsAt {
		if r := re.FindStringSubmatch(text); r != nil {
			return strings.TrimRight(strings.TrimSpace(r[1]), ".")
		}
	}
	return ""
}

func isFooter(line string) bool {
	lower := strings.ToLower(line)
	for _, f := range promptFooters {
		if strings.Contains(lower, f) {
			return true
		}
	}
	return false
}

// isRule reports a horizontal rule: a line of box-drawing dashes.
func isRule(line string) bool {
	t := strings.TrimSpace(line)
	return utf8.RuneCountInString(t) >= 10 && strings.Trim(t, "─╌━-") == ""
}

func indent(line string) int {
	return len(line) - len(strings.TrimLeft(line, " "))
}

// parseOptions finds the option list above a prompt's footer: numbered
// ("❯ 1. Yes") or, as in the trust dialog, plain lines at the cursor's
// indent. Each numbered option may be followed by more-indented
// description lines. It returns the options, which one the cursor is
// on, and the index of the list's first line.
func parseOptions(lines []string) (options []registry.AttentionOption, selected, first int) {
	cursor := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if cursorLine.MatchString(lines[i]) {
			cursor = i
			break
		}
	}
	if cursor < 0 {
		return nil, 0, 0
	}
	if m := numberedOption.FindStringSubmatch(lines[cursor]); m != nil {
		return parseNumbered(lines, cursor, utf8.RuneCountInString(m[1]))
	}
	return parsePlain(lines, cursor)
}

func parseNumbered(lines []string, cursor, col int) (options []registry.AttentionOption, selected, first int) {
	// Walk up from the cursor to the list's first option: past options,
	// their descriptions, rules and blank lines.
	first = cursor
	for i := cursor - 1; i >= 0; i-- {
		l := lines[i]
		if m := numberedOption.FindStringSubmatch(l); m != nil && utf8.RuneCountInString(m[1]) == col {
			first = i
			continue
		}
		if l == "" || isRule(l) || indent(l) > col {
			continue
		}
		break
	}
	for i := first; i < len(lines); i++ {
		l := lines[i]
		if m := numberedOption.FindStringSubmatch(l); m != nil && utf8.RuneCountInString(m[1]) == col {
			if i == cursor {
				selected = len(options)
			}
			options = append(options, registry.AttentionOption{Label: shortcutSuffix.ReplaceAllString(m[3], "")})
			continue
		}
		if len(options) > 0 && l != "" && !isRule(l) && indent(l) > col {
			o := &options[len(options)-1]
			o.Description = strings.TrimSpace(strings.TrimSpace(o.Description + " " + strings.TrimSpace(l)))
		}
	}
	return options, selected, first
}

func parsePlain(lines []string, cursor int) (options []registry.AttentionOption, selected, first int) {
	m := cursorLine.FindStringSubmatch(lines[cursor])
	col := utf8.RuneCountInString(lines[cursor][:strings.Index(lines[cursor], m[2])])
	inList := func(l string) bool {
		return l != "" && indent(l) == col && !isRule(l)
	}
	first = cursor
	for first > 0 && inList(lines[first-1]) {
		first--
	}
	last := cursor
	for last+1 < len(lines) && inList(lines[last+1]) {
		last++
	}
	for i := first; i <= last; i++ {
		label := strings.TrimSpace(lines[i])
		if i == cursor {
			selected = len(options)
			label = m[2]
		}
		options = append(options, registry.AttentionOption{Label: label})
	}
	return options, selected, first
}

// isContinuation reports whether t reads as the rest of a wrapped
// sentence rather than a line of its own: it starts in lower case or
// with punctuation.
func isContinuation(t string) bool {
	r, _ := utf8.DecodeRuneInString(t)
	return unicode.IsLower(r) || unicode.IsPunct(r)
}

// endsSentence reports whether t ends with a sentence's closing
// punctuation, so nothing wraps on from it.
func endsSentence(t string) bool {
	return strings.HasSuffix(t, ".") || strings.HasSuffix(t, "!") || strings.HasSuffix(t, "?")
}

// splitHeader splits what's above a prompt's options into its question
// (the last line asking one) and the heading and detail lines around
// it. Claude Code draws a solid rule above a prompt, which bounds it;
// with no rule (Codex), what's above the question is the agent's
// earlier output, so only the lines after the question are kept.
func splitHeader(lines []string) (question string, header []string) {
	start, bounded := 0, false
	for i := len(lines) - 1; i >= 0; i-- {
		// A dashed rule (╌) frames a command inside the prompt.
		if isRule(lines[i]) && strings.Trim(strings.TrimSpace(lines[i]), "╌") != "" {
			start, bounded = i+1, true
			break
		}
	}
	// tipOpen: the line before was a tip that may wrap onto this one.
	tipOpen := false
	for _, l := range lines[start:] {
		t := strings.TrimSpace(l)
		if t == "" || isRule(l) {
			tipOpen = false
			continue
		}
		// A tip is the CLI's own advice, not the prompt's. It can wrap
		// onto one more line, which carries on its sentence, so doesn't
		// start with a capital. Only one: what follows is the prompt's
		// own detail, even if it starts in lower case.
		if strings.HasPrefix(t, "Tip:") {
			tipOpen = !endsSentence(t)
			continue
		}
		if tipOpen && isContinuation(t) {
			tipOpen = false
			continue
		}
		tipOpen = false
		header = append(header, t)
	}
	q := -1
	for i := len(header) - 1; i >= 0; i-- {
		if strings.HasSuffix(header[i], "?") {
			q = i
			break
		}
	}
	if q < 0 {
		return "", header
	}
	question = header[q]
	if !bounded {
		// No title: the detail lines are what follows the question.
		return question, append([]string{""}, header[q+1:]...)
	}
	return question, append(header[:q:q], header[q+1:]...)
}

func promptKind(a *registry.Attention, lines []string) registry.AttentionKind {
	for _, l := range lines {
		if strings.Contains(l, "trust this folder") || strings.Contains(l, "one you trust") ||
			strings.Contains(l, "Do you trust the files") {
			return registry.AttentionTrust
		}
	}
	for _, o := range a.Options {
		lower := strings.ToLower(o.Label)
		if strings.HasPrefix(lower, "yes") || strings.HasPrefix(lower, "no") {
			return registry.AttentionPermission
		}
	}
	return registry.AttentionQuestion
}

// trustedPath is the folder a trust dialog names: the heading line that
// looks like a path.
func trustedPath(header []string) string {
	for _, l := range header {
		if strings.HasPrefix(l, "/") || strings.HasPrefix(l, "~") {
			return l
		}
	}
	return ""
}
