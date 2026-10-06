package targets_test

import (
	"context"
	"os/exec"
	"testing"

	"github.com/Loomux/server/targets"
)

// A session on one instance's socket is invisible on another's, so two
// instances sharing a target never see, or sweep, each other's sessions.
func TestTmuxSocket_SeparatesInstances(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	if targets.SetTmuxSocket("bad name") == nil {
		t.Fatal("SetTmuxSocket accepted a name with a space")
	}
	t.Cleanup(func() { _ = targets.SetTmuxSocket(targets.DefaultTmuxSocket) })

	ctx := context.Background()
	ex := targets.NewLocalExecutor()
	session := targets.SessionPrefix + "socket-test"
	if err := targets.SetTmuxSocket("loomux-sockettest"); err != nil {
		t.Fatal(err)
	}
	if err := ex.NewSession(ctx, session, "", "sleep 30"); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() {
		_ = targets.SetTmuxSocket("loomux-sockettest")
		_ = ex.KillSession(context.Background(), session)
		_ = targets.SetTmuxSocket(targets.DefaultTmuxSocket)
	})
	if ok, err := ex.HasSession(ctx, session); err != nil || !ok {
		t.Fatalf("HasSession on its own socket = %v, %v", ok, err)
	}
	if got := targets.AttachCommand(session); got != "tmux -L loomux-sockettest attach -t "+session {
		t.Errorf("AttachCommand = %q", got)
	}

	if err := targets.SetTmuxSocket(targets.DefaultTmuxSocket); err != nil {
		t.Fatal(err)
	}
	if ok, _ := ex.HasSession(ctx, session); ok {
		t.Error("the default socket sees another socket's session")
	}
}
