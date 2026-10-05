package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/pressly/goose/v3"
)

// TestMigration8PreservesRowsAndForeignKeys runs migrations up to 7,
// writes a target, workspace, task and message the way a deployed
// database already holds them, then opens the store (applying 00008's
// table rebuilds, LOOM-71). Every row must survive, the new 'failed'
// workspace status and 'command' task kind must be accepted, and foreign
// keys must still be enforced afterwards — the rebuild switches them off
// for its own duration only.
func TestMigration8PreservesRowsAndForeignKeys(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "migrate.db")

	db, err := sql.Open("sqlite", dsn(dbPath))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	goose.SetTableName("schema_migrations")
	goose.SetBaseFS(migrationsFS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatalf("SetDialect: %v", err)
	}
	if err := goose.UpTo(db, "migrations", 7); err != nil {
		t.Fatalf("goose.UpTo(7): %v", err)
	}
	for _, stmt := range []string{
		`INSERT INTO targets (id, name, kind, created_at, updated_at) VALUES ('t1', 'local', 'local', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		`INSERT INTO workspaces (id, name, path, target_id, status, created_at, updated_at)
		 VALUES ('w1', 'ws', '/tmp', 't1', 'active', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		`INSERT INTO tasks (id, workspace_id, kind, agent_type, tmux_session, status, conversation_id, created_at, updated_at, reaped_at)
		 VALUES ('k1', 'w1', 'agent', 'codex', 'loomux-x', 'running', 'c1', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		`INSERT INTO messages (id, conversation_id, task_id, role, content, created_at)
		 VALUES ('m1', 'c1', 'k1', 'user', 'hi', CURRENT_TIMESTAMP)`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open (applies 00008): %v", err)
	}
	defer store.Close()

	ws, err := store.GetWorkspace(ctx, "w1")
	if err != nil {
		t.Fatalf("GetWorkspace after migration: %v", err)
	}
	if ws.Status != "active" || ws.TargetID != "t1" {
		t.Errorf("workspace after migration = %+v", ws)
	}
	task, err := store.GetTask(ctx, "k1")
	if err != nil {
		t.Fatalf("GetTask after migration: %v", err)
	}
	if task.AgentType != "codex" || task.ReapedAt == nil || task.Command != "" || task.ExitCode != nil {
		t.Errorf("task after migration = %+v", task)
	}
	msgs, err := store.ListMessagesByConversation(ctx, "c1")
	if err != nil || len(msgs) != 1 || msgs[0].TaskID != "k1" {
		t.Errorf("messages after migration = %v, %v", msgs, err)
	}

	if _, err := store.db.ExecContext(ctx, `UPDATE workspaces SET status = 'failed' WHERE id = 'w1'`); err != nil {
		t.Errorf("status 'failed' rejected after migration: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE tasks SET kind = 'command' WHERE id = 'k1'`); err != nil {
		t.Errorf("kind 'command' rejected after migration: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO workspaces (id, name, path, target_id, status, created_at, updated_at)
		VALUES ('w2', 'orphan', '/tmp', 'no-such-target', 'idle', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`); err == nil {
		t.Error("foreign keys not enforced after migration: orphan workspace accepted")
	}
	var violations int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil {
		t.Fatalf("foreign_key_check: %v", err)
	}
	if violations != 0 {
		t.Errorf("foreign_key_check reports %d violations after migration", violations)
	}
}

// TestMigration17BackfillsMessageOrigin: a message still linked to a task
// gets its target's purpose (” read as personal); one with no task stays
// unknown, since nothing says where it ran.
func TestMigration17BackfillsMessageOrigin(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "migrate.db")

	db, err := sql.Open("sqlite", dsn(dbPath))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	goose.SetTableName("schema_migrations")
	goose.SetBaseFS(migrationsFS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatalf("SetDialect: %v", err)
	}
	if err := goose.UpTo(db, "migrations", 16); err != nil {
		t.Fatalf("goose.UpTo(16): %v", err)
	}
	for _, stmt := range []string{
		`INSERT INTO targets (id, name, kind, purpose, created_at, updated_at) VALUES ('t-home', 'home', 'local', '', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		`INSERT INTO targets (id, name, kind, purpose, created_at, updated_at) VALUES ('t-work', 'sc1', 'local', 'work', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		`INSERT INTO workspaces (id, name, path, target_id, status, created_at, updated_at)
		 VALUES ('w-home', 'home-ws', '/tmp/a', 't-home', 'idle', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		`INSERT INTO workspaces (id, name, path, target_id, status, created_at, updated_at)
		 VALUES ('w-work', 'work-ws', '/tmp/b', 't-work', 'idle', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		`INSERT INTO tasks (id, workspace_id, kind, agent_type, tmux_session, status, conversation_id, created_at, updated_at)
		 VALUES ('k-home', 'w-home', 'agent', 'claude-code', 'loomux-a', 'completed', 'c1', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		`INSERT INTO tasks (id, workspace_id, kind, agent_type, tmux_session, status, conversation_id, created_at, updated_at)
		 VALUES ('k-work', 'w-work', 'agent', 'claude-code', 'loomux-b', 'completed', 'c1', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		`INSERT INTO messages (id, conversation_id, task_id, role, content, created_at) VALUES ('m1', 'c1', 'k-home', 'assistant', 'a', CURRENT_TIMESTAMP)`,
		`INSERT INTO messages (id, conversation_id, task_id, role, content, created_at) VALUES ('m2', 'c1', 'k-work', 'assistant', 'b', CURRENT_TIMESTAMP)`,
		`INSERT INTO messages (id, conversation_id, task_id, role, content, created_at) VALUES ('m3', 'c1', NULL, 'user', 'c', CURRENT_TIMESTAMP)`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open (applies 00017): %v", err)
	}
	defer store.Close()
	msgs, err := store.ListMessagesByConversation(ctx, "c1")
	if err != nil || len(msgs) != 3 {
		t.Fatalf("messages after migration = %v, %v", msgs, err)
	}
	if msgs[0].Origin != "personal" || msgs[1].Origin != "work" || msgs[2].Origin != "" {
		t.Errorf("origins = %q, %q, %q; want personal, work, unknown", msgs[0].Origin, msgs[1].Origin, msgs[2].Origin)
	}
}
