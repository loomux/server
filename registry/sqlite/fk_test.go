package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/Loomux/server/registry"
)

// TestForeignKeysEnforcedOnAllPooledConnections proves that foreign-key
// enforcement is active on every connection the *sql.DB pool hands out, not
// just whichever single connection happened to receive a PRAGMA after Open.
// The bug behind LOOM-67: setting PRAGMA foreign_keys via db.Exec reaches only
// one pooled connection, so other connections silently accept orphan rows such
// as workspaces referencing non-existent targets.
func TestForeignKeysEnforcedOnAllPooledConnections(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "fk-pool-test.db")

	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	// Force the pool to keep several real SQLite connections open so the test
	// exercises distinct underlying connections rather than repeatedly hitting
	// the same one.
	db := store.db
	db.SetMaxOpenConns(3)
	db.SetMaxIdleConns(3)

	// Reserve three distinct pooled connections. Sequential Conn() calls with
	// MaxOpenConns=3 will each create a new connection until the limit is hit.
	conns := make([]*sql.Conn, 3)
	for i := range conns {
		c, err := db.Conn(ctx)
		if err != nil {
			t.Fatalf("db.Conn #%d: %v", i+1, err)
		}
		defer c.Close()
		conns[i] = c
	}

	// Each connection must reject an insert that violates the workspaces ->
	// targets foreign key. Before the DSN-based pragma fix only the one
	// connection initialized during Open would enforce it; the others would
	// silently create an orphan row.
	badTargetID := uuid.NewString()
	for i, c := range conns {
		wsID := uuid.NewString()
		_, err := c.ExecContext(ctx, `
			INSERT INTO workspaces (
				id, name, path, target_id, git_remote, tags, description, capabilities,
				status, is_dynamic, last_used_at, rolling_summary, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			wsID, wsID, "/tmp", badTargetID, "", "[]", "", "[]",
			string(registry.WorkspaceStatusIdle), false, nil, "", "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z",
		)
		if !isForeignKeyConstraintErr(err) {
			t.Fatalf("connection #%d: expected FOREIGN KEY constraint error, got %v", i+1, err)
		}
	}
}
