package plugins_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Loomux/server/plugins"
	"github.com/Loomux/server/plugins/fake"
	"github.com/Loomux/server/plugins/protocol"
	"github.com/Loomux/server/registry"
)

// Regression tests for the findings of #343's independent review.

// livePIDs lists the pids whose files are in dir and that are still
// alive.
func livePIDs(t *testing.T, dir string) []int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var alive []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if syscall.Kill(pid, 0) == nil {
			alive = append(alive, pid)
		}
	}
	return alive
}

// Blocker: four parallel Enables start one process, and none survives
// Close.
func TestManagerParallelEnableDoesNotLeak(t *testing.T) {
	m, _, _ := newManager(t)
	pidDir := t.TempDir()
	v := install(t, m, "race", map[string]any{"pid_dir": pidDir})
	if _, err := m.Disable(context.Background(), v.ID); err != nil {
		t.Fatal(err)
	}
	if n := len(livePIDs(t, pidDir)); n != 0 {
		t.Fatalf("%d process(es) alive after Disable", n)
	}
	before, _ := os.ReadDir(pidDir)

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := m.Enable(context.Background(), v.ID); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	after, _ := os.ReadDir(pidDir)
	if started := len(after) - len(before); started != 1 {
		t.Errorf("4 parallel Enables started %d process(es), want 1", started)
	}
	if alive := livePIDs(t, pidDir); len(alive) != 1 {
		t.Errorf("%d process(es) alive after the Enables, want 1", len(alive))
	}
	if err := m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for len(livePIDs(t, pidDir)) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%d process(es) alive after Close", len(livePIDs(t, pidDir)))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The same race between SetConfig, Check and Enable.
func TestManagerParallelMixedLifecycleDoesNotLeak(t *testing.T) {
	m, _, _ := newManager(t)
	pidDir := t.TempDir()
	v := install(t, m, "mixed", map[string]any{"pid_dir": pidDir})
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(3)
		go func() { defer wg.Done(); _, _ = m.Enable(context.Background(), v.ID) }()
		go func() {
			defer wg.Done()
			_, _ = m.SetConfig(context.Background(), v.ID, map[string]any{"pid_dir": pidDir, "greeting": "x"})
		}()
		go func() { defer wg.Done(); _, _ = m.Check(context.Background(), v.ID) }()
	}
	wg.Wait()
	if alive := livePIDs(t, pidDir); len(alive) != 1 {
		t.Errorf("%d process(es) alive after mixed calls, want 1", len(alive))
	}
	_ = m.Close(context.Background())
	deadline := time.Now().Add(3 * time.Second)
	for len(livePIDs(t, pidDir)) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%d process(es) alive after Close", len(livePIDs(t, pidDir)))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Should-fix 3: a plugin that echoes its token into its check and its
// stderr never gets it into a view, a row or the log.
func TestManagerScrubsSecretsFromPluginText(t *testing.T) {
	const token = "tok-ThisIsASecretValue-1234567890"
	bundle := t.TempDir()
	fakeDir(t, bundle)
	store := newStore(t, filepath.Join(t.TempDir(), "m.db"))
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	m := plugins.NewManager(store, &plugins.Catalog{BundleDir: bundle}, protocol.HostInfo{Version: "test", InstanceID: "inst"}, logger, nil)
	m.InstanceOptions = func(o *plugins.InstanceOptions) {
		o.RestartBackoff = 10 * time.Millisecond
		o.StopTimeout = 500 * time.Millisecond
	}
	t.Cleanup(func() { _ = m.Close(context.Background()) })

	v, err := m.Install(context.Background(), plugins.InstallRequest{Name: "fake", Source: registry.PluginSourceBundled, Label: "leaky",
		Config: map[string]any{"mode": fake.ModeLeakToken, "token": token}})
	var cf *plugins.CheckFailedError
	if !errors.As(err, &cf) {
		t.Fatalf("want *CheckFailedError, got %v", err)
	}
	if strings.Contains(cf.Reason, token) {
		t.Errorf("the error's reason carries the token: %q", cf.Reason)
	}
	for _, p := range cf.Problems {
		if strings.Contains(p.Message, token) {
			t.Errorf("a problem carries the token: %q", p.Message)
		}
	}
	if !strings.Contains(cf.Reason, "[redacted]") {
		t.Errorf("the reason should show where the token was: %q", cf.Reason)
	}
	b, _ := json.Marshal(v)
	if strings.Contains(string(b), token) {
		t.Errorf("the view carries the token: %s", b)
	}
	row, _ := store.GetPlugin(context.Background(), v.ID)
	if strings.Contains(row.StatusReason, token) {
		t.Errorf("the row's status_reason carries the token: %q", row.StatusReason)
	}
	// stderr went to the log, after redaction; give the reader a moment.
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(logBuf.String(), "plugin stderr") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if strings.Contains(logBuf.String(), token) {
		t.Errorf("the log carries the token:\n%s", logBuf.String())
	}
	if !strings.Contains(logBuf.String(), "[redacted]") {
		t.Errorf("expected a redacted stderr line in the log:\n%s", logBuf.String())
	}
}

// Should-fix 4: a supervisor that gives up marks the row error.
func TestManagerSupervisorGivingUpMarksRow(t *testing.T) {
	m, store, _ := newManager(t)
	v := install(t, m, "dying", map[string]any{"mode": fake.ModeExitAfterConfigure})
	deadline := time.Now().Add(10 * time.Second)
	for {
		row, err := store.GetPlugin(context.Background(), v.ID)
		if err != nil {
			t.Fatal(err)
		}
		if row.Status == registry.PluginStatusError {
			if !strings.Contains(row.StatusReason, "stopped") {
				t.Errorf("status_reason = %q", row.StatusReason)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("row still %s after the supervisor gave up", row.Status)
		}
		time.Sleep(20 * time.Millisecond)
	}
	got, err := m.Get(context.Background(), v.ID)
	if err != nil || got.Instance.State != plugins.StateFailed {
		t.Errorf("view = %+v, %v", got, err)
	}
}

// Should-fix 5: a manual Check while the supervisor is restarting the
// plugin answers unavailable and leaves the row alone.
func TestManagerCheckDuringRestartLeavesRow(t *testing.T) {
	m, store, _ := newManager(t)
	m.InstanceOptions = func(o *plugins.InstanceOptions) {
		o.RestartBackoff = 2 * time.Second // long enough to look inside
		o.StopTimeout = 500 * time.Millisecond
	}
	v := install(t, m, "flaky", map[string]any{"mode": fake.ModeExitAfterConfigure})
	deadline := time.Now().Add(5 * time.Second)
	for {
		if got, _ := m.Get(context.Background(), v.ID); got != nil && got.Instance.State == plugins.StateRestarting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the instance never went to restarting")
		}
		time.Sleep(10 * time.Millisecond)
	}
	got, err := m.Check(context.Background(), v.ID)
	if !errors.Is(err, plugins.ErrUnavailable) {
		t.Fatalf("Check during a restart: want ErrUnavailable, got %v", err)
	}
	if got.Status != registry.PluginStatusInstalled || got.Instance.State != plugins.StateRestarting {
		t.Errorf("view after the check = %+v", got)
	}
	row, _ := store.GetPlugin(context.Background(), v.ID)
	if row.Status != registry.PluginStatusInstalled {
		t.Errorf("row after a check during a restart = %s %q", row.Status, row.StatusReason)
	}
}
