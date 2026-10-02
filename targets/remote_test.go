package targets_test

import (
	"context"
	"errors"
	"net"
	osexec "os/exec"
	"testing"

	"github.com/Loomux/server/targets"
	"github.com/Loomux/server/targets/executortest"
	"github.com/Loomux/server/targets/sshtest"
)

func newTestRemoteExecutor(t *testing.T, server *sshtest.Server) *targets.RemoteExecutor {
	t.Helper()
	exec := targets.NewRemoteExecutor(server.Host, "loomux-test",
		targets.WithPort(server.Port),
		targets.WithIdentityFile(server.IdentityFile),
		targets.WithExtraSSHArgs(
			"-o", "StrictHostKeyChecking=no",
			"-o", "UserKnownHostsFile=/dev/null",
		),
	)
	t.Cleanup(func() {
		if err := exec.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return exec
}

func TestRemoteExecutor(t *testing.T) {
	server := sshtest.Start(t)
	executortest.Run(t, func(t *testing.T) targets.TargetExecutor {
		return newTestRemoteExecutor(t, server)
	})
}

// TestRemoteExecutor_FishLoginShell runs the whole contract against a
// remote user whose login shell is fish (LOOM-90 review): nothing Loomux
// sends may depend on the login shell parsing POSIX sh.
func TestRemoteExecutor_FishLoginShell(t *testing.T) {
	fish, err := osexec.LookPath("fish")
	if err != nil {
		t.Skip("fish not installed")
	}
	server := sshtest.StartWithLoginShell(t, fish)
	executortest.Run(t, func(t *testing.T) targets.TargetExecutor {
		return newTestRemoteExecutor(t, server)
	})
}

// TestRemoteExecutor_ReusesControlMaster asserts the SSH multiplexing
// this ticket specifically requires is actually happening: after the
// first operation establishes a ControlMaster, `ssh -O check` against
// the same ControlPath should succeed, proving the master is alive and
// reusable rather than each call paying a fresh handshake.
func TestRemoteExecutor_ReusesControlMaster(t *testing.T) {
	server := sshtest.Start(t)
	exec := newTestRemoteExecutor(t, server)

	ctx := context.Background()
	if _, err := exec.HasSession(ctx, "irrelevant"); err != nil {
		t.Fatalf("HasSession: %v", err)
	}

	alive, err := exec.ControlMasterAlive(ctx)
	if err != nil {
		t.Fatalf("ControlMasterAlive: %v", err)
	}
	if !alive {
		t.Fatalf("ControlMasterAlive = false after a successful operation, want true (multiplexing not set up)")
	}

	if _, err := exec.HasSession(ctx, "irrelevant"); err != nil {
		t.Fatalf("HasSession (second call, should reuse master): %v", err)
	}
}

// TestRemoteExecutor_Unreachable asserts a target that can't be connected
// to at all (nothing listening) surfaces as ErrUnreachable, bounded by a
// short connect timeout so this test can't hang.
func TestRemoteExecutor_Unreachable(t *testing.T) {
	closedPort := unusedPort(t)

	exec := targets.NewRemoteExecutor("127.0.0.1", "loomux-test",
		targets.WithPort(closedPort),
		targets.WithConnectTimeout("2s"),
		targets.WithExtraSSHArgs(
			"-o", "StrictHostKeyChecking=no",
			"-o", "UserKnownHostsFile=/dev/null",
		),
	)
	t.Cleanup(func() { exec.Close() })

	_, err := exec.HasSession(context.Background(), "irrelevant")
	if !errors.Is(err, targets.ErrUnreachable) {
		t.Fatalf("HasSession against a closed port: err = %v, want ErrUnreachable", err)
	}
}

// unusedPort finds a port nothing is listening on by briefly binding and
// releasing it — good enough for a test that needs a connection attempt
// to fail quickly, not a race-free reservation.
func unusedPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("unusedPort: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}
