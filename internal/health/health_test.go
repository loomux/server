package health_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/Loomux/server/internal/health"
	"github.com/Loomux/server/orchestrator"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/registry/sqlite"
	"github.com/Loomux/server/router/llmrouter"
	"github.com/Loomux/server/targets"
)

type fakeExecutor struct {
	runOnceErr error
}

func (e *fakeExecutor) NewSession(ctx context.Context, session, dir, command string) error {
	return nil
}
func (e *fakeExecutor) HasSession(ctx context.Context, session string) (bool, error) {
	return false, nil
}
func (e *fakeExecutor) SendKey(ctx context.Context, target, key string) error { return nil }

func (e *fakeExecutor) PasteText(ctx context.Context, target, text string, enter bool) error {
	return nil
}
func (e *fakeExecutor) SendKeys(ctx context.Context, target, keys string, enter bool) error {
	return nil
}
func (e *fakeExecutor) CapturePane(ctx context.Context, target string) (string, error) {
	return "", nil
}
func (e *fakeExecutor) KillSession(ctx context.Context, session string) error     { return nil }
func (e *fakeExecutor) Close() error                                              { return nil }
func (e *fakeExecutor) FileExists(ctx context.Context, path string) (bool, error) { return false, nil }
func (e *fakeExecutor) RemoveFile(ctx context.Context, path string) error         { return nil }
func (e *fakeExecutor) RunOnce(ctx context.Context, command string) (string, error) {
	if e.runOnceErr != nil {
		return "", e.runOnceErr
	}
	return "tmux 3.4", nil
}

func (e *fakeExecutor) PaneExited(ctx context.Context, target string) (*targets.PaneExit, error) {
	return nil, nil
}

func newFakeExecutorFactory(healthy bool) orchestrator.ExecutorFactory {
	return func(t *registry.Target) (targets.TargetExecutor, error) {
		if !healthy {
			return &fakeExecutor{runOnceErr: errors.New("tmux not found")}, nil
		}
		return &fakeExecutor{}, nil
	}
}

func newTestStore(t *testing.T) *sqlite.Store {
	t.Helper()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})
	return store
}

func routerConfig() llmrouter.Config {
	return llmrouter.Config{Primary: llmrouter.Tier{BaseURL: "http://router", Model: "m", APIKey: "k"}}
}

func TestShallow_Healthy(t *testing.T) {
	store := newTestStore(t)
	checker := health.NewChecker(store, newFakeExecutorFactory(true), routerConfig(), "")

	res := checker.Shallow(context.Background())
	if res.Status != health.StatusHealthy {
		t.Fatalf("status = %q, want healthy", res.Status)
	}
	if res.Components["database"].Status != health.StatusHealthy {
		t.Fatalf("database status = %q, want healthy", res.Components["database"].Status)
	}
	if res.Components["router_model"].Status != health.StatusHealthy {
		t.Fatalf("router_model status = %q, want healthy", res.Components["router_model"].Status)
	}
}

func TestShallow_DBFailure(t *testing.T) {
	store := newTestStore(t)
	_ = store.Close() // close the DB so Ping fails
	checker := health.NewChecker(store, newFakeExecutorFactory(true), routerConfig(), "")

	res := checker.Shallow(context.Background())
	if res.Status != health.StatusUnhealthy {
		t.Fatalf("status = %q, want unhealthy: nothing works without the database", res.Status)
	}
	db := res.Components["database"]
	if db.Status != health.StatusUnhealthy {
		t.Fatalf("database status = %q, want unhealthy", db.Status)
	}
	// LOOM-162: an anonymous caller gets a fixed string, not the driver's
	// error text.
	if db.Error != health.ErrTextUnavailable {
		t.Errorf("database error = %q, want the fixed %q", db.Error, health.ErrTextUnavailable)
	}
}

func TestDeep_DBFailureKeepsDetail(t *testing.T) {
	store := newTestStore(t)
	_ = store.Close()
	checker := health.NewChecker(store, newFakeExecutorFactory(true), routerConfig(), "")

	res := checker.Deep(context.Background())
	if res.Status != health.StatusUnhealthy {
		t.Fatalf("status = %q, want unhealthy", res.Status)
	}
	if e := res.Components["database"].Error; e == "" || e == health.ErrTextUnavailable {
		t.Errorf("deep database error = %q, want the real cause (deep is authenticated)", e)
	}
}

func TestShallow_RouterNotConfigured(t *testing.T) {
	store := newTestStore(t)
	checker := health.NewChecker(store, newFakeExecutorFactory(true), llmrouter.Config{}, "")

	res := checker.Shallow(context.Background())
	if res.Status != health.StatusDegraded {
		t.Fatalf("status = %q, want degraded", res.Status)
	}
	if res.Components["router_model"].Status != health.StatusUnhealthy {
		t.Fatalf("router_model status = %q, want unhealthy", res.Components["router_model"].Status)
	}
	if e := res.Components["router_model"].Error; e != health.ErrTextNotConfigured {
		t.Errorf("router_model error = %q, want the fixed %q", e, health.ErrTextNotConfigured)
	}
}

func TestDeep_TargetReachable(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	if err := store.CreateTarget(ctx, &registry.Target{ID: "t1", Name: "local", Kind: registry.TargetKindLocal}); err != nil {
		t.Fatalf("CreateTarget: %v", err)
	}
	checker := health.NewChecker(store, newFakeExecutorFactory(true), routerConfig(), "")

	res := checker.Deep(ctx)
	if res.Status != health.StatusHealthy {
		t.Fatalf("status = %q, want healthy", res.Status)
	}
	targets := res.Components["targets"]
	if targets.Status != health.StatusHealthy {
		t.Fatalf("targets status = %q, want healthy", targets.Status)
	}
}

func TestDeep_TargetUnreachable(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	if err := store.CreateTarget(ctx, &registry.Target{ID: "t1", Name: "local", Kind: registry.TargetKindLocal}); err != nil {
		t.Fatalf("CreateTarget: %v", err)
	}
	checker := health.NewChecker(store, newFakeExecutorFactory(false), routerConfig(), "")

	res := checker.Deep(ctx)
	if res.Status != health.StatusDegraded {
		t.Fatalf("status = %q, want degraded", res.Status)
	}
	targets := res.Components["targets"]
	if targets.Status != health.StatusDegraded {
		t.Fatalf("targets status = %q, want degraded", targets.Status)
	}
}

func TestDeep_SidecarUnreachable(t *testing.T) {
	store := newTestStore(t)
	checker := health.NewChecker(store, newFakeExecutorFactory(true), routerConfig(), "127.0.0.1:1")

	res := checker.Deep(context.Background())
	if res.Status != health.StatusDegraded {
		t.Fatalf("status = %q, want degraded", res.Status)
	}
	sidecar := res.Components["sidecar_socks5"]
	if sidecar.Status != health.StatusUnhealthy {
		t.Fatalf("sidecar status = %q, want unhealthy", sidecar.Status)
	}
}

// blockingExecutor's RunOnce answers after delay, or fails when ctx ends
// first.
type blockingExecutor struct {
	fakeExecutor
	delay time.Duration
}

func (e *blockingExecutor) RunOnce(ctx context.Context, command string) (string, error) {
	select {
	case <-time.After(e.delay):
		return "tmux 3.4", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func createTargets(t *testing.T, store *sqlite.Store, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("t%d", i)
		if err := store.CreateTarget(context.Background(), &registry.Target{ID: id, Name: id, Kind: registry.TargetKindLocal}); err != nil {
			t.Fatalf("CreateTarget: %v", err)
		}
	}
}

// LOOM-162: targets are probed at once, not one after another.
func TestDeep_ProbesTargetsInParallel(t *testing.T) {
	store := newTestStore(t)
	createTargets(t, store, 5)
	factory := func(*registry.Target) (targets.TargetExecutor, error) {
		return &blockingExecutor{delay: 300 * time.Millisecond}, nil
	}
	checker := health.NewChecker(store, factory, routerConfig(), "", health.WithDeepTimeout(5*time.Second))

	start := time.Now()
	res := checker.Deep(context.Background())
	if took := time.Since(start); took > 1200*time.Millisecond {
		t.Errorf("Deep took %s for 5 targets of 300ms each, want them probed in parallel", took)
	}
	if res.Status != health.StatusHealthy {
		t.Fatalf("status = %q, want healthy: %+v", res.Status, res.Components)
	}
}

// LOOM-162: one deadline bounds the whole check; a target that doesn't
// answer is reported unhealthy, in its place in the list.
func TestDeep_OneDeadline(t *testing.T) {
	store := newTestStore(t)
	createTargets(t, store, 4)
	factory := func(tg *registry.Target) (targets.TargetExecutor, error) {
		if tg.ID == "t2" {
			return &blockingExecutor{delay: time.Hour}, nil
		}
		return &fakeExecutor{}, nil
	}
	checker := health.NewChecker(store, factory, routerConfig(), "", health.WithDeepTimeout(200*time.Millisecond))

	start := time.Now()
	res := checker.Deep(context.Background())
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("Deep took %s with a 200ms deadline", took)
	}
	if res.Status != health.StatusDegraded {
		t.Errorf("status = %q, want degraded", res.Status)
	}
	raw, err := json.Marshal(res.Components["targets"].Detail)
	if err != nil {
		t.Fatal(err)
	}
	var details []struct{ ID, Status, Error string }
	if err := json.Unmarshal(raw, &details); err != nil {
		t.Fatal(err)
	}
	if len(details) != 4 {
		t.Fatalf("details = %+v, want 4 targets", details)
	}
	for i, d := range details {
		if d.ID != fmt.Sprintf("t%d", i) {
			t.Errorf("details[%d] = %s, want the store's order", i, d.ID)
		}
		want := health.StatusHealthy
		if d.ID == "t2" {
			want = health.StatusUnhealthy
		}
		if d.Status != want {
			t.Errorf("target %s status = %q (%s), want %q", d.ID, d.Status, d.Error, want)
		}
	}
}
