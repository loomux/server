package protocol

import "testing"

// LOOM-272: what the host takes as a read-only attach command: an argv
// that ends with exactly tmux's read-only attach to the session the host
// asked for, on the socket it named.
func TestAttachesReadOnly(t *testing.T) {
	const socket, session = "loomux-b", "loomux-x"
	for name, argv := range map[string][]string{
		"kubectl":        {"kubectl", "-n", "loomux", "exec", "-it", "lx-abc", "--", "tmux", "-L", socket, "attach", "-r", "-t", session},
		"docker":         {"docker", "-H", "ssh://core@host", "exec", "-it", "lx-abc", "tmux", "-L", socket, "attach", "-r", "-t", session},
		"attach-session": {"tmux", "-L", socket, "attach-session", "-r", "-t", session},
		"a wrapper verb": {"fake", "attach", "abc", "--", "tmux", "-L", socket, "attach", "-r", "-t", session},
	} {
		if !AttachesReadOnly(argv, socket, session) {
			t.Errorf("%s: %q should count as read-only", name, argv)
		}
	}
	for name, argv := range map[string][]string{
		"none":                  nil,
		"writable":              {"kubectl", "exec", "-it", "lx-abc", "--", "tmux", "-L", socket, "attach", "-t", session},
		"-r not after attach":   {"docker", "exec", "-it", "lx-abc", "tmux", "-L", socket, "attach", "-t", session, "-r"},
		"-r before attach":      {"tool", "-r", "tmux", "-L", socket, "attach", "-t", session},
		"no attach":             {"tmux", "-L", socket, "new-session", "-r", "-t", session},
		"only the wrapper's -r": {"fake", "attach", "-r", "--", "tmux", "-L", socket, "attach", "-t", session},
		"one shell line":        {"ssh", "host", "tmux -L " + socket + " attach -r -t " + session},
		"a longer flag":         {"tmux", "-L", socket, "attach", "-rx", "-t", session},
		"another socket":        {"tmux", "-L", "loomux", "attach", "-r", "-t", session},
		"another session":       {"tmux", "-L", socket, "attach", "-r", "-t", "loomux-y"},
		"no socket":             {"tmux", "attach", "-r", "-t", session},
		"something after it":    {"tmux", "-L", socket, "attach", "-r", "-t", session, ";", "tmux", "attach"},
		"not tmux":              {"screen", "-L", socket, "attach", "-r", "-t", session},
		// A shell script that attaches writable, with the read-only words
		// only passed to it as arguments: they aren't tmux's.
		"words after a script": {"sh", "-c", "tmux attach -t " + session, "attach", "-r"},
	} {
		if AttachesReadOnly(argv, socket, session) {
			t.Errorf("%s: %q must not count as read-only", name, argv)
		}
	}
	// Nothing counts without a socket and a session to compare with.
	ok := []string{"tmux", "-L", socket, "attach", "-r", "-t", session}
	if AttachesReadOnly(ok, "", session) || AttachesReadOnly(ok, socket, "") {
		t.Error("an empty socket or session matched")
	}
	if AttachesReadOnly([]string{"tmux", "-L", "", "attach", "-r", "-t", ""}, "", "") {
		t.Error("an argv with empty socket and session matched empty ones")
	}
}
