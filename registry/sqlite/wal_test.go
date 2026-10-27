package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// LOOM-248: every connection the pool opens is in WAL mode, waits out a
// lock for the busy timeout, and enforces foreign keys; not only the one
// a PRAGMA happened to run on.
func TestOpen_EveryConnectionWALAndBusyTimeout(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "wal.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Holding each, the pool opens a new one for the next.
	var conns []*sql.Conn
	defer func() {
		for _, c := range conns {
			_ = c.Close()
		}
	}()
	for i := range 4 {
		c, err := store.db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
		var mode string
		var busy, fk int
		if err := c.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil {
			t.Fatal(err)
		}
		if err := c.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&busy); err != nil {
			t.Fatal(err)
		}
		if err := c.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk); err != nil {
			t.Fatal(err)
		}
		if mode != "wal" || busy != sqliteBusyTimeout || fk != 1 {
			t.Errorf("connection %d: journal_mode %s, busy_timeout %d, foreign_keys %d; want wal, %d, 1",
				i, mode, busy, fk, sqliteBusyTimeout)
		}
	}
}

// LOOM-248: a database a release before WAL left in rollback-journal
// mode opens with today's code, its rows intact, and is in WAL from then
// on.
func TestOpen_ConvertsRollbackJournalDatabase(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "v020.db")
	seedAt(t, dbPath, v020Schema, v020Seed)
	if _, err := os.Stat(dbPath + "-wal"); !os.IsNotExist(err) {
		t.Fatalf("the seeded database already has a -wal file (%v): it isn't as an old release left it", err)
	}

	store, err := Open(dbPath, WithMasterKey(bytes.Repeat([]byte{7}, 32)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	var mode string
	if err := store.db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		t.Errorf("journal_mode = %q, %v; want wal", mode, err)
	}
	if _, err := store.GetTarget(ctx, "t-jet"); err != nil {
		t.Errorf("a seeded target after the switch: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopened, it is still WAL, and still reads.
	store, err = Open(dbPath, WithMasterKey(bytes.Repeat([]byte{7}, 32)))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer store.Close()
	if creds, err := store.ListCredentials(ctx); err != nil || len(creds) != 2 {
		t.Errorf("credentials after reopening = %d, %v; want 2", len(creds), err)
	}
}

// LOOM-248: transactions that read and then write, run at once, all
// commit. Deferred, the second to write can't wait for the first (SQLite
// fails it with SQLITE_BUSY at once rather than risk a deadlock);
// IMMEDIATE takes the write lock at BEGIN, where the busy timeout
// applies.
func TestTransactions_ReadThenWriteConcurrently(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "tx.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.db.ExecContext(ctx, `CREATE TABLE counter (n INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO counter (n) VALUES (0)`); err != nil {
		t.Fatal(err)
	}

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Go(func() {
			errs[i] = func() error {
				tx, err := store.db.BeginTx(ctx, nil)
				if err != nil {
					return err
				}
				defer func() { _ = tx.Rollback() }()
				var v int
				if err := tx.QueryRowContext(ctx, `SELECT n FROM counter`).Scan(&v); err != nil {
					return err
				}
				time.Sleep(20 * time.Millisecond)
				if _, err := tx.ExecContext(ctx, `UPDATE counter SET n = ?`, v+1); err != nil {
					return err
				}
				return tx.Commit()
			}()
		})
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("transaction %d: %v", i, err)
		}
	}
	// Each read what the one before it wrote: none was lost.
	var got int
	if err := store.db.QueryRowContext(ctx, `SELECT n FROM counter`).Scan(&got); err != nil || got != n {
		t.Errorf("counter = %d, %v; want %d", got, err, n)
	}
}
