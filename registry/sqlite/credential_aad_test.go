package sqlite

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/Loomux/server/registry"
)

// LOOM-175: a credential's ciphertext is bound to its row: copied onto
// another credential's row it no longer decrypts.
func TestCredential_CiphertextBoundToRow(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "c.db"), WithMasterKey(bytes.Repeat([]byte{3}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, c := range []*registry.Credential{{ID: "c-a", Name: "A", Value: "secret-a"}, {ID: "c-b", Name: "B", Value: "secret-b"}} {
		if err := store.CreateCredential(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.SetCredentialValue(ctx, "c-a", "secret-a2"); err != nil {
		t.Fatal(err)
	}
	if got, err := store.GetCredential(ctx, "c-a"); err != nil || got.Value != "secret-a2" {
		t.Fatalf("c-a = %+v, %v", got, err)
	}

	if _, err := store.db.ExecContext(ctx, `UPDATE credentials SET ciphertext = (SELECT ciphertext FROM credentials WHERE id = 'c-a') WHERE id = 'c-b'`); err != nil {
		t.Fatal(err)
	}
	if got, err := store.GetCredential(ctx, "c-b"); err == nil {
		t.Fatalf("c-b with c-a's ciphertext decrypted to %q; want an error", got.Value)
	}
}

// LOOM-192: a credential's ciphertext is bound to its scope: a row moved
// to another workspace, target or agent type, or from unscoped to
// scoped, no longer decrypts, and decrypts again once moved back.
func TestCredential_CiphertextBoundToScope(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "scope.db")
	seedAt(t, dbPath, v020Schema, v020Seed)
	s, err := Open(dbPath, WithMasterKey(bytes.Repeat([]byte{7}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, c := range []*registry.Credential{
		{ID: "cr-3", Name: "NPM_TOKEN", WorkspaceID: "w-shell", Value: "npm-secret"},
		{ID: "cr-4", Name: "PROVIDER_TOKEN", TargetID: "t-jet", Value: "machine-secret"},
	} {
		if err := s.CreateCredential(ctx, c); err != nil {
			t.Fatal(err)
		}
	}

	for _, tc := range []struct{ id, swap, back string }{
		{"cr-2", `workspace_id = 'w-shell'`, `workspace_id = 'w-1'`},
		{"cr-2", `agent_type = 'codex'`, `agent_type = 'claude-code'`},
		{"cr-2", `workspace_id = NULL`, `workspace_id = 'w-1'`},
		{"cr-1", `workspace_id = 'w-1'`, `workspace_id = NULL`},
		{"cr-1", `agent_type = 'claude-code'`, `agent_type = ''`},
		{"cr-3", `workspace_id = 'w-1'`, `workspace_id = 'w-shell'`},
		{"cr-4", `target_id = 't-work'`, `target_id = 't-jet'`},
		{"cr-4", `target_id = NULL`, `target_id = 't-jet'`},
		{"cr-1", `target_id = 't-jet'`, `target_id = NULL`},
		{"cr-2", `target_id = 't-jet'`, `target_id = NULL`},
	} {
		set := func(clause string) {
			t.Helper()
			if _, err := s.db.ExecContext(ctx, `UPDATE credentials SET `+clause+` WHERE id = ?`, tc.id); err != nil {
				t.Fatal(err)
			}
		}
		set(tc.swap)
		if got, err := s.GetCredential(ctx, tc.id); err == nil {
			t.Errorf("%s with %s decrypted to %q; want an error", tc.id, tc.swap, got.Value)
		}
		if _, err := s.ListCredentials(ctx); err == nil {
			t.Errorf("ListCredentials with %s moved (%s) succeeded; want an error", tc.id, tc.swap)
		}
		set(tc.back)
		if _, err := s.GetCredential(ctx, tc.id); err != nil {
			t.Errorf("%s moved back (%s): %v", tc.id, tc.back, err)
		}
	}

	// A new value is sealed to the row's scope too.
	if err := s.SetCredentialValue(ctx, "cr-2", "rotated"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetCredential(ctx, "cr-2"); err != nil || got.Value != "rotated" {
		t.Fatalf("cr-2 = %+v, %v; want rotated", got, err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE credentials SET workspace_id = 'w-shell' WHERE id = 'cr-2'`); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetCredential(ctx, "cr-2"); err == nil {
		t.Errorf("rotated cr-2 moved to w-shell decrypted to %q; want an error", got.Value)
	}
	if err := s.SetCredentialValue(ctx, "cr-4", "rotated-machine"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE credentials SET target_id = 't-work' WHERE id = 'cr-4'`); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetCredential(ctx, "cr-4"); err == nil {
		t.Errorf("rotated cr-4 moved to t-work decrypted to %q; want an error", got.Value)
	}
}

// LOOM-175, LOOM-192: a v0.2.0 vault (no additional data) and a row from
// LOOM-175 (bound to its id only) are re-sealed to their row ids and
// scopes by the first Open with the master key, which records the
// re-seal as done, and then read as before. Opened without the key, or
// with another key, its rows are left alone and nothing is recorded.
func TestCredential_ResealOnOpen(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "v020.db")
	seedAt(t, dbPath, v020Schema, v020Seed)
	key := bytes.Repeat([]byte{7}, 32)

	count := func(s *Store, query string) int {
		t.Helper()
		var n int
		if err := s.db.QueryRowContext(ctx, query).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	const resealedRows = `SELECT COUNT(*) FROM credentials WHERE seal_version = 2`
	const resealDone = `SELECT COUNT(*) FROM vault_reseal WHERE seal_version = 2`

	for i, opts := range [][]Option{nil, {WithMasterKey(bytes.Repeat([]byte{9}, 32))}} {
		s, err := Open(dbPath, opts...)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if i == 0 {
			// A row as LOOM-175 sealed it: bound to its id alone.
			v1, err := seal(key, []byte("loom-175-value"), []byte("credential:cr-3"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.ExecContext(ctx, `INSERT INTO credentials (id, name, workspace_id, agent_type, ciphertext, seal_version, created_at, updated_at)
				VALUES ('cr-3', 'NPM_TOKEN', 'w-1', '', ?, 1, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`, v1); err != nil {
				t.Fatal(err)
			}
		}
		if n := count(s, resealedRows); n != 0 {
			t.Errorf("%d rows re-sealed without the right key; want none", n)
		}
		if n := count(s, resealDone); n != 0 {
			t.Errorf("re-seal recorded as done without the right key")
		}
		s.Close()
	}

	s, err := Open(dbPath, WithMasterKey(key))
	if err != nil {
		t.Fatalf("Open with the key: %v", err)
	}
	if n := count(s, resealedRows); n != 3 {
		t.Errorf("%d rows re-sealed; want all 3", n)
	}
	if n := count(s, resealDone); n != 1 {
		t.Errorf("re-seal not recorded as done")
	}
	for id, want := range map[string]string{"cr-1": "upgrade-test-value", "cr-2": "upgrade-test-value-scoped", "cr-3": "loom-175-value"} {
		if got, err := s.GetCredential(ctx, id); err != nil || got.Value != want {
			t.Errorf("credential %s = %+v, %v; want %q", id, got, err, want)
		}
	}
	// A row sealed the old way that appears later (written behind the
	// store's back) isn't read, and once the re-seal is recorded Open
	// doesn't scan the vault for it either.
	v1, err := seal(key, []byte("late"), []byte("credential:cr-1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE credentials SET ciphertext = ?, seal_version = 1 WHERE id = 'cr-1'`, v1); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetCredential(ctx, "cr-1"); err == nil {
		t.Errorf("a row sealed the old way read as %q; want an error", got.Value)
	}
	s.Close()

	s, err = Open(dbPath, WithMasterKey(key))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()
	if n := count(s, resealedRows); n != 2 {
		t.Errorf("%d rows at seal_version 2 after a reopen; want 2 (no re-scan once done)", n)
	}
}
