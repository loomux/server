// Package executortest is the shared behavioral test suite for
// targets.TargetExecutor. Any implementation should pass Run
// identically — that's what proves local and remote executors are
// actually interchangeable behind the interface.
package executortest

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Loomux/server/targets"
)

// Run executes the full behavioral suite against an executor built fresh
// by newExecutor for each subtest. newExecutor's caller is responsible
// for the executor's own cleanup (e.g. via t.Cleanup).
func Run(t *testing.T, newExecutor func(t *testing.T) targets.TargetExecutor) {
	t.Run("SessionLifecycle", func(t *testing.T) { testSessionLifecycle(t, newExecutor(t)) })
	t.Run("FileExistsLifecycle", func(t *testing.T) { testFileExistsLifecycle(t, newExecutor(t)) })
	t.Run("RunOnce", func(t *testing.T) { testRunOnce(t, newExecutor(t)) })
	t.Run("PaneSurvivesCommandExit", func(t *testing.T) { testPaneSurvivesCommandExit(t, newExecutor(t)) })
	t.Run("NoReparseByOtherShells", func(t *testing.T) { testNoReparseByOtherShells(t, newExecutor(t)) })
	t.Run("SessionIsSized", func(t *testing.T) { testSessionIsSized(t, newExecutor(t)) })
}

// testSessionIsSized checks a new session is created wide and tall
// (LOOM-91), not tmux's detached default of 80x24 that truncates an
// agent's answer.
func testSessionIsSized(t *testing.T, exec targets.TargetExecutor) {
	ctx := context.Background()
	session := uniqueSessionName(t)
	if err := exec.NewSession(ctx, session, "", "sleep 30"); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = exec.KillSession(context.Background(), session) })
	out, err := exec.RunOnce(ctx, "tmux -L "+targets.TmuxSocket+" display-message -p -t "+posixQuote(session)+
		" '#{window_width}x#{window_height}'")
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if got := strings.TrimSpace(out); got != "220x50" {
		t.Errorf("session size = %q, want 220x50", got)
	}
}

// posixQuote single-quotes s for POSIX sh.
func posixQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// testNoReparseByOtherShells covers the LOOM-90 review's latent injection:
// commands are POSIX sh, quoted for POSIX sh, so no other shell may parse
// them on the way — not tmux's default-shell for a session command, not
// the remote user's login shell for anything sent over SSH. Under fish,
// the POSIX quoting of `\'` ends the quoted string early, so this payload
// runs its touch if any fish (or other non-POSIX shell) re-parses it.
func testNoReparseByOtherShells(t *testing.T, exec targets.TargetExecutor) {
	ctx := context.Background()
	var marker, payload, printPayload string
	fresh := func(t *testing.T) {
		marker = filepath.Join(t.TempDir(), "injected")
		payload = `\';touch ` + marker + ` #`
		printPayload = `printf '%s\n' ` + posixQuote(payload)
	}
	assertNotInjected := func(t *testing.T, what string) {
		t.Helper()
		if _, err := os.Stat(marker); err == nil {
			t.Fatalf("%s: the payload's touch ran — a non-POSIX shell re-parsed the command", what)
		}
	}

	t.Run("RunOnce", func(t *testing.T) {
		fresh(t)
		out, err := exec.RunOnce(ctx, printPayload)
		if err != nil {
			t.Fatalf("RunOnce: %v (output %q)", err, out)
		}
		assertNotInjected(t, "RunOnce")
		if strings.TrimSpace(out) != payload {
			t.Errorf("RunOnce output = %q, want the payload literally %q", out, payload)
		}
	})

	t.Run("session command", func(t *testing.T) {
		fresh(t)
		session := uniqueSessionName(t)
		if err := exec.NewSession(ctx, session, "", printPayload); err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		t.Cleanup(func() { _ = exec.KillSession(context.Background(), session) })
		exit := waitExited(t, exec, session)
		assertNotInjected(t, "session command")
		if exit.Status != 0 || !strings.Contains(exit.Output, payload) {
			t.Errorf("exit = %+v, want status 0 and the payload printed literally", exit)
		}
	})

	t.Run("send-keys", func(t *testing.T) {
		fresh(t)
		session := uniqueSessionName(t)
		if err := exec.NewSession(ctx, session, "", "cat"); err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		t.Cleanup(func() { _ = exec.KillSession(context.Background(), session) })
		if err := exec.SendKeys(ctx, session, payload, false); err != nil {
			t.Fatalf("SendKeys: %v", err)
		}
		deadline := time.Now().Add(5 * time.Second)
		var captured string
		for time.Now().Before(deadline) {
			captured, _ = exec.CapturePane(ctx, session)
			captured = strings.ReplaceAll(captured, "\n", "") // a long line wraps in the pane
			if strings.Contains(captured, payload) {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		assertNotInjected(t, "send-keys")
		if !strings.Contains(captured, payload) {
			t.Errorf("pane = %q, want the keys typed literally", captured)
		}
	})
}

func waitExited(t *testing.T, exec targets.TargetExecutor, session string) *targets.PaneExit {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		exit, err := exec.PaneExited(context.Background(), session)
		if err != nil {
			t.Fatalf("PaneExited: %v", err)
		}
		if exit != nil {
			return exit
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the session's command never exited")
	return nil
}

// testPaneSurvivesCommandExit covers LOOM-71: a session whose command
// exits — an agent CLI that isn't installed, a one-shot command that
// finished — must keep its pane (remain-on-exit), so its last output can
// still be captured and its exit status read, instead of the session
// vanishing and every later tmux call failing with "can't find pane".
func testPaneSurvivesCommandExit(t *testing.T, exec targets.TargetExecutor) {
	ctx := context.Background()

	cases := []struct {
		name       string
		command    string
		wantStatus int
		wantOutput string
	}{
		{"missing command", "loomux-no-such-cli-xyz", 127, "loomux-no-such-cli-xyz"},
		{"clean exit", "echo loomux-done-marker", 0, "loomux-done-marker"},
		{"non-zero exit", "echo loomux-failing; exit 3", 3, "loomux-failing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			session := uniqueSessionName(t)
			if err := exec.NewSession(ctx, session, "", "sh -c '"+strings.ReplaceAll(tc.command, "'", `'\''`)+"'"); err != nil {
				t.Fatalf("NewSession: %v", err)
			}
			t.Cleanup(func() { _ = exec.KillSession(context.Background(), session) })

			var exit *targets.PaneExit
			var err error
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				exit, err = exec.PaneExited(ctx, session)
				if err != nil {
					t.Fatalf("PaneExited: %v", err)
				}
				if exit != nil {
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
			if exit == nil {
				t.Fatal("PaneExited never reported the command as exited")
			}
			if exit.Status != tc.wantStatus {
				t.Errorf("exit status = %d, want %d", exit.Status, tc.wantStatus)
			}
			if !strings.Contains(exit.Output, tc.wantOutput) {
				t.Errorf("exit output = %q, want it to contain %q", exit.Output, tc.wantOutput)
			}
			if strings.Contains(exit.Output, "Pane is dead") || strings.HasSuffix(exit.Output, "\n\n") {
				t.Errorf("exit output = %q, want tmux's own dead-pane line and trailing blank lines trimmed", exit.Output)
			}

			alive, err := exec.HasSession(ctx, session)
			if err != nil {
				t.Fatalf("HasSession: %v", err)
			}
			if !alive {
				t.Fatal("session vanished when its command exited; want the pane kept (remain-on-exit)")
			}
			if _, err := exec.CapturePane(ctx, session); err != nil {
				t.Fatalf("CapturePane after exit: %v", err)
			}
		})
	}

	// A pane still running its command (here the default shell) has not
	// exited.
	session := uniqueSessionName(t)
	if err := exec.NewSession(ctx, session, "", ""); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = exec.KillSession(context.Background(), session) })
	exit, err := exec.PaneExited(ctx, session)
	if err != nil {
		t.Fatalf("PaneExited (live pane): %v", err)
	}
	if exit != nil {
		t.Errorf("PaneExited reports a live shell as exited: %+v", exit)
	}
}

// testRunOnce covers RunOnce's contract (design spec §10 axis 3 —
// router.VersionCheck's real primitive): a one-shot, non-interactive
// command whose combined stdout+stderr comes straight back, distinct
// from the tmux-pane-oriented session methods.
func testRunOnce(t *testing.T, exec targets.TargetExecutor) {
	ctx := context.Background()

	out, err := exec.RunOnce(ctx, "echo hello-runonce")
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if !strings.Contains(out, "hello-runonce") {
		t.Fatalf("RunOnce output = %q, want it to contain %q", out, "hello-runonce")
	}

	if _, err := exec.RunOnce(ctx, "exit 1"); err == nil {
		t.Fatal("RunOnce with a failing command: want error, got nil")
	}
}

func testFileExistsLifecycle(t *testing.T, exec targets.TargetExecutor) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "marker-"+uniqueSessionName(t))

	exists, err := exec.FileExists(ctx, path)
	if err != nil {
		t.Fatalf("FileExists (before create): %v", err)
	}
	if exists {
		t.Fatalf("FileExists = true before the file was ever created")
	}

	if err := os.WriteFile(path, []byte("marker"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	exists, err = exec.FileExists(ctx, path)
	if err != nil {
		t.Fatalf("FileExists (after create): %v", err)
	}
	if !exists {
		t.Fatalf("FileExists = false after the file was created")
	}

	if err := exec.RemoveFile(ctx, path); err != nil {
		t.Fatalf("RemoveFile: %v", err)
	}

	exists, err = exec.FileExists(ctx, path)
	if err != nil {
		t.Fatalf("FileExists (after remove): %v", err)
	}
	if exists {
		t.Fatalf("FileExists = true after RemoveFile succeeded")
	}
}

func testSessionLifecycle(t *testing.T, exec targets.TargetExecutor) {
	ctx := context.Background()
	session := uniqueSessionName(t)

	exists, err := exec.HasSession(ctx, session)
	if err != nil {
		t.Fatalf("HasSession (before create): %v", err)
	}
	if exists {
		t.Fatalf("HasSession = true before NewSession was ever called")
	}

	if err := exec.NewSession(ctx, session, "", ""); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() {
		_ = exec.KillSession(context.Background(), session)
	})

	exists, err = exec.HasSession(ctx, session)
	if err != nil {
		t.Fatalf("HasSession (after create): %v", err)
	}
	if !exists {
		t.Fatalf("HasSession = false after NewSession succeeded")
	}

	marker := fmt.Sprintf("executortest-marker-%d", rand.Int())
	if err := exec.SendKeys(ctx, session, "echo "+marker, true); err != nil {
		t.Fatalf("SendKeys: %v", err)
	}

	var captured string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		captured, err = exec.CapturePane(ctx, session)
		if err != nil {
			t.Fatalf("CapturePane: %v", err)
		}
		if strings.Contains(captured, marker) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(captured, marker) {
		t.Fatalf("CapturePane never showed marker %q; last capture:\n%s", marker, captured)
	}

	if err := exec.KillSession(ctx, session); err != nil {
		t.Fatalf("KillSession: %v", err)
	}

	exists, err = exec.HasSession(ctx, session)
	if err != nil {
		t.Fatalf("HasSession (after kill): %v", err)
	}
	if exists {
		t.Fatalf("HasSession = true after KillSession succeeded")
	}
}

func uniqueSessionName(t *testing.T) string {
	sanitized := strings.NewReplacer("/", "-", " ", "-").Replace(t.Name())
	return fmt.Sprintf("loomux-test-%s-%d", sanitized, rand.Int())
}
