package targets_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"testing"

	"github.com/Loomux/server/internal/tmuxtest"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/targets"
	"github.com/Loomux/server/targets/executortest"
	"github.com/Loomux/server/targets/sshtest"
)

// A session on one instance's socket is invisible on another's, so two
// instances sharing a target never see, or sweep, each other's sessions
// (LOOM-93). The socket is each executor's (LOOM-202): two instances in
// one process use theirs at the same time.
func TestTmuxSocket_SeparatesInstances(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	local := &registry.Target{Name: "box", Kind: registry.TargetKindLocal}
	if _, err := targets.NewExecutorWith(local, targets.ExecutorOptions{TmuxSocket: "bad name"}); err == nil {
		t.Fatal("NewExecutorWith accepted a socket name with a space")
	}

	sockets := [2]string{tmuxtest.Socket(t), tmuxtest.Socket(t)}
	var execs [2]targets.TargetExecutor
	for i, socket := range sockets {
		e, err := targets.NewExecutorWith(local, targets.ExecutorOptions{TmuxSocket: socket})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = e.Close() })
		if got := e.TmuxSocket(); got != socket {
			t.Errorf("TmuxSocket() = %q, want %q", got, socket)
		}
		execs[i] = e
	}

	ctx := context.Background()
	session := func(i int) string { return fmt.Sprintf("%ssocket-test-%d-%d", targets.SessionPrefix, os.Getpid(), i) }
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			own, other := execs[i%2], execs[1-i%2]
			if err := own.NewSession(ctx, session(i), "", "sleep 30"); err != nil {
				t.Errorf("%d: NewSession: %v", i, err)
				return
			}
			if ok, err := own.HasSession(ctx, session(i)); err != nil || !ok {
				t.Errorf("%d: HasSession on its own socket = %v, %v", i, ok, err)
			}
			if ok, _ := other.HasSession(ctx, session(i)); ok {
				t.Errorf("%d: the other instance's socket sees its session", i)
			}
		}()
	}
	wg.Wait()

	if ok, _ := targets.NewLocalExecutor(tmuxtest.Socket(t)).HasSession(ctx, session(0)); ok {
		t.Error("a third socket sees an instance's session")
	}
	if got, want := targets.AttachCommand(sockets[0], "s"), "tmux -L "+sockets[0]+" attach -t s"; got != want {
		t.Errorf("AttachCommand = %q, want %q", got, want)
	}
}

// The whole executor contract holds on a socket of the instance's own,
// locally and over SSH: every operation, and every raw tmux command the
// suite runs through RunOnce, is on that socket (LOOM-202).
func TestLocalExecutor_OwnSocket(t *testing.T) {
	socket := tmuxtest.Socket(t)
	local := &registry.Target{Name: "box", Kind: registry.TargetKindLocal}
	executortest.Run(t, func(t *testing.T) targets.TargetExecutor {
		e, err := targets.NewExecutorWith(local, targets.ExecutorOptions{TmuxSocket: socket})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = e.Close() })
		return e
	})
}

func TestRemoteExecutor_OwnSocket(t *testing.T) {
	socket := tmuxtest.Socket(t)
	server := sshtest.Start(t)
	executortest.Run(t, func(t *testing.T) targets.TargetExecutor {
		return newTestRemoteExecutor(t, server, targets.WithTmuxSocket(socket))
	})
}
