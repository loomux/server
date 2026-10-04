package targets

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSSH puts an "ssh" on PATH running script, for tests of what
// RemoteExecutor does around ssh rather than over it.
func fakeSSH(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

// LOOM-84: a target that never answers fails the operation as
// unreachable once its deadline passes, instead of hanging.
func TestRemoteExecutor_OpDeadline(t *testing.T) {
	fakeSSH(t, "exec sleep 30\n")
	e := NewRemoteExecutor("blackhole-"+t.Name(), "u", WithOpTimeout(300*time.Millisecond))
	start := time.Now()
	_, err := e.HasSession(context.Background(), "s")
	if !errors.Is(err, ErrUnreachable) {
		t.Errorf("HasSession = %v, want ErrUnreachable", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("took %s, want it bounded by the 300ms deadline", time.Since(start))
	}
}

// The caller cancelling is not reported as an unreachable target.
func TestRemoteExecutor_CallerCancelIsNotUnreachable(t *testing.T) {
	fakeSSH(t, "exec sleep 30\n")
	e := NewRemoteExecutor("cancel-"+t.Name(), "u", WithOpTimeout(time.Minute))
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := e.CapturePane(ctx, "s"); errors.Is(err, ErrUnreachable) {
		t.Errorf("CapturePane = %v, want the caller's own cancellation, not ErrUnreachable", err)
	}
}

// At most maxConcurrentOps ssh operations run at once against one
// target, however many callers there are, and every executor for that
// target shares the limit.
func TestRemoteExecutor_ConcurrencyCap(t *testing.T) {
	dir := fakeSSH(t, `d=$(dirname "$0")
touch "$d/run.$$"
sleep 0.3
ls "$d" | grep -c '^run\.' >> "$d/seen"
rm -f "$d/run.$$"
`)
	var wg sync.WaitGroup
	for i := 0; i < 3*maxConcurrentOps; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e := NewRemoteExecutor("busy-"+t.Name(), "u")
			_, _ = e.CapturePane(context.Background(), "s")
		}()
	}
	wg.Wait()
	data, err := os.ReadFile(filepath.Join(dir, "seen"))
	if err != nil {
		t.Fatal(err)
	}
	peak := 0
	for _, f := range strings.Fields(string(data)) {
		n, _ := strconv.Atoi(f)
		peak = max(peak, n)
	}
	if peak == 0 || peak > maxConcurrentOps {
		t.Errorf("peak concurrent ssh operations = %d, want 1..%d", peak, maxConcurrentOps)
	}
}

func TestRemoteExecutor_KeepalivesSet(t *testing.T) {
	args := strings.Join(NewRemoteExecutor("h", "u").baseArgs(), " ")
	for _, want := range []string{"ServerAliveInterval=15", "ServerAliveCountMax=3"} {
		if !strings.Contains(args, want) {
			t.Errorf("ssh args %q missing %s", args, want)
		}
	}
}
