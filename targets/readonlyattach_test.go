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
// tmux this runs against: a person at a terminal attached with it can't
// put anything into the pane, by typing or through tmux's own command
// prompt, where one attached with the writable command can do both.
// That is the whole of what a watch relies on, so it is checked against
// tmux itself rather than by reading the command.
//
// The terminal is a pane of a second tmux server that runs the attach
// command, as a person's terminal would: keys sent to that pane are
// typed at the attached client. (A control-mode client, -C, wouldn't do:
// whether tmux refuses a read-only control client's commands depends on
// its version, and it isn't what the attach command starts.)
func TestReadOnlyAttachCommand_ClientCannotType(t *testing.T) {
	ctx := context.Background()
	socket := tmuxtest.Socket(t)
	exec := targets.NewLocalExecutor(socket)
	session := fmt.Sprintf("loomux-ro-%d", time.Now().UnixNano())
	// cat shows what reaches the pane: the terminal echoes typed keys.
	if err := exec.NewSession(ctx, session, "", "cat"); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = exec.KillSession(context.Background(), session) })
	terminals := targets.NewLocalExecutor(tmuxtest.Socket(t))

	clients := func() int {
		t.Helper()
		out, err := exec.RunOnce(ctx, "tmux -L "+socket+" list-clients -t '="+session+"' -F '#{client_name}'")
		if err != nil {
			t.Fatalf("list-clients: %v: %s", err, out)
		}
		return len(strings.Fields(out))
	}
	waitClients := func(want int, what string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for clients() != want {
			if time.Now().After(deadline) {
				t.Fatalf("%s: the session has %d clients, want %d", what, clients(), want)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	pane := func() string {
		t.Helper()
		out, err := exec.CapturePane(ctx, session)
		if err != nil {
			t.Fatalf("CapturePane: %v", err)
		}
		return out
	}

	// typeAt opens a terminal attached with attach and, at it, types
	// text, then asks tmux's command prompt (prefix :) to send-keys
	// prompted, then detaches (prefix d). The client handles its keys in
	// order, so once it has detached, everything before was handled:
	// what reached the pane has, and what didn't won't.
	typeAt := func(attach, text, prompted string) {
		t.Helper()
		terminal := "terminal-" + text
		// TMUX is the terminal's own tmux: unset, as in a person's plain
		// terminal, or tmux refuses to attach from inside tmux.
		if err := terminals.NewSession(ctx, terminal, "", "env -u TMUX "+attach); err != nil {
			t.Fatalf("opening a terminal running %q: %v", attach, err)
		}
		t.Cleanup(func() { _ = terminals.KillSession(context.Background(), terminal) })
		waitClients(1, "attaching with "+attach)

		keys := func(err error) {
			t.Helper()
			if err != nil {
				t.Fatalf("typing at the terminal running %q: %v", attach, err)
			}
		}
		keys(terminals.SendKeys(ctx, terminal, text, true))
		keys(terminals.SendKey(ctx, terminal, "C-b"))
		keys(terminals.SendKeys(ctx, terminal, ":", false))
		keys(terminals.SendKeys(ctx, terminal, "send-keys -t "+session+" -l "+prompted, true))
		keys(terminals.SendKey(ctx, terminal, "C-b"))
		keys(terminals.SendKeys(ctx, terminal, "d", false))
		waitClients(0, "detaching from "+attach)
	}

	typeAt(targets.ReadOnlyAttachCommand(socket, session), "typed-by-a-watcher", "prompted-by-a-watcher")
	if got := pane(); strings.Contains(got, "typed-by-a-watcher") || strings.Contains(got, "prompted-by-a-watcher") {
		t.Fatalf("a terminal attached with the read-only command put this into the pane:\n%s", got)
	}

	// The same at a terminal attached with the writable command does
	// both: the check above would have seen it.
	typeAt(targets.AttachCommand(socket, session), "typed-by-a-driver", "prompted-by-a-driver")
	got := pane()
	if !strings.Contains(got, "typed-by-a-driver") {
		t.Errorf("keys typed at a terminal attached with the writable command didn't reach the pane:\n%s", got)
	}
	if !strings.Contains(got, "prompted-by-a-driver") {
		t.Errorf("a send-keys from the command prompt of a terminal attached with the writable command didn't reach the pane:\n%s", got)
	}
	if strings.Contains(got, "by-a-watcher") {
		t.Errorf("the watcher's keys reached the pane after all:\n%s", got)
	}
}
