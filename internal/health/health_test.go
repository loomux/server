package health_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

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
	if res.Status != health.StatusDegraded {
		t.Fatalf("status = %q, want degraded", res.Status)
	}
	if res.Components["database"].Status != health.StatusUnhealthy {
		t.Fatalf("database status = %q, want unhealthy", res.Components["database"].Status)
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
