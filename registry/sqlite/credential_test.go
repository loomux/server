package sqlite_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/registry/sqlite"
)

func TestCredential_WrongMasterKeyFailsToDecrypt(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	keyA := bytes.Repeat([]byte("A"), 32)
	keyB := bytes.Repeat([]byte("B"), 32)
	ctx := context.Background()

	storeA, err := sqlite.Open(dbPath, sqlite.WithMasterKey(keyA))
	if err != nil {
		t.Fatalf("sqlite.Open(keyA): %v", err)
	}
	cred := &registry.Credential{ID: "cred-1", Name: "TOKEN", Value: "top-secret"}
	if err := storeA.CreateCredential(ctx, cred); err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}
	if err := storeA.Close(); err != nil {
		t.Fatalf("Close storeA: %v", err)
	}

	storeB, err := sqlite.Open(dbPath, sqlite.WithMasterKey(keyB))
	if err != nil {
		t.Fatalf("sqlite.Open(keyB): %v", err)
	}
	defer storeB.Close()

	if _, err := storeB.GetCredential(ctx, cred.ID); err == nil {
		t.Fatalf("GetCredential with the wrong master key: got nil error, want a decryption failure")
	}
}

func TestCredential_NoMasterKeyConfiguredFailsFast(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	store, err := sqlite.Open(dbPath) // no WithMasterKey
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	cred := &registry.Credential{ID: "cred-1", Name: "TOKEN", Value: "value"}
	if err := store.CreateCredential(ctx, cred); err == nil {
		t.Fatalf("CreateCredential without a master key: got nil error, want a fail-fast error")
	}
}

func TestCredential_EncryptedAtRest(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	store, err := sqlite.Open(dbPath, sqlite.WithMasterKey(bytes.Repeat([]byte("K"), 32)))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}

	distinctive := "distinctive-plaintext-marker-should-never-appear-on-disk"
	cred := &registry.Credential{ID: "cred-1", Name: "TOKEN", Value: distinctive}
	if err := store.CreateCredential(context.Background(), cred); err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	raw, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if bytes.Contains(raw, []byte(distinctive)) {
		t.Fatalf("plaintext credential value appears verbatim in the raw database file")
	}
}
