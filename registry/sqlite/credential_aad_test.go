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

// LOOM-175: a v0.2.0 vault (no additional data) is re-sealed to its row
// ids by the first Open with the master key, and then reads as before.
// Opened without the key, or with another key, its rows are left alone.
func TestCredential_ResealOnOpen(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "v020.db")
	seedAt(t, dbPath, v020Schema, v020Seed)
	key := bytes.Repeat([]byte{7}, 32)

	sealed := func(s *Store) int {
		t.Helper()
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM credentials WHERE sealed_to_id = 1`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	for _, opts := range [][]Option{nil, {WithMasterKey(bytes.Repeat([]byte{9}, 32))}} {
		s, err := Open(dbPath, opts...)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if n := sealed(s); n != 0 {
			t.Errorf("%d rows re-sealed without the right key; want none", n)
		}
		s.Close()
	}

	s, err := Open(dbPath, WithMasterKey(key))
	if err != nil {
		t.Fatalf("Open with the key: %v", err)
	}
	if n := sealed(s); n != 2 {
		t.Errorf("%d rows re-sealed; want both", n)
	}
	for id, want := range map[string]string{"cr-1": "upgrade-test-value", "cr-2": "upgrade-test-value-scoped"} {
		if got, err := s.GetCredential(ctx, id); err != nil || got.Value != want {
			t.Errorf("credential %s = %+v, %v; want %q", id, got, err, want)
		}
	}
	// A row unbound to its id that appears later (written behind the
	// store's back) isn't read.
	if _, err := s.db.ExecContext(ctx, `UPDATE credentials SET sealed_to_id = 0 WHERE id = 'cr-1'`); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetCredential(ctx, "cr-1"); err == nil {
		t.Errorf("an unbound row read as %q; want an error", got.Value)
	}
	s.Close()
}
