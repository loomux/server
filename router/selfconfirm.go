package router

import (
	"regexp"
	"strings"
	"unicode"
)

// confirmationRequest matches a sentence asking the user to confirm or
// to reply yes — what Loomux's own offers say (`Reply "yes" to go
// ahead. Any other reply cancels.`) and a routing model can copy into
// an answer of its own (LOOM-255).
var confirmationRequest = regexp.MustCompile(`(?i)` +
	`\b(reply|respond|type|send)(\s+(with|back))?\s*:?\s*["'“”‘’]?(yes|y|ok|okay|confirm|confirmed)\b` +
	`|\b(say|answer)\s+(with\s+)?["'“”‘’](yes|y|ok|okay|confirm)["'“”‘’]` +
	`|^\W*(please\s+)?confirm\b` +
	`|\bplease\s+confirm\b` +
	`|\b(can|could|would|will)\s+you\s+(please\s+)?confirm\b` +
	`|\b(once|when|if|after)\s+you\s+confirm\b` +
	`|\bany other reply cancels\b`)

// sentence is one sentence of a line: up to and including its ending
// punctuation (and a closing quote), or the line's unended rest.
var sentence = regexp.MustCompile(`[^.!?\n]*[.!?]+["'”’)]*[ \t]*|[^.!?\n]+`)

// AsksForConfirmation reports whether text asks the user to confirm
// something or to reply yes.
func AsksForConfirmation(text string) bool {
	for _, line := range strings.Split(text, "\n") {
		for _, s := range sentence.FindAllString(line, -1) {
			if confirmationRequest.MatchString(strings.TrimSpace(s)) {
				return true
			}
		}
	}
	return false
}

// noConfirmationNote ends an answer whose request to confirm was taken
// out: no offer was made, so a yes would approve nothing.
const noConfirmationNote = "Nothing has started, and nothing is waiting for a yes: to go ahead, ask for the work itself, and say where."

// withoutConfirmationRequest takes the sentences asking the user to
// confirm or reply yes out of a routing model's direct answer (LOOM-255).
// Only an offer Loomux records can be confirmed, and an answer records
// none, so the user's yes would approve nothing: the answer says so
// instead. removed reports whether anything was taken out.
func withoutConfirmationRequest(answer string) (out string, removed bool) {
	lines := strings.Split(answer, "\n")
	for i, line := range lines {
		var kept strings.Builder
		for _, s := range sentence.FindAllString(line, -1) {
			if confirmationRequest.MatchString(strings.TrimSpace(s)) {
				removed = true
				continue
			}
			kept.WriteString(s)
		}
		lines[i] = strings.TrimRight(kept.String(), " \t")
	}
	if !removed {
		return answer, false
	}
	rest := strings.TrimSpace(strings.Join(lines, "\n"))
	if !strings.ContainsFunc(rest, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) {
		return noConfirmationNote, true
	}
	return rest + "\n\n" + noConfirmationNote, true
}
