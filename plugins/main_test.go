package plugins_test

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Loomux/server/plugins"
	"github.com/Loomux/server/plugins/fake"
	"github.com/Loomux/server/plugins/sdk"
)

// fakeBin is the fake plugin's executable, built once for the package.
var fakeBin string

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
// stop is called, and waits for the socket to exist.
func serveFakeSocket(t *testing.T, path string) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		sdk.RunContext(ctx, fake.New(), []string{"--listen", path}, strings.NewReader(""), io.Discard, io.Discard)
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("socket never appeared")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return func() {
		cancel()
		<-done
	}
}
