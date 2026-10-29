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
//
// "Confirm" and "type yes" also ask for information ("Could you confirm
// which workspace you mean?", "Type yes in the terminal to accept the
// host key"), and taking such a question out leaves the user without
// the choice they were asked to make. So they count only when what
// follows asks for approval (approvalTail): the sentence ends, or goes
// on with "and I'll …", "you want …", "to go ahead" or the work to be
// started.
var confirmationRequest = regexp.MustCompile(`(?i)` +
	`\b(reply|respond|send)(\s+(with|back))?\s*:?\s*["'“”‘’]?(yes|y|ok|okay|confirm|confirmed)\b` +
	`|\b(say|answer)\s+(with\s+)?["'“”‘’](yes|y|ok|okay|confirm)["'“”‘’]` +
	`|\btype\s*:?\s*["'“”‘’]?(yes|y|ok|okay|confirm|confirmed)["'“”‘’]?` + approvalTail +
	`|^\W*(please\s+)?confirm` + approvalTail +
	`|\bplease\s+confirm` + approvalTail +
	`|\b(can|could|would|will)\s+you\s+(please\s+)?confirm` + approvalTail +
	`|\b(once|when|if|after)\s+you\s+confirm` + approvalTail +
	`|\bany other reply cancels\b`)

// approvalTail is what follows "confirm" or "type yes" in a request for
// approval: the end of the sentence, a clause break, "and I'll …",
// "(that) you want …", "(that) I should start …", "to go ahead …", or
// the work itself ("confirm starting new work there").
const approvalTail = `(\s*[.!?]*["'”’)]*$` +
	`|\s*[,;]` +
	`|\s+(and|then)\s+(I|we)\b` +
	`|\s+(that\s+)?you\s+(want|would\s+like|['’]d\s+like)\b` +
	`|\s+(that\s+)?(I|we)\s+(should|can|may)\s+(go|start|proceed|run|create|clone)\b` +
	`|\s+to\s+(go\s+ahead|proceed|start|continue|run|create|clone)\b` +
	`|\s+(starting|creating|cloning|running|provisioning|proceeding|going\s+ahead|launching|dispatching|deleting|installing|adding)\b)`

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
