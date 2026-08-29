package sqlite_test

import (
	"path/filepath"
	"testing"

	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/registry/sqlite"
	"github.com/Loomux/server/registry/storetest"
)

func TestStore(t *testing.T) {
	storetest.Run(t, func(t *testing.T) registry.Store {
		dbPath := filepath.Join(t.TempDir(), "test.db")
		store, err := sqlite.Open(dbPath)
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
