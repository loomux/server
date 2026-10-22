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
	off := ExecutorOptions{LocalTargetsOff: true}
	_, err := NewExecutorWith(&registry.Target{Name: "box", Kind: registry.TargetKindLocal}, off)
	if !errors.Is(err, ErrLocalTargetsOff) {
		t.Fatalf("NewExecutor(local) err = %v, want ErrLocalTargetsOff", err)
	}
	if _, err := NewExecutorWith(&registry.Target{Name: "r", Kind: registry.TargetKindRemote, Host: "h"}, off); err != nil {
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
	socket := fmt.Sprintf("loomux-scrubtest-%d", os.Getpid())
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", socket, "kill-server").Run() })
	ctx := context.Background()

	start := exec.Command("tmux", "-L", socket, "new-session", "-d", "-s", "first", "sleep 60")
	start.Env = append(os.Environ(), "LOOMUX_MASTER_KEY=leak")
	if out, err := start.CombinedOutput(); err != nil {
		t.Fatalf("start tmux: %v: %s", err, out)
	}
	if err := ScrubLocalTmuxEnv(ctx, socket); err != nil {
		t.Fatalf("ScrubLocalTmuxEnv: %v", err)
	}

	envFile := filepath.Join(t.TempDir(), "env")
	if err := (&LocalExecutor{socket: socket}).NewSession(ctx, "second", t.TempDir(), "env > "+envFile+"; sleep 60"); err != nil {
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
	_ = exec.Command("tmux", "-L", socket, "kill-server").Run()
	if err := ScrubLocalTmuxEnv(ctx, socket); err != nil {
		t.Fatalf("ScrubLocalTmuxEnv with no server: %v", err)
	}
}

// fakeTmuxServer is a tmux on PATH whose server holds FOO in its global
// environment. Its set-environment does as onSet says: "refuse" fails;
// "exit" finds the server gone (its last session ended in between), and
// later commands find no server; "hang" doesn't return.
func fakeTmuxServer(t *testing.T, onSet string) {
	t.Helper()
	dir := t.TempDir()
	gone := filepath.Join(dir, "gone")
	set, ok := map[string]string{
		"refuse": "exit 1",
		"exit":   "touch '" + gone + "'; echo 'server exited unexpectedly' >&2; exit 1",
		"hang":   "exec sleep 60",
	}[onSet]
	if !ok {
		t.Fatalf("fakeTmuxServer: no %q", onSet)
	}
	script := "#!/bin/sh\n[ -e '" + gone + "' ] && { echo 'no server running' >&2; exit 1; }\n" +
		"case \"$*\" in\n*show-environment*) echo FOO=1 ;;\n*set-environment*) " + set + " ;;\nesac\n"
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
	fakeTmuxServer(t, "exit")
	if err := ScrubLocalTmuxEnv(context.Background(), ""); err != nil {
		t.Fatalf("ScrubLocalTmuxEnv: %v", err)
	}
}

// A server that's still there and won't unset a variable is an error.
func TestScrubLocalTmuxEnv_RunningServerRefuses(t *testing.T) {
	fakeTmuxServer(t, "refuse")
	if err := ScrubLocalTmuxEnv(context.Background(), ""); err == nil {
		t.Fatal("ScrubLocalTmuxEnv = nil, want the refused unset")
	}
}

// A scrub its deadline cuts short is an error, not "no server": the
// server may still hold what was being removed. The retry's listing
// fails at once on the expired context, which used to read as the
// server being gone, and Build went on.
func TestScrubLocalTmuxEnv_DeadlineIsAnError(t *testing.T) {
	fakeTmuxServer(t, "hang")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := ScrubLocalTmuxEnv(ctx, ""); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ScrubLocalTmuxEnv = %v, want the deadline", err)
	}
}

// The socket goes on tmux's command line: a name ValidTmuxSocket refuses
// is refused before tmux runs (LOOM-202).
func TestScrubLocalTmuxEnv_RefusesABadSocket(t *testing.T) {
	fakeTmuxServer(t, "refuse")
	if err := ScrubLocalTmuxEnv(context.Background(), "-S /tmp/x"); err == nil || !strings.Contains(err.Error(), "socket name") {
		t.Fatalf("ScrubLocalTmuxEnv(bad socket) = %v, want the name refused", err)
	}
}
