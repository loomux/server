// Package targets defines the TargetExecutor interface — the mechanism
// that runs tmux operations against a registry.Target, either locally or
// over SSH. See design spec §1.
package targets

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Loomux/server/internal/metrics"
	"github.com/Loomux/server/registry"
)

// ErrUnreachable is returned when the target itself couldn't be reached
// (e.g. an SSH connection failure), as distinct from the requested tmux
// operation failing on an otherwise-reachable target.
var ErrUnreachable = errors.New("targets: target unreachable")

// TargetExecutor runs tmux operations against a single target. session and
// target parameters are plain tmux target-spec strings (a session name, or
// "session:window.pane") — this package doesn't model window/pane
// semantics beyond that; that belongs to the orchestrator.
// TmuxSocket is the tmux server Loomux runs every session on (LOOM-93):
// `tmux -L loomux`, never the user's default one, so the user's tmux
// config and plugins don't apply to agents, Loomux sessions don't show
// in their `tmux ls`, and the orphan sweep can only ever see Loomux's
// own sessions.
const TmuxSocket = "loomux"

// SessionPrefix starts the name of every session Loomux creates.
const SessionPrefix = "loomux-"

// AttachCommand is the command a human runs on a target to attach to a
// Loomux session.
func AttachCommand(session string) string {
	return "tmux -L " + TmuxSocket + " attach -t " + session
}

type TargetExecutor interface {
	// NewSession creates a new detached tmux session. dir may be empty to
	// use the default working directory; command may be empty to start
	// the pane's default shell. A non-empty command is POSIX sh, run as
	// `sh -c command` whatever the target's default or login shell is. The session's pane is kept after its
	// command exits (tmux remain-on-exit, LOOM-71) so that an agent CLI
	// that dies at once still leaves its last output and exit status
	// readable — see PaneExited. Whoever owns the session is
	// responsible for killing it.
	NewSession(ctx context.Context, session, dir, command string) error

	// PaneExited reports whether target's pane process has exited: nil
	// while it is still running, otherwise its exit status and final
	// output (see PaneExit).
	PaneExited(ctx context.Context, target string) (*PaneExit, error)

	// HasSession reports whether a session with the given name currently
	// exists. A nonexistent session is a normal (false, nil) result, not
	// an error.
	HasSession(ctx context.Context, session string) (bool, error)

	// SendKeys sends keys literally to target, optionally followed by
	// Enter.
	SendKeys(ctx context.Context, target, keys string, enter bool) error

	// SendKey sends one key by its tmux name (e.g. "Escape", "C-c") to
	// target — not typed text (LOOM-117: interrupting an agent).
	SendKey(ctx context.Context, target, key string) error

	// CapturePane returns the current visible contents of target's pane.
	CapturePane(ctx context.Context, target string) (string, error)

	// KillSession terminates the named session.
	KillSession(ctx context.Context, session string) error

	// Close releases any resources held by the executor (e.g. an SSH
	// connection multiplexing master).
	Close() error

	// FileExists reports whether path exists on the target's
	// filesystem — a lightweight existence check, not a content read
	// (content was never the completion-detection signal; existence
	// is — see the completion package's MarkerWatcher).
	FileExists(ctx context.Context, path string) (bool, error)

	// RemoveFile best-effort removes path from the target's
	// filesystem.
	RemoveFile(ctx context.Context, path string) error

	// RunOnce runs command once, non-interactively, as POSIX sh (whatever
	// the target's login shell is), and returns its combined
	// stdout+stderr. Distinct from NewSession/SendKeys/
	// CapturePane's long-running interactive tmux pane — this is a
	// one-shot command whose output is read straight back, e.g. a
	// version-check command like "claude --version" (design spec §10
	// axis 3, router.VersionCheck).
	RunOnce(ctx context.Context, command string) (string, error)
}

// PaneExit describes a pane whose process has exited (LOOM-71).
type PaneExit struct {
	// Status is the process's exit status, or -1 if it has none: killed
	// by Signal, or (Signal 0) tmux never recorded one.
	Status int
	// Signal is the signal that killed the process, 0 if none or unknown.
	Signal int
	// Output is the pane's contents including its scrollback — a command
	// that exits at once has its output scrolled off the visible screen
	// when tmux writes its "Pane is dead" line — with that line and
	// trailing blank lines removed. Bounded by the pane's history-limit.
	Output string
}

// Describe says how the process ended: "exit 3", "killed by signal 9",
// or that tmux never recorded a status.
func (e *PaneExit) Describe() string {
	switch {
	case e.Status >= 0:
		return fmt.Sprintf("exit %d", e.Status)
	case e.Signal > 0:
		return fmt.Sprintf("killed by signal %d", e.Signal)
	default:
		return "an exit status tmux never recorded"
	}
}

// NewExecutor constructs the TargetExecutor appropriate for t.Kind.
// Metrics are disabled; use NewExecutorWithMetrics to instrument calls.
func NewExecutor(t *registry.Target) (TargetExecutor, error) {
	return NewExecutorWithMetrics(t, nil)
}

// NewExecutorWithMetrics constructs the TargetExecutor appropriate for
// t.Kind and wraps it with Prometheus instrumentation when met is non-nil
// (LOOM-103).
func NewExecutorWithMetrics(t *registry.Target, met *metrics.Metrics) (TargetExecutor, error) {
	switch t.Kind {
	case registry.TargetKindLocal:
		return newMetricsExecutor(NewLocalExecutor(), met, string(t.Kind), t.Name), nil
	case registry.TargetKindRemote:
		return newMetricsExecutor(NewRemoteExecutor(t.Host, t.User), met, string(t.Kind), t.Name), nil
	default:
		return nil, fmt.Errorf("targets: unknown target kind %q", t.Kind)
	}
}

// The size a session's window is created at (LOOM-91). A person who
// attaches resizes it to their own terminal.
const (
	sessionWidth  = "220"
	sessionHeight = "50"
)

// newSessionArgs is the tmux argument list NewSession runs, shared by the
// local and remote executors. remain-on-exit is set in the same tmux
// invocation, after new-session: tmux runs a client's command list to
// completion before servicing the new pane's process exiting, so even a
// command that dies instantly (an agent CLI that isn't installed) can't
// take its session down before the option is in place (LOOM-71).
func newSessionArgs(session, dir, command string) []string {
	// Sized (LOOM-91): a detached session is otherwise tmux's default
	// 80x24, and an agent's TUI wraps and truncates its answer to fit.
	args := []string{"new-session", "-d", "-s", session, "-x", sessionWidth, "-y", sessionHeight}
	if dir != "" {
		args = append(args, "-c", dir)
	}
	if command != "" {
		// argv form: tmux execs `sh -c <command>` directly. Given a single
		// string it would instead hand it to the target's default-shell,
		// which may be fish or anything else — and command is POSIX sh,
		// quoted for POSIX sh (credentials.ShellEnvPrefix, the probe
		// script…), so a non-POSIX shell re-parsing it could run what was
		// meant to be quoted data (LOOM-90 review).
		args = append(args, "sh", "-c", command)
	}
	return append(args, ";", "set-option", "-w", "-t", session, "remain-on-exit", "on")
}

// paneDeadArgs/parsePaneDead are PaneExited's tmux query and its parse:
// "<pane_dead> <pane_dead_status> <pane_dead_signal>", e.g. "0  " for a
// live pane, "1 127 " for an exited one, "1  9" for one killed by a
// signal (no exit status). "1  " is a pane whose pty has closed but whose
// process tmux hasn't reaped yet: it marks the pane dead first and fills
// in the status a moment later, so neither is known yet (statusPending).
// A tmux without pane_dead_signal (before 3.3) reports a signal death
// the same way; paneExited stops waiting and reports -1.
func paneDeadArgs(target string) []string {
	return []string{"display-message", "-p", "-t", target, "#{pane_dead} #{pane_dead_status} #{pane_dead_signal}"}
}

// statusPending is parsePaneDead's status for a dead pane whose exit
// status tmux hasn't recorded yet.
const statusPending = -2

// parsePaneDead reads paneDeadArgs' output: whether the pane is dead,
// its exit status (-1 if killed by a signal, statusPending if not known
// yet) and the signal (0 if none).
func parsePaneDead(out string) (exited bool, status, signal int, err error) {
	fields := strings.Split(strings.TrimRight(out, "\n"), " ")
	for len(fields) < 3 {
		fields = append(fields, "")
	}
	dead, code, sig := strings.TrimSpace(fields[0]), strings.TrimSpace(fields[1]), strings.TrimSpace(fields[2])
	switch dead {
	case "0":
		return false, 0, 0, nil
	case "1":
		if code == "" {
			if sig == "" {
				return true, statusPending, 0, nil
			}
			n, err := strconv.Atoi(sig)
			if err != nil {
				return false, 0, 0, fmt.Errorf("targets: pane exit status: unparseable signal %q", sig)
			}
			return true, -1, n, nil
		}
		n, err := strconv.Atoi(code)
		if err != nil {
			return false, 0, 0, fmt.Errorf("targets: pane exit status: unparseable status %q", code)
		}
		return true, n, 0, nil
	default:
		return false, 0, 0, fmt.Errorf("targets: pane exit status: unexpected tmux output %q", out)
	}
}

// paneHistoryArgs captures target's pane from the start of its
// scrollback.
func paneHistoryArgs(target string) []string {
	return []string{"capture-pane", "-p", "-J", "-S", "-", "-t", target}
}

// trimDeadPaneOutput drops tmux's own trailing "Pane is dead (...)" line
// and the blank lines around it.
func trimDeadPaneOutput(captured string) string {
	lines := strings.Split(strings.TrimRight(captured, " \t\n"), "\n")
	if n := len(lines); n > 0 && strings.HasPrefix(lines[n-1], "Pane is dead") {
		lines = lines[:n-1]
	}
	return strings.TrimRight(strings.Join(lines, "\n"), " \t\n")
}

// paneExited is PaneExited for either executor, given its tmux runner.
func paneExited(ctx context.Context, run func(context.Context, ...string) (string, error), target string) (*PaneExit, error) {
	out, err := run(ctx, paneDeadArgs(target)...)
	if err != nil {
		return nil, err
	}
	exited, status, signal, err := parsePaneDead(out)
	if err != nil || !exited {
		return nil, err
	}
	// Dead but not yet reaped: ask again until tmux has the status. Read
	// as "killed by a signal" it would fail a recipe that exited 0. tmux
	// reaps on its own event loop, which a busy server (many sessions,
	// a loaded host) can run late: allow it statusPendingWait.
	for waited := time.Duration(0); status == statusPending; waited += deadPaneRecaptureDelay {
		if waited >= statusPendingWait {
			status = -1
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(deadPaneRecaptureDelay):
		}
		if out, err = run(ctx, paneDeadArgs(target)...); err != nil {
			return nil, err
		}
		if _, status, signal, err = parsePaneDead(out); err != nil {
			return nil, err
		}
	}
	// tmux can mark a pane dead before it has read the process's last
	// output from the pty: a capture taken right then has nothing but the
	// "Pane is dead" line, and the failure would be reported without its
	// reason. Capture again, briefly, until the output shows up.
	var output string
	for i := 0; ; i++ {
		captured, err := run(ctx, paneHistoryArgs(target)...)
		if err != nil {
			return nil, err
		}
		output = trimDeadPaneOutput(captured)
		if output != "" || i >= deadPaneRecaptures {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(deadPaneRecaptureDelay):
		}
	}
	return &PaneExit{Status: status, Signal: signal, Output: output}, nil
}

// How long paneExited waits for a dead pane's last output: up to
// deadPaneRecaptures more captures, deadPaneRecaptureDelay apart. A
// process that printed nothing costs that once. statusPendingWait bounds
// the wait for tmux to record the exit status.
var (
	deadPaneRecaptures     = 10
	deadPaneRecaptureDelay = 100 * time.Millisecond
	statusPendingWait      = 5 * time.Second
)

// newSession is NewSession for either executor, given its tmux runner.
// tmux stops its server when the last session closes, and a new-session
// racing that shutdown fails with "server exited unexpectedly": nothing
// was created, so it's tried once more, which starts a fresh server.
func newSession(ctx context.Context, run func(context.Context, ...string) (string, error), session, dir, command string) error {
	_, err := run(ctx, newSessionArgs(session, dir, command)...)
	if err == nil || !strings.Contains(err.Error(), "server exited unexpectedly") {
		return err
	}
	select {
	case <-ctx.Done():
		return err
	case <-time.After(50 * time.Millisecond):
	}
	_, err = run(ctx, newSessionArgs(session, dir, command)...)
	return err
}
