package plugins_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Loomux/server/plugins"
	"github.com/Loomux/server/registry"
)

func TestCatalogBundledAndDir(t *testing.T) {
	bundle := t.TempDir()
	fakeDir(t, bundle)
	pluginDir := t.TempDir()
	fakeDir(t, pluginDir)
	// A directory with a manifest but no executable, and one with a
	// manifest that doesn't validate: both skipped, nothing fails.
	noExe := filepath.Join(pluginDir, "noexe")
	_ = os.MkdirAll(noExe, 0o755)
	_ = os.WriteFile(filepath.Join(noExe, plugins.ManifestFile), []byte(`{"name":"noexe","version":"1","protocol":"loomux-plugin/1"}`), 0o644)
	bad := filepath.Join(pluginDir, "bad")
	_ = os.MkdirAll(bad, 0o755)
	_ = os.WriteFile(filepath.Join(bad, plugins.ManifestFile), []byte(`{"name":"bad","version":"1","protocol":"loomux-plugin/9"}`), 0o644)
	_ = os.WriteFile(filepath.Join(bad, plugins.ExecutableName("bad")), []byte("#!/bin/sh\n"), 0o755)

	c := &plugins.Catalog{BundleDir: bundle, PluginDir: pluginDir, SocketDir: filepath.Join(t.TempDir(), "missing")}
	got, err := c.Available(context.Background())
	if err != nil {
		t.Fatalf("Available: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Available = %d entries, want 2: %+v", len(got), got)
	}
	if got[0].Source != registry.PluginSourceBundled || got[0].Trust != registry.PluginTrustBundled || got[0].Isolation != plugins.IsolationNone || got[0].Manifest.Name != "fake" {
		t.Errorf("bundled entry = %+v", got[0])
	}
	if got[1].Source != registry.PluginSourceDir || got[1].Trust != registry.PluginTrustUnsigned || got[1].Path != filepath.Join(pluginDir, "fake") {
		t.Errorf("dir entry = %+v", got[1])
	}

	if a, err := c.Find(context.Background(), "fake", registry.PluginSourceDir); err != nil || a.Source != registry.PluginSourceDir {
		t.Errorf("Find dir: %+v, %v", a, err)
	}
	if _, err := c.Find(context.Background(), "fake", registry.PluginSourceSocket); !errors.Is(err, plugins.ErrNotAvailable) {
		t.Errorf("Find wrong source: %v", err)
	}
	if _, err := c.Find(context.Background(), "noexe", registry.PluginSourceDir); !errors.Is(err, plugins.ErrNotAvailable) {
		t.Errorf("Find skipped plugin: %v", err)
	}
}

func TestCatalogEmpty(t *testing.T) {
	c := &plugins.Catalog{}
	got, err := c.Available(context.Background())
	if err != nil || len(got) != 0 {
		t.Fatalf("Available on an empty catalog = %v, %v", got, err)
	}
}

func TestCatalogSocket(t *testing.T) {
	dir := shortTempDir(t)
	stop := serveFakeSocket(t, filepath.Join(dir, "fake.sock"))
	defer stop()
	// A socket nobody serves is skipped.
	_ = os.WriteFile(filepath.Join(dir, "dead.sock"), nil, 0o600)

	c := &plugins.Catalog{SocketDir: dir}
	got, err := c.Available(context.Background())
	if err != nil {
		t.Fatalf("Available: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Available = %+v", got)
	}
	if got[0].Source != registry.PluginSourceSocket || got[0].Isolation != plugins.IsolationContainer || got[0].Manifest.Name != "fake" || got[0].Path != filepath.Join(dir, "fake.sock") {
		t.Errorf("socket entry = %+v", got[0])
	}
}

type stubVerifier struct{ trust string }

func (s stubVerifier) Verify(ctx context.Context, dir string) (string, error) { return s.trust, nil }

func TestCatalogVerifier(t *testing.T) {
	pluginDir := t.TempDir()
	fakeDir(t, pluginDir)
	c := &plugins.Catalog{PluginDir: pluginDir, Verifier: stubVerifier{registry.PluginTrustSignedPrefix + "ci"}}
	got, err := c.Available(context.Background())
	if err != nil || len(got) != 1 || got[0].Trust != "signed:ci" {
		t.Fatalf("Available with verifier = %+v, %v", got, err)
	}
}
