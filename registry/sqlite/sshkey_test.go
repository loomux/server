package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/pressly/goose/v3"

	"github.com/Loomux/server/registry"
)

func testSSHKey(id, name, secret string) *registry.SSHKey {
	return &registry.SSHKey{ID: id, Name: name, Type: "ssh-ed25519", PublicKey: "ssh-ed25519 AAAA loomux-" + name,
		Fingerprint: "SHA256:" + name, Origin: registry.SSHKeyOriginGenerated, PrivateKey: []byte(secret)}
}

// LOOM-138: the private key is encrypted at rest, and each ciphertext is
// bound to its row: copied onto another key's row it no longer decrypts.
func TestSSHKey_EncryptedAndBoundToItsRow(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "keys.db")
	s, err := Open(dbPath, WithMasterKey(bytes.Repeat([]byte("k"), 32)))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, k := range []*registry.SSHKey{testSSHKey("a", "alpha", "PRIVATE-ALPHA"), testSSHKey("b", "beta", "PRIVATE-BETA")} {
		if err := s.CreateSSHKey(ctx, k); err != nil {
			t.Fatal(err)
		}
	}
	var blob []byte
	if err := s.db.QueryRowContext(ctx, `SELECT private_key FROM ssh_keys WHERE id = 'a'`).Scan(&blob); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(blob, []byte("PRIVATE-ALPHA")) {
		t.Fatal("private key stored in plaintext")
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE ssh_keys SET private_key = ? WHERE id = 'b'`, blob); err != nil {
		t.Fatal(err)
	}
	if k, err := s.GetSSHKey(ctx, "b"); err == nil {
		t.Fatalf("a's ciphertext decrypted as b's key: %q", k.PrivateKey)
	}
	if k, err := s.GetSSHKey(ctx, "a"); err != nil || string(k.PrivateKey) != "PRIVATE-ALPHA" {
		t.Fatalf("GetSSHKey(a) = %+v, %v", k, err)
	}
}

// Without a master key nothing secret can be written or read, but keys
// can still be listed and deleted (a key whose master key is lost has to
// be removable).
func TestSSHKey_NoMasterKey(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "keys.db")
	withKey, err := Open(dbPath, WithMasterKey(bytes.Repeat([]byte("k"), 32)))
	if err != nil {
		t.Fatal(err)
	}
	if err := withKey.CreateSSHKey(ctx, testSSHKey("a", "alpha", "secret")); err != nil {
		t.Fatal(err)
	}
	withKey.Close()

	s, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.CreateSSHKey(ctx, testSSHKey("b", "beta", "secret")); !errors.Is(err, registry.ErrNoMasterKey) {
		t.Errorf("CreateSSHKey: %v, want ErrNoMasterKey", err)
	}
	if _, err := s.GetSSHKey(ctx, "a"); !errors.Is(err, registry.ErrNoMasterKey) {
		t.Errorf("GetSSHKey: %v, want ErrNoMasterKey", err)
	}
	if list, err := s.ListSSHKeys(ctx); err != nil || len(list) != 1 {
		t.Errorf("ListSSHKeys = %v, %v", list, err)
	}
	if err := s.DeleteSSHKey(ctx, "a"); err != nil {
		t.Errorf("DeleteSSHKey: %v", err)
	}
}

// Migration 23: nothing ever read targets.ssh_key_ref before LOOM-138,
// so whatever an early client stored there is cleared; from now on a
// non-empty one names a managed key.
func TestMigration23ClearsDeadSSHKeyRef(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "migrate.db")
	db, err := sql.Open("sqlite", dsn(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	goose.SetTableName("schema_migrations")
	goose.SetBaseFS(migrationsFS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpTo(db, "migrations", 22); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO targets (id, name, kind, host, user, ssh_key_ref, created_at, updated_at)
		VALUES ('t1', 'wyzer', 'remote', 'wyzer', 'orski', 'vault:old-key', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open (applies 00023): %v", err)
	}
	defer s.Close()
	got, err := s.GetTarget(ctx, "t1")
	if err != nil || got.SSHKeyRef != "" {
		t.Fatalf("target after migration = %+v, %v; want ssh_key_ref cleared", got, err)
	}
	got.Name = "wyzer-renamed"
	if err := s.UpdateTarget(ctx, got); err != nil {
		t.Fatalf("UpdateTarget after migration: %v", err)
	}
}

// A wrong master key fails to decrypt, and the error carries nothing of
// the key.
func TestSSHKey_WrongMasterKeyErrorHasNoKeyMaterial(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "keys.db")
	a, err := Open(dbPath, WithMasterKey(bytes.Repeat([]byte("A"), 32)))
	if err != nil {
		t.Fatal(err)
	}
	if err := a.CreateSSHKey(ctx, testSSHKey("a", "alpha", "PRIVATE-ALPHA")); err != nil {
		t.Fatal(err)
	}
	a.Close()
	b, err := Open(dbPath, WithMasterKey(bytes.Repeat([]byte("B"), 32)))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	_, err = b.GetSSHKey(ctx, "a")
	if err == nil || bytes.Contains([]byte(err.Error()), []byte("PRIVATE")) {
		t.Errorf("GetSSHKey with the wrong master key: %v", err)
	}
}
