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
