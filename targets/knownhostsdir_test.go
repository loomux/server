package targets_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/targets"
)

const pinLine = "pinned.invalid ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"

// LOOM-197: where a pinned target's known_hosts file goes is the
// caller's (each app instance's data dir), not a process-wide setting
// the last Build repointed.
func TestNewExecutorWith_KnownHostsDirIsTheCallers(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	for _, c := range []struct{ id, dir string }{{"t-a", dirA}, {"t-b", dirB}} {
		tgt := &registry.Target{ID: c.id, Kind: registry.TargetKindRemote, Host: "pinned.invalid", User: "u", HostKeys: pinLine}
		if _, err := targets.NewExecutorWith(tgt, targets.ExecutorOptions{KnownHostsDir: c.dir}); err != nil {
			t.Fatalf("NewExecutorWith(%s): %v", c.id, err)
		}
	}
	for _, c := range []struct {
		dir, id string
		want    bool
	}{{dirA, "t-a", true}, {dirA, "t-b", false}, {dirB, "t-b", true}, {dirB, "t-a", false}} {
		_, err := os.Stat(filepath.Join(c.dir, c.id))
		if got := err == nil; got != c.want {
			t.Errorf("%s in %s: present %v, want %v", c.id, c.dir, got, c.want)
		}
	}
}
