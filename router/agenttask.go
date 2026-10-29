package router

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// AgentTask is what an agent is sent for message (LOOM-258): message
// without the parts the routing model quoted in routingText as only
// routing it — which agent, which workspace, which machine — so the
// agent gets the task, not instructions meant for Loomux. Only text is
// ever taken out, never added or reworded: a part not found in message
// (matched case-insensitively) is ignored, so one the model saw only
// redacted leaves the vault value in place. If nothing but routing
// would be left, the agent gets message whole.
func AgentTask(message string, routingText []string) string {
	task := message
	for _, part := range routingText {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		i := indexFold(task, part)
		if i < 0 {
			continue
		}
		task = joinAround(task[:i], task[i+len(part):])
	}
	task = strings.Trim(task, " \t\r\n,;:")
	if task == message || !strings.ContainsFunc(task, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) {
		return message
	}
	return task
}

// indexFold is where sub is in s, ignoring case, as a match of sub's
// own length in bytes that doesn't run into a word, path or name on
// either side: "in api-server" isn't found in "in api-server/cmd".
func indexFold(s, sub string) int {
	for i := range s {
		end := i + len(sub)
		if end > len(s) {
			break
		}
		if strings.EqualFold(s[i:end], sub) && !joinsWord(s[:i], sub, false) && !joinsWord(s[end:], sub, true) {
			return i
		}
	}
	return -1
}

// joinsWord reports whether the text beside a match (before it, or
// after when after is set) continues the match's first or last word.
func joinsWord(beside, sub string, after bool) bool {
	var b, edge rune
	if after {
		b, _ = utf8.DecodeRuneInString(beside)
		edge, _ = utf8.DecodeLastRuneInString(sub)
	} else {
		b, _ = utf8.DecodeLastRuneInString(beside)
		edge, _ = utf8.DecodeRuneInString(sub)
	}
	return beside != "" && wordRune(b) && wordRune(edge)
}

func wordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("_-/", r)
}

// joinAround closes the gap a removed part leaves: one space where it
// sat between words, nothing at a line's start or end.
func joinAround(left, right string) string {
	left, right = strings.TrimRight(left, " \t"), strings.TrimLeft(right, " \t")
	if left == "" || right == "" || strings.HasSuffix(left, "\n") || strings.HasPrefix(right, "\n") {
		return left + right
	}
	return left + " " + right
}
