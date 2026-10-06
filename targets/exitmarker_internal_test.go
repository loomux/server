package targets

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestExitMarker(t *testing.T) {
	for _, tc := range []struct {
		name, in   string
		wantStatus int
		wantRest   string
		wantOK     bool
	}{
		{"exit 0", "done\n\n[loomux:exit=0]", 0, "done", true},
		{"non-zero", "boom\n\n[loomux:exit=3]\n", 3, "boom", true},
		{"no output", "\n[loomux:exit=127]", 127, "", true},
		{"no marker", "partial", 0, "partial", false},
		{"a marker that isn't last is the command's own text", "[loomux:exit=0]\nmore", 0, "[loomux:exit=0]\nmore", false},
	} {
		s, rest, ok := exitMarker(tc.in)
		if ok != tc.wantOK || rest != tc.wantRest || (ok && s != tc.wantStatus) {
			t.Errorf("%s: exitMarker = (%d, %q, %v), want (%d, %q, %v)", tc.name, s, rest, ok, tc.wantStatus, tc.wantRest, tc.wantOK)
		}
	}
}

// A dead pane whose output ends with the marker reports its status at
// once, however long tmux takes to record its own (#239 CI: a missing
// agent CLI waited out the pending-status bound).
func TestPaneExitedTakesTheMarkerWithoutWaiting(t *testing.T) {
	queries := 0
	run := func(ctx context.Context, args ...string) (string, error) {
		if strings.Contains(strings.Join(args, " "), "pane_dead") {
			queries++
			return "1  \n", nil // never reaped
		}
		return "sh: loomux-no-such-cli: not found\n\n[loomux:exit=127]\nPane is dead (status 127, x)\n", nil
	}
	start := time.Now()
	exit, err := paneExited(context.Background(), run, "s")
	if err != nil {
		t.Fatal(err)
	}
	if exit.Status != 127 || exit.Output != "sh: loomux-no-such-cli: not found" || queries != 1 {
		t.Fatalf("exit = %+v after %d status queries", exit, queries)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("took %s, want at once", d)
	}
}

// The marker can show up in a later capture than the first: it's picked
// up while waiting for tmux's status.
func TestPaneExitedFindsALateMarker(t *testing.T) {
	old := deadPaneRecaptureDelay
	deadPaneRecaptureDelay = time.Millisecond
	t.Cleanup(func() { deadPaneRecaptureDelay = old })
	captures := 0
	run := func(ctx context.Context, args ...string) (string, error) {
		if strings.Contains(strings.Join(args, " "), "pane_dead") {
			return "1  \n", nil
		}
		captures++
		if captures < 3 {
			return "working\n", nil
		}
		return "working\n\n[loomux:exit=0]\n", nil
	}
	exit, err := paneExited(context.Background(), run, "s")
	if err != nil || exit.Status != 0 || exit.Output != "working" {
		t.Fatalf("exit = %+v, %v", exit, err)
	}
}

// Through a real tmux: the wrapped command's status and output, including
// a command that exits itself and one that isn't installed.
func TestWrapExitStatusInTmux(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	ex := NewLocalExecutor()
	ctx := context.Background()
	for i, tc := range []struct {
		command    string
		wantStatus int
		wantOutput string
	}{
		{"echo provisioned", 0, "provisioned"},
		{"echo failing >&2; exit 3", 3, "failing"},
		{"false", 1, ""},
		{"cat <<'X'\nheredoc line\nX", 0, "heredoc line"},
		{"echo end # a trailing comment", 0, "end"},
		{"loomux-no-such-cli-xyz", 127, "loomux-no-such-cli-xyz"},
	} {
		session := SessionPrefix + "wrap-" + strconv.Itoa(i) + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
		if err := ex.NewSession(ctx, session, "", WrapExitStatus(tc.command)); err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		t.Cleanup(func() { _ = ex.KillSession(context.Background(), session) })
		start := time.Now()
		var exit *PaneExit
		for exit == nil && time.Since(start) < 10*time.Second {
			var err error
			if exit, err = ex.PaneExited(ctx, session); err != nil {
				t.Fatalf("PaneExited: %v", err)
			}
			if exit == nil {
				time.Sleep(20 * time.Millisecond)
			}
		}
		if exit == nil {
			t.Fatalf("%q never exited", tc.command)
		}
		if exit.Status != tc.wantStatus || !strings.Contains(exit.Output, tc.wantOutput) || strings.Contains(exit.Output, "[loomux:exit=") {
			t.Errorf("%q: %+v, want status %d output containing %q, marker stripped", tc.command, exit, tc.wantStatus, tc.wantOutput)
		}
	}
}

// A C-c typed into a wrapped pane (a human who attached) reaches the
// wrapping shells too; they must carry on, so a command that catches
// SIGINT and finishes still reports its own status (#240 review: dash and
// busybox ash died on it). Run under the system sh and, when present,
// busybox ash, the loomux image's own shell.
func TestWrapExitStatusSurvivesCtrlC(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	// Catches SIGINT, says so, and exits 0 about a second later.
	child := `trap 'echo caught' INT; i=0; while [ $i -lt 10 ]; do sleep 0.1; i=$((i+1)); done; echo finished; exit 0`
	shells := map[string]string{"sh": ""}
	if _, err := exec.LookPath("busybox"); err == nil {
		shells["busybox ash"] = "busybox sh -c "
	}
	ex := NewLocalExecutor()
	ctx := context.Background()
	for name, prefix := range shells {
		t.Run(name, func(t *testing.T) {
			cmd := WrapExitStatus(child)
			if prefix != "" {
				cmd = prefix + "'" + strings.ReplaceAll(cmd, "'", `'\''`) + "'"
			}
			session := SessionPrefix + "ctrlc-" + strconv.FormatInt(time.Now().UnixNano(), 36)
			if err := ex.NewSession(ctx, session, "", cmd); err != nil {
				t.Fatalf("NewSession: %v", err)
			}
			t.Cleanup(func() { _ = ex.KillSession(context.Background(), session) })
			time.Sleep(300 * time.Millisecond)
			if err := ex.SendKey(ctx, session, "C-c"); err != nil {
				t.Fatalf("SendKey C-c: %v", err)
			}
			var exit *PaneExit
			for start := time.Now(); exit == nil && time.Since(start) < 10*time.Second; {
				var err error
				if exit, err = ex.PaneExited(ctx, session); err != nil {
					t.Fatalf("PaneExited: %v", err)
				}
				if exit == nil {
					time.Sleep(50 * time.Millisecond)
				}
			}
			if exit == nil {
				t.Fatal("never exited")
			}
			if exit.Status != 0 || !strings.Contains(exit.Output, "caught") || !strings.Contains(exit.Output, "finished") {
				t.Errorf("after C-c: %+v, want the command's own exit 0 with its output", exit)
			}
		})
	}
}
