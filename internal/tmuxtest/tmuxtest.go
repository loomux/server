// Package tmuxtest gives each test a tmux server of its own (LOOM-210).
// A test on the shared `-L loomux` socket meets other runs' servers,
// and one left alive (remain-on-exit, its cwd a deleted t.TempDir)
// breaks the next test to reuse it; so no test runs on loomux, and
// each kills its own server and removes its socket file when done.
package tmuxtest

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
)

var count atomic.Int64

// Socket is a tmux socket name no other test, test run or instance
// uses. At t's cleanup its server is killed and its socket file
// removed; it outlives the cleanups t registers after this call.
func Socket(t testing.TB) string {
	t.Helper()
	name := fmt.Sprintf("loomuxtest-%d-%d", os.Getpid(), count.Add(1))
	t.Cleanup(func() { Kill(name) })
	return name
}

// Kill kills socket's tmux server, if one runs, and removes its socket
// file.
func Kill(socket string) {
	_ = exec.Command("tmux", "-L", socket, "kill-server").Run()
	_ = os.Remove(Path(socket))
}

// Path is where tmux puts socket's file: $TMUX_TMPDIR, else /tmp, then
// tmux-<uid>/<socket>.
func Path(socket string) string {
	dir := os.Getenv("TMUX_TMPDIR")
	if dir == "" {
		dir = "/tmp"
	}
	return filepath.Join(dir, "tmux-"+strconv.Itoa(os.Getuid()), socket)
}
