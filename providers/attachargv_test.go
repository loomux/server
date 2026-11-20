package providers

import (
	"slices"
	"strings"
	"testing"

	"github.com/Loomux/server/plugins/protocol"
)

// LOOM-269: an argv a client may execute as given is passed on; anything
// else is dropped, and the command is then offered without one.
func TestValidAttachArgv(t *testing.T) {
	ok := []string{"kubectl", "-n", "loomux", "exec", "-it", "lx-abc", "--", "tmux", "-L", "loomux", "attach", "-t", "loomux-x"}
	got := validAttachArgv(ok)
	if !slices.Equal(got, ok) {
		t.Errorf("a plain argv = %q", got)
	}
	got[0] = "changed"
	if ok[0] != "kubectl" {
		t.Error("the result shares the plugin's slice")
	}
	// An argument may hold spaces and shell characters: nothing parses it.
	if spaced := []string{"ssh", "host", "-t", "tmux -L loomux attach -t 'x'; $(y)"}; !slices.Equal(validAttachArgv(spaced), spaced) {
		t.Error("an argument with spaces and shell characters was refused")
	}
	for name, argv := range map[string][]string{
		"none":            nil,
		"empty":           {},
		"no program":      {"", "exec"},
		"a NUL":           {"kubectl", "a\x00b"},
		"a newline":       {"kubectl", "a\nb"},
		"a return":        {"kubectl", "a\rb"},
		"too many":        slices.Repeat([]string{"x"}, maxAttachArgs+1),
		"too long in all": {"kubectl", strings.Repeat("x", maxAttachArgvSize)},
	} {
		if got := validAttachArgv(argv); got != nil {
			t.Errorf("%s: validAttachArgv = %q, want nil", name, got)
		}
	}
	if got := validAttachArgv(slices.Repeat([]string{"x"}, maxAttachArgs)); len(got) != maxAttachArgs {
		t.Errorf("an argv of exactly %d arguments was refused", maxAttachArgs)
	}
}

// LOOM-272: what of a plugin's answer to a read_only request is handed
// to a person who only watches: nothing unless the plugin confirmed
// read_only, and then only commands whose argv ends with tmux's
// read-only attach to the session and socket that were asked for.
func TestReadOnlyAttachAnswer(t *testing.T) {
	const socket, session = "loomux", "loomux-x"
	ro := []string{"kubectl", "exec", "-it", "lx-abc", "--", "tmux", "-L", socket, "attach", "-r", "-t", session}
	rw := []string{"kubectl", "exec", "-it", "lx-abc", "--", "tmux", "-L", socket, "attach", "-t", session}
	cmd := func(argv []string) protocol.AttachCommand {
		return protocol.AttachCommand{Via: "kubectl", Command: strings.Join(argv, " "), Argv: argv}
	}
	answer := func(res protocol.AttachResult) []protocol.AttachCommand {
		return readOnlyAttachAnswer(res, socket, session)
	}

	got := answer(protocol.AttachResult{ReadOnly: true, Commands: []protocol.AttachCommand{cmd(ro)}})
	if len(got) != 1 || !slices.Equal(got[0].Argv, ro) {
		t.Errorf("a confirmed read-only answer = %+v; want its command", got)
	}
	elsewhere := []string{"kubectl", "exec", "-it", "lx-abc", "--", "tmux", "-L", socket, "attach", "-r", "-t", "loomux-y"}
	for name, res := range map[string]protocol.AttachResult{
		// As a plugin from before LOOM-272 answers: it ignored read_only.
		"unconfirmed, writable":  {Commands: []protocol.AttachCommand{cmd(rw)}},
		"unconfirmed, read-only": {Commands: []protocol.AttachCommand{cmd(ro)}},
		"confirmed, writable":    {ReadOnly: true, Commands: []protocol.AttachCommand{cmd(rw)}},
		"confirmed, no argv":     {ReadOnly: true, Commands: []protocol.AttachCommand{{Via: "kubectl", Command: strings.Join(ro, " ")}}},
		"confirmed, bad argv":    {ReadOnly: true, Commands: []protocol.AttachCommand{{Via: "kubectl", Command: "x", Argv: append([]string{"a\nb"}, ro...)}}},
		"confirmed, no via":      {ReadOnly: true, Commands: []protocol.AttachCommand{{Command: strings.Join(ro, " "), Argv: ro}}},
		"confirmed, none":        {ReadOnly: true},
		// Read-only, but not the session that was asked for.
		"confirmed, another session": {ReadOnly: true, Commands: []protocol.AttachCommand{cmd(elsewhere)}},
		// A script that attaches writable, the read-only words only its
		// arguments.
		"confirmed, words after a script": {ReadOnly: true, Commands: []protocol.AttachCommand{
			{Via: "sh", Command: "x", Argv: []string{"sh", "-c", "tmux attach -t " + session, "attach", "-r"}}}},
	} {
		if got := answer(res); len(got) != 0 {
			t.Errorf("%s: readOnlyAttachAnswer = %+v, want none", name, got)
		}
	}
	// The line to paste is made from the argv that was checked, not taken
	// from the plugin: a line that says something else than the argv is
	// never shown for a watch.
	lying := answer(protocol.AttachResult{ReadOnly: true, Commands: []protocol.AttachCommand{
		{Via: "kubectl", Command: strings.Join(rw, " "), Argv: ro},
	}})
	if len(lying) != 1 || lying[0].Command != strings.Join(ro, " ") {
		t.Errorf("a writable line beside a read-only argv = %+v; want the argv's own line", lying)
	}
	spaced := []string{"tool", "--as", "a b", "it's", "tmux", "-L", socket, "attach", "-r", "-t", session}
	quoted := answer(protocol.AttachResult{ReadOnly: true, Commands: []protocol.AttachCommand{{Via: "tool", Command: "x", Argv: spaced}}})
	if want := `tool --as 'a b' 'it'\''s' tmux -L loomux attach -r -t loomux-x`; len(quoted) != 1 || quoted[0].Command != want {
		t.Errorf("the line for an argv with spaces and a quote = %+v; want %q", quoted, want)
	}
	// One good command beside a bad one: the good one alone.
	mixed := answer(protocol.AttachResult{ReadOnly: true, Commands: []protocol.AttachCommand{cmd(rw), cmd(ro)}})
	if len(mixed) != 1 || !slices.Equal(mixed[0].Argv, ro) {
		t.Errorf("a mixed answer = %+v; want only the read-only command", mixed)
	}
}
