package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"

	"github.com/Loomux/server/registry"
)

// LOOM-307: migration 39 leaves every existing target pinning nothing and
// every existing task launched unpinned; down drops the columns and keeps
// the rows. UpTo(38) is the schema before it, whether or not a 00038
// exists yet (draft PR #558 adds one).
func TestMigration39AgentArgs(t *testing.T) {
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
	if err := goose.UpTo(db, "migrations", 38); err != nil {
		t.Fatalf("goose.UpTo(38): %v", err)
	}
	for _, stmt := range []string{
		`INSERT INTO targets (id, name, kind, created_at, updated_at) VALUES ('t1', 'local', 'local', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		`INSERT INTO workspaces (id, name, path, target_id, status, created_at, updated_at)
		 VALUES ('w1', 'ws', '/tmp', 't1', 'active', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		`INSERT INTO tasks (id, workspace_id, kind, agent_type, tmux_session, status, conversation_id, created_at, updated_at)
		 VALUES ('k1', 'w1', 'agent', 'claude-code', 'loomux-x', 'running', 'c1', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open (applies 00039): %v", err)
	}
	defer store.Close()
	target, err := store.GetTarget(ctx, "t1")
	if err != nil || target.AgentArgs != nil {
		t.Fatalf("target from before the migration = %+v, %v; want no agent_args", target, err)
	}
	task, err := store.GetTask(ctx, "k1")
	if err != nil || !task.AgentArgs.IsZero() {
		t.Fatalf("task from before the migration = %+v, %v; want zero AgentArgs", task, err)
	}
	target.AgentArgs = map[string]registry.AgentArgs{"claude-code": {Model: "opus", Effort: "high"}}
	if err := store.UpdateTarget(ctx, target); err != nil {
		t.Fatal(err)
	}

	migrateMu.Lock()
	err = goose.DownTo(store.db, "migrations", 38)
	migrateMu.Unlock()
	if err != nil {
		t.Fatalf("goose.DownTo(38): %v", err)
	}
	var targets, tasks int
	if err := store.db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM targets), (SELECT count(*) FROM tasks)`).
		Scan(&targets, &tasks); err != nil || targets != 1 || tasks != 1 {
		t.Errorf("after down: %d targets, %d tasks, %v; want 1 and 1", targets, tasks, err)
	}
	for _, col := range []string{"targets.agent_args", "tasks.agent_model", "tasks.agent_effort"} {
		var n int
		table, column, _ := strings.Cut(col, ".")
		if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM pragma_table_info(?) WHERE name = ?`, table, column).
			Scan(&n); err != nil || n != 0 {
			t.Errorf("after down: column %s present (%d, %v); want it dropped", col, n, err)
		}
	}
}
