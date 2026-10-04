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
	// Status is the process's exit status, or -1 if it was killed by a
	// signal and so has none.
	Status int
	// Output is the pane's contents including its scrollback — a command
	// that exits at once has its output scrolled off the visible screen
	// when tmux writes its "Pane is dead" line — with that line and
	// trailing blank lines removed. Bounded by the pane's history-limit.
	Output string
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

// newSessionArgs is the tmux argument list NewSession runs, shared by the
// local and remote executors. remain-on-exit is set in the same tmux
// invocation, after new-session: tmux runs a client's command list to
// completion before servicing the new pane's process exiting, so even a
// command that dies instantly (an agent CLI that isn't installed) can't
// take its session down before the option is in place (LOOM-71).
func newSessionArgs(session, dir, command string) []string {
	args := []string{"new-session", "-d", "-s", session}
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
// "<pane_dead> <pane_dead_status>", e.g. "0 " for a live pane, "1 127"
// for an exited one, "1 " for one killed by a signal (no exit status).
func paneDeadArgs(target string) []string {
	return []string{"display-message", "-p", "-t", target, "#{pane_dead} #{pane_dead_status}"}
}

func parsePaneDead(out string) (exited bool, status int, err error) {
	dead, code, _ := strings.Cut(strings.TrimSpace(out), " ")
	switch dead {
	case "0":
		return false, 0, nil
	case "1":
		code = strings.TrimSpace(code)
		if code == "" {
			return true, -1, nil
		}
		n, err := strconv.Atoi(code)
		if err != nil {
			return false, 0, fmt.Errorf("targets: pane exit status: unparseable status %q", code)
		}
		return true, n, nil
	default:
		return false, 0, fmt.Errorf("targets: pane exit status: unexpected tmux output %q", out)
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
	exited, status, err := parsePaneDead(out)
	if err != nil || !exited {
		return nil, err
	}
	captured, err := run(ctx, paneHistoryArgs(target)...)
	if err != nil {
		return nil, err
	}
	return &PaneExit{Status: status, Output: trimDeadPaneOutput(captured)}, nil
}
