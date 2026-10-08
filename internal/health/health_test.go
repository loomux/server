package health_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync/atomic"
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
	runOnce    func(ctx context.Context) error
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
	if e.runOnce != nil {
		if err := e.runOnce(ctx); err != nil {
			return "", err
		}
		return "tmux 3.4", nil
	}
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
		t.Fatalf("status = %q, want unhealthy (nothing works without the database)", res.Status)
	}
	db := res.Components["database"]
	if db.Status != health.StatusUnhealthy {
		t.Fatalf("database status = %q, want unhealthy", db.Status)
	}
	// Anonymous callers get a fixed string, never the driver's error
	// (LOOM-162); the authenticated deep check keeps the detail.
	if db.Error != "unavailable" {
		t.Fatalf("database error = %q, want the fixed %q", db.Error, "unavailable")
	}
	deep := checker.Deep(context.Background())
	if deep.Status != health.StatusUnhealthy {
		t.Errorf("deep status = %q, want unhealthy", deep.Status)
	}
	if e := deep.Components["database"].Error; e == "" || e == "unavailable" {
		t.Errorf("deep database error = %q, want the ping's own error", e)
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
	if e := res.Components["router_model"].Error; e != "unavailable" {
		t.Fatalf("router_model error = %q, want the fixed %q", e, "unavailable")
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

func createTargets(t *testing.T, store *sqlite.Store, n int) {
	t.Helper()
	for i := range n {
		id := fmt.Sprintf("t%d", i)
		if err := store.CreateTarget(context.Background(), &registry.Target{ID: id, Name: id, Kind: registry.TargetKindLocal}); err != nil {
			t.Fatalf("CreateTarget: %v", err)
		}
	}
}

// The deep check probes its targets at once (LOOM-162): here each probe
// answers only once every one of them has started, which a one-by-one
// sweep would never reach.
func TestDeep_ProbesTargetsInParallel(t *testing.T) {
	defer health.SetDeepTimeout(5 * time.Second)()
	store := newTestStore(t)
	const n = 5
	createTargets(t, store, n)
	var started atomic.Int32
	all := make(chan struct{})
	factory := func(*registry.Target) (targets.TargetExecutor, error) {
		return &fakeExecutor{runOnce: func(ctx context.Context) error {
			if started.Add(1) == n {
				close(all)
			}
			select {
			case <-all:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}}, nil
	}
	res := health.NewChecker(store, factory, routerConfig(), "").Deep(context.Background())
	if res.Components["targets"].Status != health.StatusHealthy {
		t.Fatalf("targets = %+v, want all healthy (probed together)", res.Components["targets"])
	}
}

// A probe that hangs, ignoring its context, doesn't hold the deep check
// past its one deadline: that target is reported timed out, the others
// as they are.
func TestDeep_OneDeadlineForAllProbes(t *testing.T) {
	defer health.SetDeepTimeout(200 * time.Millisecond)()
	store := newTestStore(t)
	createTargets(t, store, 3)
	hang := make(chan struct{})
	defer close(hang)
	factory := func(tg *registry.Target) (targets.TargetExecutor, error) {
		if tg.ID == "t1" {
			return &fakeExecutor{runOnce: func(context.Context) error { <-hang; return nil }}, nil
		}
		return &fakeExecutor{}, nil
	}
	start := time.Now()
	res := health.NewChecker(store, factory, routerConfig(), "").Deep(context.Background())
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("Deep took %v with a 200ms deadline", took)
	}
	if res.Status != health.StatusDegraded {
		t.Errorf("status = %q, want degraded", res.Status)
	}
	raw, _ := json.Marshal(res.Components["targets"].Detail)
	var details []struct{ ID, Status, Error string }
	if err := json.Unmarshal(raw, &details); err != nil {
		t.Fatal(err)
	}
	if len(details) != 3 {
		t.Fatalf("details = %+v, want 3", details)
	}
	for _, d := range details {
		want := health.StatusHealthy
		if d.ID == "t1" {
			want = health.StatusUnhealthy
		}
		if d.Status != want {
			t.Errorf("target %s: status %q, want %q", d.ID, d.Status, want)
		}
		if d.ID == "t1" && d.Error != "timed out" {
			t.Errorf("hung target's error = %q, want timed out", d.Error)
		}
	}
}
