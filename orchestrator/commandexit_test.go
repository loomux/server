package orchestrator

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Loomux/server/targets"
)

func TestCommandExit(t *testing.T) {
	for _, tc := range []struct {
		name       string
		in         targets.PaneExit
		wantStatus int
		wantOutput string
	}{
		{"marker wins over a status tmux didn't record", targets.PaneExit{Status: -1, Output: "done\n\n[loomux:exit=0]"}, 0, "done"},
		{"non-zero", targets.PaneExit{Status: 3, Output: "boom\n\n[loomux:exit=3]\n"}, 3, "boom"},
		{"no output", targets.PaneExit{Status: 0, Output: "\n[loomux:exit=0]"}, 0, ""},
		{"no marker: as tmux saw it", targets.PaneExit{Status: -1, Signal: "9", Output: "partial"}, -1, "partial"},
		{"a marker that isn't last is the command's own text", targets.PaneExit{Status: 2, Output: "[loomux:exit=0]\nmore"}, 2, "[loomux:exit=0]\nmore"},
	} {
		got := CommandExit(&tc.in)
		if got.Status != tc.wantStatus || got.Output != tc.wantOutput {
			t.Errorf("%s: CommandExit = %+v, want status %d output %q", tc.name, got, tc.wantStatus, tc.wantOutput)
		}
	}
	if CommandExit(nil) != nil {
		t.Error("CommandExit(nil) != nil")
	}
}

// The wrapper reports the command's status through a real tmux pane,
// including a command that calls exit itself.
func TestWrapCommandInTmux(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	ex := targets.NewLocalExecutor()
	ctx := context.Background()
	for _, tc := range []struct {
		command    string
		wantStatus int
		wantOutput string
	}{
		{"echo provisioned", 0, "provisioned"},
		{"echo failing >&2; exit 3", 3, "failing"},
		{"false", 1, ""},
		{"cat <<'X'\nheredoc line\nX", 0, "heredoc line"},
		{"echo end # a trailing comment", 0, "end"},
	} {
		session := "loomux-test-wrap-" + strings.ReplaceAll(t.Name(), "/", "-") + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
		if err := ex.NewSession(ctx, session, "", wrapCommand(tc.command)); err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		t.Cleanup(func() { _ = ex.KillSession(context.Background(), session) })
		var exit *targets.PaneExit
		for deadline := time.Now().Add(10 * time.Second); exit == nil && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			var err error
			if exit, err = ex.PaneExited(ctx, session); err != nil {
				t.Fatalf("PaneExited: %v", err)
			}
		}
		if exit == nil {
			t.Fatalf("%q never exited", tc.command)
		}
		got := CommandExit(exit)
		if got.Status != tc.wantStatus || got.Output != tc.wantOutput {
			t.Errorf("%q: %+v, want status %d output %q", tc.command, got, tc.wantStatus, tc.wantOutput)
		}
	}
}
