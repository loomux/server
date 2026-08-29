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
