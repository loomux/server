package providers_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Loomux/server/plugins"
	"github.com/Loomux/server/plugins/protocol"
	"github.com/Loomux/server/providers"
	"github.com/Loomux/server/registry"
)

// The background jobs (#344 review): a job cancelled on purpose leaves
// the row as the canceller set it; only the deadline records an error;
// a replaced job never removes the newer job's entry; a job's result is
// dropped once the machine moved on.

// slowHarness is one over the fake in slow_start (three polls to
// running) with a poll interval long enough that a machine stays
// starting while the test acts on it.
func slowHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t, map[string]any{"targets_mode": "slow_start"})
	h.m.PollInterval = 400 * time.Millisecond
	return h
}

func TestStopWhileStartingStaysStopped(t *testing.T) {
	h := slowHarness(t)
	ctx := context.Background()
	target, _ := h.create(t, "slow", true)
	h.waitStatus(t, target.ID, registry.EnvironmentStarting)
	if err := h.m.Stop(ctx, target.ID); err != nil {
		t.Fatalf("Stop while starting: %v", err)
	}
	h.m.WaitJobsForTest()
	if e, _ := h.m.Get(ctx, target.ID); e.Status != registry.EnvironmentStopped {
		t.Fatalf("after Stop and the job's end = %s (%s)", e.Status, e.StatusReason)
	}
	if err := h.m.Start(ctx, target.ID); err != nil {
		t.Fatalf("Start after: %v", err)
	}
	h.waitStatus(t, target.ID, registry.EnvironmentRunning)
}

func TestUninstallKeepWhileStartingDetaches(t *testing.T) {
	h := slowHarness(t)
	ctx := context.Background()
	target, _ := h.create(t, "kept", true)
	h.waitStatus(t, target.ID, registry.EnvironmentStarting)
	if err := h.pm.Uninstall(ctx, h.pluginID, plugins.UninstallKeep); err != nil {
		t.Fatalf("uninstall keep while starting: %v", err)
	}
	h.m.WaitJobsForTest()
	e, _ := h.m.Get(ctx, target.ID)
	if e.Status != registry.EnvironmentDetached || e.PluginID != "" {
		t.Errorf("after uninstall keep and the job's end = %s (%s), plugin %q", e.Status, e.StatusReason, e.PluginID)
	}
}

func TestCloseWhileStartingLeavesItToReconcile(t *testing.T) {
	h := slowHarness(t)
	ctx := context.Background()
	target, _ := h.create(t, "shutdown", true)
	h.waitStatus(t, target.ID, registry.EnvironmentStarting)
	h.m.Close()
	e, _ := h.m.Get(ctx, target.ID)
	if e.Status != registry.EnvironmentStarting || e.StatusReason != "" {
		t.Fatalf("after Close = %s (%s), want starting with no reason", e.Status, e.StatusReason)
	}
	// The next server's reconcile takes the plugin's word and the
	// machine comes up.
	m2 := providers.NewManager(h.store, h.pm, "inst", nil, nil)
	defer m2.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		m2.Reconcile(ctx)
		e, _ = m2.Get(ctx, target.ID)
		if e.Status == registry.EnvironmentRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after Close and reconcile = %s (%s), never running", e.Status, e.StatusReason)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestDeadlineRecordsError(t *testing.T) {
	h := slowHarness(t)
	h.m.CreateTimeout = 300 * time.Millisecond
	target, _ := h.create(t, "late", true)
	e := h.waitStatus(t, target.ID, registry.EnvironmentError)
	if !strings.Contains(e.StatusReason, "didn't become ready within 300ms") {
		t.Errorf("reason = %q", e.StatusReason)
	}
}

func TestReplacedJobKeepsTheNewerEntry(t *testing.T) {
	h := newHarness(t, nil)
	const id = "job-env"
	aDone := make(chan struct{})
	h.m.StartJobForTest(id, func(ctx context.Context) { <-ctx.Done(); close(aDone) })
	bCancelled := make(chan struct{})
	h.m.StartJobForTest(id, func(ctx context.Context) { <-ctx.Done(); close(bCancelled) })
	select {
	case <-aDone:
	case <-time.After(2 * time.Second):
		t.Fatal("the replaced job wasn't cancelled")
	}
	// The replaced job's cleanup runs right after; the newer job's
	// entry must survive it.
	until := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(until) {
		if !h.m.FollowingForTest(id) {
			t.Fatal("the replaced job's cleanup removed the newer job's entry")
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.m.CancelJobForTest(id)
	select {
	case <-bCancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelJob didn't reach the newer job")
	}
	h.m.WaitJobsForTest()
	if h.m.FollowingForTest(id) {
		t.Error("an entry is left after the job ended")
	}
}

func TestJobResultDroppedOnceStopped(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	target, env := h.create(t, "race", true)
	h.waitStatus(t, target.ID, registry.EnvironmentRunning)
	if err := h.m.Stop(ctx, target.ID); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// What a job saw before the Stop must not be written over it.
	err := h.m.ApplyLockedForTest(ctx, env.ID, protocol.Environment{
		ID: env.ID, Status: protocol.EnvRunning,
		Address: protocol.Address{Host: target.Host, Port: target.SSHPort, Proxy: protocol.SSHProxyNone},
	})
	if !errors.Is(err, providers.ErrStaleForTest) {
		t.Fatalf("apply after Stop: %v, want the stale signal", err)
	}
	if e, _ := h.m.Get(ctx, target.ID); e.Status != registry.EnvironmentStopped {
		t.Errorf("after a stale apply = %s", e.Status)
	}
}
