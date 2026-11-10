package providers

import (
	"slices"
	"strings"
	"testing"
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
