package tmuxtest

import (
	"os"
	"path/filepath"
	"testing"
)

// Kill removes the <socket>.lock tmux leaves when its server is killed
// while starting, as well as the socket file (LOOM-243); CI fails a run
// that leaves either behind.
func TestKillRemovesSocketAndLock(t *testing.T) {
	t.Setenv("TMUX_TMPDIR", t.TempDir())
	name := "loomuxtest-kill"
	path := Path(name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{path, path + ".lock"} {
		if err := os.WriteFile(f, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	Kill(name)
	for _, f := range []string{path, path + ".lock"} {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Errorf("%s still exists after Kill (stat err %v)", filepath.Base(f), err)
		}
	}
}
