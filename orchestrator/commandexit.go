package orchestrator

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/Loomux/server/targets"
)

// A command task reports its own exit status (LOOM-136). tmux records a
// pane's status only once it has reaped the process, which a busy
// server does late, so reading it alone could find none and misreport a
// command that exited 0. The command runs in a subshell, so its own
// `exit` ends only that, and the pane then prints a marker line with the
// status before exiting with it.

// commandExitMarker is the line wrapCommand prints: "[loomux:exit=N]".
var commandExitMarker = regexp.MustCompile(`(?m)\n?^\[loomux:exit=(\d+)\]\s*\z`)

// wrapCommand is what a command task's pane runs for command.
func wrapCommand(command string) string {
	return "(\n" + command + "\n)\n__loomux_s=$?\nprintf '\\n[loomux:exit=%d]\\n' \"$__loomux_s\"\nexit \"$__loomux_s\""
}

// CommandExit is a command task's exit read off its pane: the status the
// command reported, with the marker line taken out of the output. With
// no marker (the shell itself was killed), it's exit as tmux saw it.
func CommandExit(exit *targets.PaneExit) *targets.PaneExit {
	if exit == nil {
		return nil
	}
	m := commandExitMarker.FindStringSubmatchIndex(exit.Output)
	if m == nil {
		return exit
	}
	status, err := strconv.Atoi(exit.Output[m[2]:m[3]])
	if err != nil {
		return exit
	}
	// The status is the marker's; Signal stays as tmux saw it, so
	// Describe still reads the same exit.
	return &targets.PaneExit{Status: status, Signal: exit.Signal, Output: strings.TrimRight(exit.Output[:m[0]], " \t\n")}
}
