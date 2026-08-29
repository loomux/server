package sqlite_test

import (
	"path/filepath"
	"testing"

	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/registry/sqlite"
	"github.com/Loomux/server/registry/storetest"
)

// testMasterKey is a fixed 32-byte (AES-256) key for tests that don't
// care about key secrecy, just that a valid-length key is configured.
func testMasterKey() []byte {
	return []byte("01234567890123456789012345678901"[:32])
}

func TestStore(t *testing.T) {
	storetest.Run(t, func(t *testing.T) registry.Store {
		dbPath := filepath.Join(t.TempDir(), "test.db")
		store, err := sqlite.Open(dbPath, sqlite.WithMasterKey(testMasterKey()))
		if err != nil {
			t.Fatalf("sqlite.Open: %v", err)
		}
		t.Cleanup(func() {
			if err := store.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
		})
		return store
	})
}
