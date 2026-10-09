package sqlite_test

import (
	"path/filepath"
	"testing"

	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/registry/sqlite"
	"github.com/Loomux/server/registry/storetest"
)

// Without a master key, plugin secrets can't be stored (LOOM-178).
func TestStoreConformance_NoMasterKey(t *testing.T) {
	storetest.RunWithoutMasterKey(t, func(t *testing.T) registry.Store {
		t.Helper()
		store, err := sqlite.Open(filepath.Join(t.TempDir(), "nokey.db"))
		if err != nil {
			t.Fatalf("sqlite.Open: %v", err)
		}
		t.Cleanup(func() { _ = store.Close() })
		return store
	})
}
