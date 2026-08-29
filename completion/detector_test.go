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
	"github.com/Loomux/server/targets/sshtest"
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

// capturePaneForbiddenExecutor wraps a real TargetExecutor but fails the
// test if CapturePane is ever called — used to strictly prove a
// marker-tier Wait never touches the idle-heuristic path. Marker
// detection now genuinely needs a working executor (for FileExists/
// RemoveFile, LOOM-11), so a factory that fails on ANY call (as used
// pre-LOOM-11) no longer works for this — only CapturePane specifically
// must never be reached.
type capturePaneForbiddenExecutor struct {
	targets.TargetExecutor
	t *testing.T
}

func (e *capturePaneForbiddenExecutor) CapturePane(ctx context.Context, target string) (string, error) {
	e.t.Fatalf("CapturePane called; expected marker-tier detection to never need it")
	return "", nil
}

func TestDetector_LocalMarkerConfiguredTaskUsesMarkerTier(t *testing.T) {
	store := newTestStore(t)
	ws := createFixtureWorkspace(t, store, registry.TargetKindLocal)
	task := &registry.Task{ID: uuid.NewString(), WorkspaceID: ws.ID, AgentType: "claude-code", TmuxSession: "sess"}

	markerDir := t.TempDir()
	cfg := completion.Config{"claude-code": {Tier: completion.TierMarker}}
	factory := func(*registry.Target) (targets.TargetExecutor, error) {
		return &capturePaneForbiddenExecutor{TargetExecutor: targets.NewLocalExecutor(), t: t}, nil
	}
	d := completion.NewDetector(store, factory, cfg, markerDir)

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

// TestDetector_RemoteMarkerConfiguredTaskUsesMarkerTier replaces the old
// TestDetector_RemoteMarkerConfiguredTaskFallsBackToIdle now that remote
// marker detection is real (LOOM-11) — this is now a positive test, via
// sshtest (no new SSH test infrastructure), proving TierMarker is
// actually selected and works for a remote target, with
// capturePaneForbiddenExecutor strictly proving the idle path is never
// touched.
func TestDetector_RemoteMarkerConfiguredTaskUsesMarkerTier(t *testing.T) {
	store := newTestStore(t)
	ws := createFixtureWorkspace(t, store, registry.TargetKindRemote)
	task := &registry.Task{ID: uuid.NewString(), WorkspaceID: ws.ID, AgentType: "claude-code", TmuxSession: "sess"}

	server := sshtest.Start(t)
	markerDir := t.TempDir()
	cfg := completion.Config{"claude-code": {Tier: completion.TierMarker}}
	factory := func(*registry.Target) (targets.TargetExecutor, error) {
		real := targets.NewRemoteExecutor(server.Host, "loomux-test",
			targets.WithPort(server.Port),
			targets.WithIdentityFile(server.IdentityFile),
			targets.WithExtraSSHArgs(
				"-o", "StrictHostKeyChecking=no",
				"-o", "UserKnownHostsFile=/dev/null",
			),
		)
		return &capturePaneForbiddenExecutor{TargetExecutor: real, t: t}, nil
	}
	d := completion.NewDetector(store, factory, cfg, markerDir)

	errCh := make(chan error, 1)
	go func() { errCh <- d.Wait(context.Background(), task) }()

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
		t.Fatalf("Wait did not return after the marker appeared (remote)")
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
