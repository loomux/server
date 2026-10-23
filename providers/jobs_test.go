package providers_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
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

// deadlineStore holds a job's environment write until the job's
// deadline passes, then fails it with the context's error, as sqlite
// does: the deadline lands inside the write.
type deadlineStore struct{ registry.Store }

func (s deadlineStore) UpdateEnvironment(ctx context.Context, e *registry.Environment) error {
	if _, ok := ctx.Deadline(); ok && e.Status != registry.EnvironmentError {
		<-ctx.Done()
		return fmt.Errorf("sqlite: update environment: %w", ctx.Err())
	}
	return s.Store.UpdateEnvironment(ctx, e)
}

// A deadline that passes during a write is still the deadline: the
// reason says the machine wasn't ready in time, not that the write
// failed (seen on TEST 2026-10-20, a cold image pull past 10 minutes).
func TestDeadlineDuringWriteRecordsTheDeadline(t *testing.T) {
	h := slowHarness(t)
	m := providers.NewManager(deadlineStore{h.store}, h.pm, "inst", nil, nil)
	defer m.Close()
	m.PollInterval = 20 * time.Millisecond
	m.CreateTimeout = 300 * time.Millisecond
	p := true
	target, _, err := m.Create(context.Background(), providers.CreateRequest{
		Target: registry.Target{Name: "late-write"}, PluginID: h.pluginID, Size: "small", Persistent: &p,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	m.WaitJobsForTest()
	e, err := m.Get(context.Background(), target.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if e.Status != registry.EnvironmentError || !strings.Contains(e.StatusReason, "didn't become ready within 300ms") {
		t.Errorf("after the deadline = %s (%q), want error naming the deadline", e.Status, e.StatusReason)
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

// readDeadlineStore holds one read of a starting machine (follow's, the
// first after the create applied) until the job's deadline passes, then
// fails it with the context's error, as sqlite does.
type readDeadlineStore struct {
	registry.Store
	held *atomic.Bool
}

func (s readDeadlineStore) GetEnvironment(ctx context.Context, id string) (*registry.Environment, error) {
	env, err := s.Store.GetEnvironment(ctx, id)
	if err == nil && env.Status == registry.EnvironmentStarting && s.held.CompareAndSwap(false, true) {
		<-ctx.Done()
		return nil, fmt.Errorf("sqlite: get environment: %w", ctx.Err())
	}
	return env, err
}

// A deadline that passes during follow's read of the machine is still
// the deadline: the machine is failed, not left starting for ever
// (LOOM-203; seen as a flake under CI load).
func TestDeadlineDuringReadRecordsTheDeadline(t *testing.T) {
	h := newHarness(t, map[string]any{"targets_mode": "stuck"})
	m := providers.NewManager(readDeadlineStore{h.store, new(atomic.Bool)}, h.pm, "inst", nil, nil)
	defer m.Close()
	m.PollInterval = 20 * time.Millisecond
	m.CreateTimeout = 300 * time.Millisecond
	p := true
	target, _, err := m.Create(context.Background(), providers.CreateRequest{
		Target: registry.Target{Name: "late-read"}, PluginID: h.pluginID, Size: "small", Persistent: &p,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	m.WaitJobsForTest()
	e, err := m.Get(context.Background(), target.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if e.Status != registry.EnvironmentError || !strings.Contains(e.StatusReason, "didn't become ready within 300ms") {
		t.Errorf("after the deadline = %s (%q), want error naming the deadline", e.Status, e.StatusReason)
	}
}
