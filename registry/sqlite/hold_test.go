package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/Loomux/server/registry"
)

// TestMigration36TaskHolds (LOOM-263): a database at 00035 holding a
// task already in human-takeover opens, keeps the task as it is with no
// hold made up for it, and has the task_holds table with its one active
// drive hold per task enforced by the index itself, not only by the
// store. Down drops the table and leaves the tasks.
func TestMigration36TaskHolds(t *testing.T) {
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
	if err := goose.UpTo(db, "migrations", 35); err != nil {
		t.Fatalf("goose.UpTo(35): %v", err)
	}
	for _, stmt := range []string{
		`INSERT INTO targets (id, name, kind, created_at, updated_at) VALUES ('t1', 'local', 'local', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		`INSERT INTO workspaces (id, name, path, target_id, status, created_at, updated_at)
		 VALUES ('w1', 'ws', '/tmp', 't1', 'active', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		`INSERT INTO tasks (id, workspace_id, kind, agent_type, tmux_session, status, conversation_id, created_at, updated_at)
		 VALUES ('k1', 'w1', 'agent', 'claude-code', 'loomux-x', 'human-takeover', 'c1', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
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
		t.Fatalf("Open (applies 00036): %v", err)
	}
	defer store.Close()

	task, err := store.GetTask(ctx, "k1")
	if err != nil || task.Status != registry.TaskStatusHumanTakeover {
		t.Fatalf("task after migration = %+v, %v; want it still human-takeover", task, err)
	}
	if holds, err := store.ListActiveTaskHolds(ctx); err != nil || len(holds) != 0 {
		t.Fatalf("holds after migration = %+v, %v; want none", holds, err)
	}

	insert := `INSERT INTO task_holds (id, task_id, session_id, mode, acquired_at, renewed_at, ttl_ms, expires_at, ended_at)
		VALUES (?, 'k1', 's1', ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, 45000, CURRENT_TIMESTAMP, ?)`
	if _, err := store.db.ExecContext(ctx, insert, "h1", "drive", nil); err != nil {
		t.Fatalf("first drive hold: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, insert, "h2", "drive", nil); !isUniqueConstraintErr(err) {
		t.Errorf("second active drive hold = %v; want the unique index to refuse it", err)
	}
	if _, err := store.db.ExecContext(ctx, insert, "h3", "drive", time.Now().UTC()); err != nil {
		t.Errorf("an ended drive hold beside an active one: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, insert, "h4", "watch", nil); err != nil {
		t.Errorf("a watch hold beside an active drive hold: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, insert, "h5", "steer", nil); err == nil {
		t.Error("a hold with an unknown mode was accepted")
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO task_holds (id, task_id, session_id, mode, acquired_at, renewed_at, ttl_ms, expires_at)
		VALUES ('h6', 'no-such-task', 's1', 'drive', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, 45000, CURRENT_TIMESTAMP)`); !isForeignKeyConstraintErr(err) {
		t.Errorf("a hold on an unknown task = %v; want the foreign key to refuse it", err)
	}

	if err := goose.DownTo(store.db, "migrations", 35); err != nil {
		t.Fatalf("goose.DownTo(35): %v", err)
	}
	var n int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE name LIKE '%task_holds%'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("after down: %d task_holds objects left, %v; want none", n, err)
	}
	if _, err := store.GetTask(ctx, "k1"); err != nil {
		t.Errorf("task after down: %v", err)
	}
}

// Ended holds are deleted in batches like every retention delete
// (LOOM-193), and an active one is never among them.
func TestRetentionDeletesTaskHoldsInBatches(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.retentionBatch = 2
	var chunks []int
	s.afterRetentionChunk = func(table string, n int) {
		if table != "task_holds" {
			t.Errorf("chunk on table %q", table)
		}
		chunks = append(chunks, n)
	}
	if err := s.CreateTarget(ctx, &registry.Target{ID: "t", Name: "t", Kind: registry.TargetKindLocal}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateWorkspace(ctx, &registry.Workspace{ID: "w", Name: "w", Path: "/w", TargetID: "t", Status: registry.WorkspaceStatusIdle}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateTask(ctx, &registry.Task{ID: "k", WorkspaceID: "w", Kind: registry.TaskKindAgent, TmuxSession: "loomux-k",
		Status: registry.TaskStatusAwaitingInput}); err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-40 * 24 * time.Hour)
	for i := range 6 {
		id := fmt.Sprint("h-", i)
		if err := s.CreateTaskHold(ctx, &registry.TaskHold{ID: id, TaskID: "k", SessionID: "s", Mode: registry.HoldModeDrive,
			TTL: time.Minute, AcquiredAt: old}); err != nil {
			t.Fatal(err)
		}
		if i == 5 {
			break // the last one stays active
		}
		if _, _, err := s.EndTaskHold(ctx, id, registry.HoldEndReleased, old.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.DeleteTaskHoldsEndedBefore(ctx, time.Now())
	if err != nil || n != 5 {
		t.Errorf("deleted %d, %v; want 5", n, err)
	}
	if want := []int{2, 2, 1}; !slices.Equal(chunks, want) {
		t.Errorf("chunks %v; want %v", chunks, want)
	}
	if h, err := s.GetActiveDriveHold(ctx, "k"); err != nil || h.ID != "h-5" {
		t.Errorf("active hold = %+v, %v; want h-5 kept", h, err)
	}
}
