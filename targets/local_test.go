package targets_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Loomux/server/internal/tmuxtest"
	"github.com/Loomux/server/targets"
	"github.com/Loomux/server/targets/executortest"
)

func TestLocalExecutor(t *testing.T) {
	executortest.Run(t, func(t *testing.T) targets.TargetExecutor {
		exec := targets.NewLocalExecutor(tmuxtest.Socket(t))
		t.Cleanup(func() {
			if err := exec.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
		})
		return exec
	})
}

// LOOM-265 review: DetachClients fails closed. A tmux that fails for any
// reason other than there being nothing to detach (here, a socket
// directory it refuses for its permissions) is an error, never "no
// clients".
func TestLocalExecutor_DetachClientsFailsClosed(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMUX_TMPDIR", dir)
	sockDir := filepath.Join(dir, fmt.Sprintf("tmux-%d", os.Getuid()))
	if err := os.Mkdir(sockDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sockDir, 0o777); err != nil {
		t.Fatal(err)
	}
	err := targets.NewLocalExecutor("loomux-unsafe").DetachClients(context.Background(), "s")
	if err == nil || !strings.Contains(err.Error(), "unsafe permissions") {
		t.Fatalf("DetachClients with a socket directory tmux refuses = %v; want tmux's error", err)
	}
}
