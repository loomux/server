package targets

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Loomux/server/registry"
)

// LOOM-141: a local target's processes don't inherit loomuxd's secrets.
func TestLocalEnv_KeepsOnlyAllowedVariables(t *testing.T) {
	t.Setenv("LOOMUX_MASTER_KEY", "vault-key")
	t.Setenv("LOOMUX_ROUTER_PRIMARY_API_KEY", "router-key")
	t.Setenv("LC_ALL", "C.UTF-8")
	env := strings.Join(localEnv(), "\n")
	for _, secret := range []string{"vault-key", "router-key", "LOOMUX_"} {
		if strings.Contains(env, secret) {
			t.Errorf("local env carries %q:\n%s", secret, env)
		}
	}
	for _, kept := range []string{"PATH=", "HOME=", "LC_ALL=C.UTF-8"} {
		if !strings.Contains(env, kept) {
			t.Errorf("local env lacks %s", kept)
		}
	}
}

func TestLocalExecutor_RunOnceDoesNotSeeLoomuxSecrets(t *testing.T) {
	t.Setenv("LOOMUX_MASTER_KEY", "vault-key")
	out, err := NewLocalExecutor().RunOnce(context.Background(), "env")
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if strings.Contains(out, "vault-key") || strings.Contains(out, "LOOMUX_MASTER_KEY") {
		t.Fatalf("a local command saw loomuxd's vault key:\n%s", out)
	}
}

// LOOM-141: with local targets off, nothing runs on one.
func TestNewExecutor_LocalTargetsOff(t *testing.T) {
	defer SetLocalTargets(LocalTargets)
	SetLocalTargets(false)
	_, err := NewExecutor(&registry.Target{Name: "box", Kind: registry.TargetKindLocal})
	if !errors.Is(err, ErrLocalTargetsOff) {
		t.Fatalf("NewExecutor(local) err = %v, want ErrLocalTargetsOff", err)
	}
	if _, err := NewExecutor(&registry.Target{Name: "r", Kind: registry.TargetKindRemote, Host: "h"}); err != nil {
		t.Fatalf("NewExecutor(remote) with local off: %v", err)
	}
}

// LOOM-141: a tmux server started with loomuxd's full environment (by an
// earlier loomuxd) hands it to every new session; ScrubLocalTmuxEnv
// removes it.
func TestScrubLocalTmuxEnv_CleansARunningServer(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("no tmux")
	}
	old := TmuxSocket
	TmuxSocket = fmt.Sprintf("loomux-scrubtest-%d", os.Getpid())
	t.Cleanup(func() {
		_ = exec.Command("tmux", "-L", TmuxSocket, "kill-server").Run()
		TmuxSocket = old
	})
	ctx := context.Background()

	start := exec.Command("tmux", "-L", TmuxSocket, "new-session", "-d", "-s", "first", "sleep 60")
	start.Env = append(os.Environ(), "LOOMUX_MASTER_KEY=leak")
	if out, err := start.CombinedOutput(); err != nil {
		t.Fatalf("start tmux: %v: %s", err, out)
	}
	if err := ScrubLocalTmuxEnv(ctx); err != nil {
		t.Fatalf("ScrubLocalTmuxEnv: %v", err)
	}

	envFile := filepath.Join(t.TempDir(), "env")
	if err := NewLocalExecutor().NewSession(ctx, "second", t.TempDir(), "env > "+envFile+"; sleep 60"); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	var got []byte
	for i := 0; i < 50; i++ {
		if got, _ = os.ReadFile(envFile); len(got) > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(got) == 0 {
		t.Fatal("the new session never wrote its environment")
	}
	if strings.Contains(string(got), "LOOMUX_MASTER_KEY") {
		t.Fatalf("a new session still inherited the old server's LOOMUX_MASTER_KEY:\n%s", got)
	}
	if !strings.Contains(string(got), "PATH=") {
		t.Errorf("the scrub took PATH too:\n%s", got)
	}

	// Nothing running is fine.
	_ = exec.Command("tmux", "-L", TmuxSocket, "kill-server").Run()
	if err := ScrubLocalTmuxEnv(ctx); err != nil {
		t.Fatalf("ScrubLocalTmuxEnv with no server: %v", err)
	}
}

// fakeTmuxServer is a tmux on PATH whose server holds FOO in its global
// environment and refuses every set-environment; with exitOnSet, the
// first set-environment finds the server gone (its last session ended
// in between) and later commands find no server.
func fakeTmuxServer(t *testing.T, exitOnSet bool) {
	t.Helper()
	dir := t.TempDir()
	gone := filepath.Join(dir, "gone")
	onSet := "exit 1"
	if exitOnSet {
		onSet = "touch '" + gone + "'; echo 'server exited unexpectedly' >&2; exit 1"
	}
	script := "#!/bin/sh\n[ -e '" + gone + "' ] && { echo 'no server running' >&2; exit 1; }\n" +
		"case \"$*\" in\n*show-environment*) echo FOO=1 ;;\n*set-environment*) " + onSet + " ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
}

// A server that exits mid-scrub (its last session ended: another
// loomuxd's, or a test's in a parallel package) leaves nothing to scrub;
// the next server starts with localEnv. Build used to fail on it with
// "scrub tmux environment: FOO: exit status 1".
func TestScrubLocalTmuxEnv_ServerExitsMidScrub(t *testing.T) {
	fakeTmuxServer(t, true)
	if err := ScrubLocalTmuxEnv(context.Background()); err != nil {
		t.Fatalf("ScrubLocalTmuxEnv: %v", err)
	}
}

// A server that's still there and won't unset a variable is an error.
func TestScrubLocalTmuxEnv_RunningServerRefuses(t *testing.T) {
	fakeTmuxServer(t, false)
	if err := ScrubLocalTmuxEnv(context.Background()); err == nil {
		t.Fatal("ScrubLocalTmuxEnv = nil, want the refused unset")
	}
}
