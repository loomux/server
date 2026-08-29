package completion_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Loomux/server/completion"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/registry/sqlite"
	"github.com/Loomux/server/targets"
)

func newTestStore(t *testing.T) registry.Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	store, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return store
}

func createFixtureWorkspace(t *testing.T, store registry.Store, targetKind registry.TargetKind) *registry.Workspace {
	t.Helper()
	ctx := context.Background()

	target := &registry.Target{
		ID:   uuid.NewString(),
		Name: "fixture-target-" + t.Name(),
		Kind: targetKind,
	}
	if targetKind == registry.TargetKindRemote {
		target.Host = "example.invalid"
		target.User = "loomux-test"
	}
	if err := store.CreateTarget(ctx, target); err != nil {
		t.Fatalf("fixture CreateTarget: %v", err)
	}

	ws := &registry.Workspace{
		ID:       uuid.NewString(),
		Name:     "fixture-ws-" + t.Name(),
		Path:     "/fixture",
		TargetID: target.ID,
		Status:   registry.WorkspaceStatusIdle,
	}
	if err := store.CreateWorkspace(ctx, ws); err != nil {
		t.Fatalf("fixture CreateWorkspace: %v", err)
	}
	return ws
}

// failingFactory returns an ExecutorFactory that fails the test if
// invoked — used to strictly prove a marker-tier Wait never touches the
// idle-heuristic path at all.
func failingFactory(t *testing.T) func(*registry.Target) (targets.TargetExecutor, error) {
	return func(*registry.Target) (targets.TargetExecutor, error) {
		t.Fatalf("executor factory was called; expected marker-tier detection to never need one")
		return nil, nil
	}
}

func TestDetector_LocalMarkerConfiguredTaskUsesMarkerTier(t *testing.T) {
	store := newTestStore(t)
	ws := createFixtureWorkspace(t, store, registry.TargetKindLocal)
	task := &registry.Task{ID: uuid.NewString(), WorkspaceID: ws.ID, AgentType: "claude-code", TmuxSession: "sess"}

	markerDir := t.TempDir()
	cfg := completion.Config{"claude-code": {Tier: completion.TierMarker}}
	d := completion.NewDetector(store, failingFactory(t), cfg, markerDir)

	errCh := make(chan error, 1)
	go func() { errCh <- d.Wait(context.Background(), task) }()

	// Let Wait start polling for the marker before it exists.
	time.Sleep(50 * time.Millisecond)
	markerPath := filepath.Join(markerDir, task.ID+".done")
	if err := os.WriteFile(markerPath, []byte("done"), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Wait: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Wait did not return after the marker appeared")
	}
}

func TestDetector_RemoteMarkerConfiguredTaskFallsBackToIdle(t *testing.T) {
	store := newTestStore(t)
	ws := createFixtureWorkspace(t, store, registry.TargetKindRemote)
	task := &registry.Task{ID: uuid.NewString(), WorkspaceID: ws.ID, AgentType: "claude-code", TmuxSession: "sess"}

	exec := newScriptedExecutor([]string{"same output", "same output", "same output"})
	factory := func(*registry.Target) (targets.TargetExecutor, error) { return exec, nil }

	// A marker-configured agent-type, but the target is remote — marker
	// detection can't observe a remote host's filesystem, so this must
	// fall back to idle. A tiny IdleTimeout keeps this test fast.
	cfg := completion.Config{"claude-code": {Tier: completion.TierMarker, IdleTimeout: 10 * time.Millisecond}}
	d := completion.NewDetector(store, factory, cfg, t.TempDir())

	if err := d.Wait(context.Background(), task); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if exec.callCount() == 0 {
		t.Fatalf("expected idle-tier CapturePane calls (fallback from marker on a remote target), got none")
	}
}

func TestDetector_UnconfiguredAgentTypeFallsBackToIdle(t *testing.T) {
	store := newTestStore(t)
	ws := createFixtureWorkspace(t, store, registry.TargetKindLocal)
	// AgentType "" is what every shell-kind task has; also stands in
	// here for "no config entry at all" generally.
	task := &registry.Task{ID: uuid.NewString(), WorkspaceID: ws.ID, AgentType: "", TmuxSession: "sess"}

	exec := newScriptedExecutor([]string{"a", "b", "c", "d", "e", "f", "g", "h"})
	factory := func(*registry.Target) (targets.TargetExecutor, error) { return exec, nil }

	d := completion.NewDetector(store, factory, completion.Config{}, t.TempDir())

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- d.Wait(ctx, task) }()

	deadline := time.Now().Add(2 * time.Second)
	for exec.callCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if exec.callCount() == 0 {
		t.Fatalf("expected idle-tier CapturePane calls, got none")
	}

	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Wait: err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Wait did not return after ctx was cancelled")
	}
}
