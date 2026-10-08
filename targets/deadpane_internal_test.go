package targets

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A pane reported dead before tmux has read the process's last output
// (seen on CI: a provisioning failure with no reason): capture again
// until it shows.
func TestPaneExitedWaitsForLastOutput(t *testing.T) {
	old := deadPaneRecaptureDelay
	deadPaneRecaptureDelay = time.Millisecond
	t.Cleanup(func() { deadPaneRecaptureDelay = old })

	captures := 0
	run := func(ctx context.Context, args ...string) (string, error) {
		if strings.Contains(strings.Join(args, " "), "pane_dead") {
			return "1 1 \n", nil
		}
		captures++
		if captures < 3 {
			return "\nPane is dead (status 1, Mon Oct  5 12:40:09 2026)\n", nil
		}
		return "ls: no directory\nPane is dead (status 1, Mon Oct  5 12:40:09 2026)\n", nil
	}
	exit, err := paneExited(context.Background(), run, "s")
	if err != nil {
		t.Fatal(err)
	}
	if exit == nil || exit.Status != 1 || exit.Output != "ls: no directory" || captures != 3 {
		t.Fatalf("exit = %+v after %d captures", exit, captures)
	}
}

// A pane whose pty closed before tmux reaped its process reads "1  " for
// a moment (seen on CI: a recipe that exited 0 failed "with status -1"):
// ask again until the status is there.
func TestPaneExitedWaitsForExitStatus(t *testing.T) {
	old := deadPaneRecaptureDelay
	deadPaneRecaptureDelay = time.Millisecond
	t.Cleanup(func() { deadPaneRecaptureDelay = old })

	queries := 0
	run := func(ctx context.Context, args ...string) (string, error) {
		if strings.Contains(strings.Join(args, " "), "pane_dead") {
			queries++
			if queries < 3 {
				return "1  \n", nil
			}
			return "1 0 \n", nil
		}
		return "done\nPane is dead (status 0, Mon Oct  5 12:40:09 2026)\n", nil
	}
	exit, err := paneExited(context.Background(), run, "s")
	if err != nil {
		t.Fatal(err)
	}
	if exit == nil || exit.Status != 0 || exit.Output != "done" || queries != 3 {
		t.Fatalf("exit = %+v after %d status queries", exit, queries)
	}
}

// A tmux that never fills in the status (a signal death on a tmux without
// pane_dead_signal) still ends the wait, as killed by a signal.
func TestPaneExitedGivesUpOnPendingStatus(t *testing.T) {
	oldDelay, oldWait := deadPaneRecaptureDelay, statusPendingWait
	deadPaneRecaptureDelay, statusPendingWait = time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { deadPaneRecaptureDelay, statusPendingWait = oldDelay, oldWait })

	run := func(ctx context.Context, args ...string) (string, error) {
		if strings.Contains(strings.Join(args, " "), "pane_dead") {
			return "1 \n", nil
		}
		return "x\n", nil
	}
	exit, err := paneExited(context.Background(), run, "s")
	if err != nil {
		t.Fatal(err)
	}
	if exit == nil || exit.Status != -1 || exit.Signal != "" {
		t.Fatalf("exit = %+v, want status -1 and no signal", exit)
	}
}

// A process killed by a signal is reported with the signal, at once.
func TestPaneExitedReportsSignal(t *testing.T) {
	queries := 0
	run := func(ctx context.Context, args ...string) (string, error) {
		if strings.Contains(strings.Join(args, " "), "pane_dead") {
			queries++
			return "1  15\n", nil
		}
		return "x\n", nil
	}
	exit, err := paneExited(context.Background(), run, "s")
	if err != nil {
		t.Fatal(err)
	}
	if exit == nil || exit.Status != -1 || exit.Signal != "15" || queries != 1 {
		t.Fatalf("exit = %+v after %d queries, want signal 15 at once", exit, queries)
	}
}

// tmux can miss the SIGCHLD of a command that exits the instant its pane
// starts, and then never records its status on its own (LOOM-181): the
// wait asks the server to reap rather than only waiting for it.
func TestPaneExitedAsksTmuxToReap(t *testing.T) {
	old := deadPaneRecaptureDelay
	deadPaneRecaptureDelay = time.Millisecond
	t.Cleanup(func() { deadPaneRecaptureDelay = old })

	reaped := false
	run := func(ctx context.Context, args ...string) (string, error) {
		joined := strings.Join(args, " ")
		switch {
		case args[0] == "run-shell":
			if joined != "run-shell kill -CHLD #{pid}" {
				t.Errorf("reap command = %q", joined)
			}
			reaped = true
			return "", nil
		case strings.Contains(joined, "pane_dead"):
			if reaped {
				return "1 127 \n", nil
			}
			return "1  \n", nil
		}
		return "sh: loomux-no-such-cli: not found\nPane is dead (status 127, Mon Oct  5 12:40:09 2026)\n", nil
	}
	exit, err := paneExited(context.Background(), run, "s")
	if err != nil {
		t.Fatal(err)
	}
	if exit == nil || exit.Status != 127 || exit.Output != "sh: loomux-no-such-cli: not found" {
		t.Fatalf("exit = %+v, want status 127 once tmux was asked to reap", exit)
	}
}

// Output tmux reads only after deadPaneOutput gave up on it is still
// reported, when it arrives while the status is pending.
func TestPaneExitedKeepsLateOutput(t *testing.T) {
	oldDelay, oldRecaptures := deadPaneRecaptureDelay, deadPaneRecaptures
	deadPaneRecaptureDelay, deadPaneRecaptures = time.Millisecond, 1
	t.Cleanup(func() { deadPaneRecaptureDelay, deadPaneRecaptures = oldDelay, oldRecaptures })

	captures, queries := 0, 0
	run := func(ctx context.Context, args ...string) (string, error) {
		joined := strings.Join(args, " ")
		switch {
		case args[0] == "run-shell":
			return "", nil
		case strings.Contains(joined, "pane_dead"):
			queries++
			if queries < 3 {
				return "1  \n", nil
			}
			return "1 0 \n", nil
		}
		captures++
		if captures < 4 {
			return "\nPane is dead (status 0, Mon Oct  5 12:40:09 2026)\n", nil
		}
		return "late\nPane is dead (status 0, Mon Oct  5 12:40:09 2026)\n", nil
	}
	exit, err := paneExited(context.Background(), run, "s")
	if err != nil {
		t.Fatal(err)
	}
	if exit == nil || exit.Status != 0 || exit.Output != "late" {
		t.Fatalf("exit = %+v, want status 0 and the late output", exit)
	}
}
