package targets_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Loomux/server/internal/tmuxtest"
	"github.com/Loomux/server/targets"
)

// LOOM-272: the read-only attach command really is read-only, on the
// tmux this runs against: a client attached with it can't put keys into
// the pane, where one attached with the writable command can. That is
// the whole of what a watch relies on, so it is checked against tmux
// itself rather than by reading the command.
//
// The clients are control-mode ones (-C), which need no terminal and
// take tmux commands on stdin: tmux refuses a read-only client's
// send-keys, as it drops the keys of a read-only terminal client.
func TestReadOnlyAttachCommand_ClientCannotType(t *testing.T) {
	socket := tmuxtest.Socket(t)
	exec := targets.NewLocalExecutor(socket)
	ctx := context.Background()
	session := fmt.Sprintf("loomux-ro-%d", time.Now().UnixNano())
	// cat shows what reaches the pane: the terminal echoes typed keys.
	if err := exec.NewSession(ctx, session, "", "cat"); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = exec.KillSession(context.Background(), session) })

	// typeVia attaches a control-mode client with attach, one of the two
	// commands, has it type text into the session, and returns what tmux
	// answered. The client stays for a moment, then its stdin ends and it
	// detaches.
	typeVia := func(attach, text string) string {
		t.Helper()
		control := strings.Replace(attach, " attach ", " -C attach ", 1)
		if control == attach {
			t.Fatalf("%q isn't a tmux attach", attach)
		}
		// A refused command ends the client with a failure status; what
		// tmux said is the answer either way.
		out, err := exec.RunOnce(ctx, "(printf 'send-keys -t %s -l %s\\n' "+session+" "+text+"; sleep 1) | "+control+" 2>&1 || true")
		if err != nil {
			t.Fatalf("control client via %q: %v: %s", attach, err, out)
		}
		return out
	}
	reached := func(text string) bool {
		t.Helper()
		pane, err := exec.CapturePane(ctx, session)
		if err != nil {
			t.Fatalf("CapturePane: %v", err)
		}
		return strings.Contains(pane, text)
	}

	out := typeVia(targets.ReadOnlyAttachCommand(socket, session), "typed-by-a-watcher")
	if reached("typed-by-a-watcher") {
		t.Fatalf("keys from a client attached with the read-only command reached the pane; tmux said: %s", out)
	}
	if !strings.Contains(out, "%error") || !strings.Contains(out, "read-only") {
		t.Errorf("tmux didn't refuse the read-only client's send-keys as read-only: %s", out)
	}

	// The same through the writable command does type: the check above
	// would have seen it.
	out = typeVia(targets.AttachCommand(socket, session), "typed-by-a-driver")
	if !reached("typed-by-a-driver") {
		t.Fatalf("keys from a client attached with the writable command didn't reach the pane; tmux said: %s", out)
	}
	if reached("typed-by-a-watcher") {
		t.Error("the watcher's keys reached the pane after all")
	}
}
