package plugins_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Loomux/server/plugins"
	"github.com/Loomux/server/plugins/fake"
	"github.com/Loomux/server/plugins/sdk"
)

// fakeBin is the fake plugin's executable, built once for the package;
// fakeBin2 the same reporting version 0.2.0, a newer bundled plugin.
var fakeBin, fakeBin2 string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "loomux-plugin-fake-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fakeBin = filepath.Join(dir, plugins.ExecutableName("fake"))
	build := exec.Command("go", "build", "-o", fakeBin, "github.com/Loomux/server/cmd/loomux-plugin-fake")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "building the fake plugin:", err)
		os.Exit(1)
	}
	fakeBin2 = filepath.Join(dir, "loomux-plugin-fake-0.2.0")
	build = exec.Command("go", "build", "-ldflags", "-X github.com/Loomux/server/plugins/fake.Version=0.2.0", "-o", fakeBin2, "github.com/Loomux/server/cmd/loomux-plugin-fake")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "building the fake plugin at 0.2.0:", err)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// fakeDir writes a plugin directory for the fake under root: the
// manifest and a copy of the executable.
func fakeDir(t *testing.T, root string) string {
	t.Helper()
	dir := filepath.Join(root, "fake")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, plugins.ManifestFile), fake.ManifestJSON, 0o644); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(fakeBin)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, plugins.ExecutableName("fake")), data, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// fakeDirVersion replaces the fake under root with one at version (its
// manifest edited, the executable bin), as a new server image would.
// The old executable is unlinked first: it may still be running.
func fakeDirVersion(t *testing.T, root, version, bin string) string {
	t.Helper()
	dir := filepath.Join(root, "fake")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	m := strings.Replace(string(fake.ManifestJSON), `"version": "0.1.0"`, `"version": "`+version+`"`, 1)
	if err := os.WriteFile(filepath.Join(dir, plugins.ManifestFile), []byte(m), 0o644); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(dir, plugins.ExecutableName("fake"))
	_ = os.Remove(exe)
	if err := os.WriteFile(exe, data, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// fakeManifest is the fake's manifest as the host reads it.
func fakeManifest(t *testing.T) *plugins.Manifest {
	t.Helper()
	m, err := plugins.ParseManifest(fake.ManifestJSON)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// shortTempDir is a temp dir whose path fits a unix socket.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "lxp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// serveFakeSocket serves the fake on path in-process until the returned
// stop is called, and waits for the socket to accept connections.
func serveFakeSocket(t *testing.T, path string) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		sdk.RunContext(ctx, fake.New(), []string{"--listen", path}, strings.NewReader(""), io.Discard, io.Discard)
	}()
	if err := waitListening(path, 3*time.Second); err != nil {
		cancel()
		<-done
		t.Fatal(err)
	}
	return func() {
		cancel()
		<-done
	}
}

// waitListening waits until the unix socket at path accepts a dial; its
// file existing isn't enough, as it exists from bind(), before listen().
func waitListening(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		conn, err := net.DialTimeout("unix", path, time.Second)
		if err == nil {
			return conn.Close()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("socket %s never accepted: %w", path, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The socket file exists from bind(), before listen(), and a dial in
// between is refused: TestCatalogSocket once listed no plugin that way
// (LOOM-209). The wait holds until the socket accepts.
func TestWaitListeningWaitsForListen(t *testing.T) {
	path := filepath.Join(shortTempDir(t), "late.sock")
	fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Bind(fd, &syscall.SockaddrUnix{Name: path}); err != nil {
		syscall.Close(fd)
		t.Fatal(err)
	}
	listened := make(chan error, 1)
	go func() {
		time.Sleep(200 * time.Millisecond)
		listened <- syscall.Listen(fd, 8)
	}()
	defer func() {
		if err := <-listened; err != nil {
			t.Error("listen:", err)
		}
		syscall.Close(fd)
	}()

	if err := waitListening(path, 3*time.Second); err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("dial right after the wait: %v", err)
	}
	conn.Close()
}
