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
			return "1 1\n", nil
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
