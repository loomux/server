package app

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/registry/sqlite"
	"github.com/Loomux/server/router"
)

// TestBuild_FailsWorkspaceLeftProvisioningAtStartup replays LOOM-60:
// a workspace row stuck in provisioning from an earlier process (a
// restart mid-provisioning, or pre-LOOM-90 code) is marked failed with a
// reason as soon as loomuxd starts — not left provisioning forever, and
// not deleted. A row still within the provisioning bound is untouched.
func TestBuild_FailsWorkspaceLeftProvisioningAtStartup(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	targetID, staleID, freshID := uuid.NewString(), uuid.NewString(), uuid.NewString()

	seed, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("sqlite.Open (seed): %v", err)
	}
	if err := seed.CreateTarget(ctx, &registry.Target{ID: targetID, Name: "t", Kind: registry.TargetKindLocal}); err != nil {
		t.Fatalf("CreateTarget: %v", err)
	}
	for id, name := range map[string]string{staleID: "stale", freshID: "fresh"} {
		if err := seed.CreateWorkspace(ctx, &registry.Workspace{
			ID: id, Name: name, TargetID: targetID, Status: registry.WorkspaceStatusProvisioning, IsDynamic: true,
		}); err != nil {
			t.Fatalf("CreateWorkspace(%s): %v", name, err)
		}
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("Close (seed): %v", err)
	}

	// Backdate the stale row past the bound; the store always stamps
	// created_at itself, so this goes around it.
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	old := time.Now().UTC().Add(-(staleProvisioningAfter + time.Hour))
	if _, err := db.ExecContext(ctx, `UPDATE workspaces SET created_at = ? WHERE id = ?`, old, staleID); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close (backdate): %v", err)
	}

	cfg := testConfig(t, fakeRouterServer(t).URL)
	cfg.DBPath = dbPath
	a, err := build(cfg, router.AgentTypeRegistry{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	defer a.Close()

	deadline := time.After(2 * time.Second)
	for {
		got, err := a.store.GetWorkspace(ctx, staleID)
		if err != nil {
			t.Fatalf("GetWorkspace(stale): %v", err)
		}
		if got.Status == registry.WorkspaceStatusFailed {
			if !strings.Contains(got.StatusReason, "provisioning did not finish") {
				t.Fatalf("StatusReason = %q, want it to say provisioning did not finish", got.StatusReason)
			}
			break
		}
		select {
		case <-deadline:
			t.Fatalf("stale workspace still %q after startup, want failed", got.Status)
		case <-time.After(10 * time.Millisecond):
		}
	}

	fresh, err := a.store.GetWorkspace(ctx, freshID)
	if err != nil {
		t.Fatalf("GetWorkspace(fresh): %v", err)
	}
	if fresh.Status != registry.WorkspaceStatusProvisioning {
		t.Fatalf("fresh workspace Status = %q, want still provisioning (within the bound)", fresh.Status)
	}
}

// The sweep must never race a live provisioning: its threshold is past
// the router's own bound, by which time the router has failed it itself.
func TestStaleProvisioningAfter_ExceedsRouterBound(t *testing.T) {
	if staleProvisioningAfter <= router.ProvisionTimeout {
		t.Fatalf("staleProvisioningAfter = %s, want more than router.ProvisionTimeout (%s)", staleProvisioningAfter, router.ProvisionTimeout)
	}
}
