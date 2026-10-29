package targets

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// PollRequest is one poll of a target (LOOM-254): which of Files exist,
// and which of Panes' processes still run. Panes are session names, as
// PaneExited takes them.
type PollRequest struct {
	Files []string
	Panes []string
}

// PollResult answers a PollRequest, index for index.
type PollResult struct {
	// Exists[i] reports whether Files[i] exists.
	Exists []bool
	// Running[i] reports whether Panes[i]'s process is still running.
	// false covers a dead pane, a missing session and a pane tmux didn't
	// report at all: PaneExited says which, and how the process ended.
	Running []bool
}

// Poller is a TargetExecutor that answers a whole PollRequest in one
// target operation (LOOM-254): the 1 s completion polls of every turn
// waiting on a target cost one ssh round trip, not two per turn, so ten
// sessions on a slow link stay inside the target's operation budget
// (maxConcurrentOps). An executor without it is polled one call at a
// time.
type Poller interface {
	Poll(ctx context.Context, req PollRequest) (*PollResult, error)
}

// pollDone is the last line of a poll script's output: proof the whole
// script ran, rather than reading a missing line as "no such file".
const pollDone = "loomux-poll-done"

// Poll checks every file with test -e and lists the tmux server's panes
// once, in one ssh operation. A session's pane is the active pane of its
// active window, the one PaneExited's display-message -t session reads.
func (e *RemoteExecutor) Poll(ctx context.Context, req PollRequest) (*PollResult, error) {
	var script strings.Builder
	for i, f := range req.Files {
		fmt.Fprintf(&script, "test -e %s && echo f%d\n", shellQuoteJoin([]string{f}), i)
	}
	if len(req.Panes) > 0 {
		// No tmux server, or no sessions, is no panes: every one is then
		// left to PaneExited.
		script.WriteString(shellQuoteJoin([]string{"tmux", "-L", e.socket, "list-panes", "-a", "-F",
			"p #{window_active}#{pane_active} #{pane_dead} #{session_name}"}) + " 2>/dev/null\n")
	}
	script.WriteString("echo " + pollDone)
	stdout, stderr, exitCode, err := e.sshExec(ctx, script.String())
	if err != nil {
		return nil, err
	}
	res, ok := parsePoll(stdout, req)
	if !ok {
		return nil, fmt.Errorf("targets: poll: %s", firstNonEmpty(stderr, fmt.Sprintf("incomplete output, exit status %d", exitCode)))
	}
	return res, nil
}

// parsePoll reads Poll's script output; ok is false unless the script
// ran to its end.
func parsePoll(out string, req PollRequest) (*PollResult, bool) {
	res := &PollResult{Exists: make([]bool, len(req.Files)), Running: make([]bool, len(req.Panes))}
	running := map[string]bool{}
	done := false
	for _, line := range strings.Split(out, "\n") {
		switch {
		case line == pollDone:
			done = true
		case strings.HasPrefix(line, "f"):
			if i, err := strconv.Atoi(line[1:]); err == nil && i >= 0 && i < len(res.Exists) {
				res.Exists[i] = true
			}
		case strings.HasPrefix(line, "p "):
			// "p <window_active><pane_active> <pane_dead> <session_name>"
			fields := strings.SplitN(line, " ", 4)
			if len(fields) == 4 && fields[1] == "11" && fields[2] == "0" {
				running[fields[3]] = true
			}
		}
	}
	for i, p := range req.Panes {
		res.Running[i] = running[p]
	}
	return res, done
}
