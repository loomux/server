package orchestrator_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Loomux/server/orchestrator"
	"github.com/Loomux/server/orchestrator/detectortest"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/registry/sqlite"
	"github.com/Loomux/server/targets"
)

// LOOM-93: the sweep kills loomux- sessions no live task owns once
// they're past the TTL, reports younger ones, and never touches a live
// task's session or anything not named loomux-.
func TestOrphanSweeper(t *testing.T) {
	store, ws, exec, _, orch := setup(t)
	ctx := context.Background()
	old := time.Now().Add(-48 * time.Hour).Unix()
	young := time.Now().Add(-time.Hour).Unix()

	owned := &registry.Task{ID: "t-owned", WorkspaceID: ws.ID, Kind: registry.TaskKindAgent, TmuxSession: "loomux-owned",
		Status: registry.TaskStatusAwaitingInput, ConversationID: "c"}
	failed := &registry.Task{ID: "t-failed", WorkspaceID: ws.ID, Kind: registry.TaskKindAgent, TmuxSession: "loomux-failed",
		Status: registry.TaskStatusFailed, ConversationID: "c"}
	for _, task := range []*registry.Task{owned, failed} {
		if err := store.CreateTask(ctx, task); err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
	}
	sessions := map[string]int64{
		"loomux-owned": old, "loomux-failed": old, "loomux-young": young, "main": old, "claude-work": old,
	}
	list := ""
	for name, created := range sessions {
		_ = exec.NewSession(ctx, name, "", "")
		list += fmt.Sprintf("%s %d\n", name, created)
	}
	exec.runOnceOut = list

	killed := orchestrator.NewOrphanSweeper(orch, 24*time.Hour, nil).Sweep(ctx)
	if killed != 1 {
		t.Errorf("killed %d, want only the old failed task's pane", killed)
	}
	for name := range sessions {
		alive := exec.sessionFor(name).alive
		if want := name != "loomux-failed"; alive != want {
			t.Errorf("session %s alive = %v, want %v", name, alive, want)
		}
	}
}

// An unreachable target is skipped, not fatal.
func TestOrphanSweeper_UnreachableTarget(t *testing.T) {
	_, _, exec, _, orch := setup(t)
	exec.unreachable = true
	if killed := orchestrator.NewOrphanSweeper(orch, time.Hour, nil).Sweep(context.Background()); killed != 0 {
		t.Errorf("killed %d on an unreachable target", killed)
	}
}

// A managed target with no pinned host key yet (LOOM-138: mid-onboarding)
// can't be connected to and has never run a session: the sweep passes it
// by without trying, rather than warning about it every tick.
func TestOrphanSweeper_SkipsUnpinnedManagedTarget(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "sweep.db"), sqlite.WithMasterKey(bytes.Repeat([]byte("k"), 32)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	key := &registry.SSHKey{ID: "k1", Name: "k1", Type: "ssh-ed25519", PublicKey: "ssh-ed25519 AAAA x", Fingerprint: "SHA256:x",
		Origin: registry.SSHKeyOriginTarget, PrivateKey: []byte("p")}
	if err := store.CreateSSHKey(ctx, key); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateTarget(ctx, &registry.Target{ID: "m", Name: "m", Kind: registry.TargetKindRemote, Host: "box.example", User: "u", SSHKeyRef: "k1"}); err != nil {
		t.Fatal(err)
	}
	built := 0
	orch := orchestrator.New(store, func(*registry.Target) (targets.TargetExecutor, error) {
		built++
		return nil, errors.New("must not be called")
	}, detectortest.NewManualDetector())
	var logs bytes.Buffer
	orchestrator.NewOrphanSweeper(orch, time.Hour, slog.New(slog.NewTextHandler(&logs, nil))).Sweep(ctx)
	if built != 0 || strings.Contains(logs.String(), "skipped") {
		t.Errorf("sweep built %d executors for an unpinned managed target; logs: %s", built, logs.String())
	}
}
