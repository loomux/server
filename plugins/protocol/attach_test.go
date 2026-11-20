package protocol

import "testing"

// LOOM-272: what the host takes as a read-only attach command.
func TestAttachesReadOnly(t *testing.T) {
	for name, argv := range map[string][]string{
		"kubectl":        {"kubectl", "-n", "loomux", "exec", "-it", "lx-abc", "--", "tmux", "-L", "loomux", "attach", "-r", "-t", "loomux-x"},
		"docker":         {"docker", "-H", "ssh://core@host", "exec", "-it", "lx-abc", "tmux", "-L", "loomux", "attach", "-r", "-t", "loomux-x"},
		"attach-session": {"tmux", "-L", "loomux", "attach-session", "-r", "-t", "loomux-x"},
		"a wrapper verb": {"fake", "attach", "abc", "--", "tmux", "-L", "loomux", "attach", "-r", "-t", "loomux-x"},
	} {
		if !AttachesReadOnly(argv) {
			t.Errorf("%s: %q should count as read-only", name, argv)
		}
	}
	for name, argv := range map[string][]string{
		"none":                    nil,
		"writable":                {"kubectl", "exec", "-it", "lx-abc", "--", "tmux", "-L", "loomux", "attach", "-t", "loomux-x"},
		"-r not after attach":     {"docker", "exec", "-it", "lx-abc", "tmux", "-L", "loomux", "attach", "-t", "loomux-x", "-r"},
		"-r before attach":        {"tool", "-r", "tmux", "attach", "-t", "loomux-x"},
		"no attach":               {"tmux", "-L", "loomux", "new-session", "-r"},
		"attach at the end":       {"tmux", "-L", "loomux", "attach"},
		"only the wrapper's -r":   {"fake", "attach", "-r", "--", "tmux", "-L", "loomux", "attach", "-t", "loomux-x"},
		"one shell line":          {"ssh", "host", "tmux -L loomux attach -r -t loomux-x"},
		"a flag that starts with": {"tmux", "attach", "-rx", "-t", "loomux-x"},
	} {
		if AttachesReadOnly(argv) {
			t.Errorf("%s: %q must not count as read-only", name, argv)
		}
	}
}
