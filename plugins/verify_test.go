package plugins_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Loomux/server/plugins"
	"github.com/Loomux/server/plugins/protocol"
	"github.com/Loomux/server/registry"
)

// LOOM-218: a plugin from the plugin directory is installed, upgraded
// to and started only when something vouches for its executable.

// switchVerifier answers what the test set.
type switchVerifier struct {
	mu         sync.Mutex
	trust, why string
}

func (s *switchVerifier) Verify(context.Context, string) (string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.trust, s.why
}

func (s *switchVerifier) set(trust, why string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.trust, s.why = trust, why
}

const pinned = registry.PluginTrustPinnedPrefix + "sha256:abc"

// dirManager is a Manager over pluginDir with v as its verifier.
func dirManager(t *testing.T, store registry.Store, pluginDir string, v plugins.Verifier, require bool) *plugins.Manager {
	t.Helper()
	m := plugins.NewManager(store, &plugins.Catalog{PluginDir: pluginDir, Verifier: v}, protocol.HostInfo{Version: "test", InstanceID: "inst"}, nil, nil)
	m.InstanceOptions = func(o *plugins.InstanceOptions) {
		o.RestartBackoff = 10 * time.Millisecond
		o.StopTimeout = 500 * time.Millisecond
	}
	m.RequireVerified = require
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	return m
}

func installDir(m *plugins.Manager, label string) (*plugins.View, error) {
	return m.Install(context.Background(), plugins.InstallRequest{Name: "fake", Source: registry.PluginSourceDir, Label: label})
}

func TestUnverifiedPluginRefused(t *testing.T) {
	pluginDir := t.TempDir()
	fakeDir(t, pluginDir)
	store := newStore(t, filepath.Join(t.TempDir(), "m.db"))
	v := &switchVerifier{trust: registry.PluginTrustUnsigned, why: "nothing vouches for it"}
	m := dirManager(t, store, pluginDir, v, true)

	_, err := installDir(m, "dirfake")
	var unverified *plugins.UnverifiedError
	if !errors.As(err, &unverified) || !strings.Contains(err.Error(), "nothing vouches for it") || !strings.Contains(err.Error(), "LOOMUX_PLUGIN_ALLOW_UNSIGNED") {
		t.Fatalf("install of an unverified plugin: %v", err)
	}
	if rows, _ := store.ListPlugins(context.Background()); len(rows) != 0 {
		t.Errorf("a refused install left %d rows", len(rows))
	}

	v.set(pinned, "")
	view, err := installDir(m, "dirfake")
	if err != nil || view.Trust != pinned || view.Status != registry.PluginStatusInstalled {
		t.Fatalf("install of a pinned plugin: %+v, %v", view, err)
	}
}

// The opt-out installs an unverified plugin, as before LOOM-218.
func TestAllowUnsignedInstalls(t *testing.T) {
	pluginDir := t.TempDir()
	fakeDir(t, pluginDir)
	store := newStore(t, filepath.Join(t.TempDir(), "m.db"))
	m := dirManager(t, store, pluginDir, &switchVerifier{trust: registry.PluginTrustUnsigned, why: "no bundle"}, false)
	view, err := installDir(m, "dirfake")
	if err != nil || view.Trust != registry.PluginTrustUnsigned {
		t.Fatalf("install with the opt-out: %+v, %v", view, err)
	}
}

// Bundled plugins are trusted as the image is: verification doesn't
// apply to them.
func TestBundledUnaffected(t *testing.T) {
	m, _, _ := newManager(t)
	m.RequireVerified = true
	if v := install(t, m, "bundled", nil); v.Trust != registry.PluginTrustBundled {
		t.Errorf("trust = %q", v.Trust)
	}
}

func TestUpgradeToUnverifiedRefused(t *testing.T) {
	pluginDir := t.TempDir()
	fakeDir(t, pluginDir)
	store := newStore(t, filepath.Join(t.TempDir(), "m.db"))
	v := &switchVerifier{trust: pinned}
	m := dirManager(t, store, pluginDir, v, true)
	view, err := installDir(m, "dirfake")
	if err != nil {
		t.Fatal(err)
	}
	fakeDirVersion(t, pluginDir, "0.2.0", fakeBin2)
	v.set(registry.PluginTrustUnsigned, "the new build has no bundle")
	_, err = m.Upgrade(context.Background(), view.ID)
	var unverified *plugins.UnverifiedError
	if !errors.As(err, &unverified) || !strings.Contains(err.Error(), "the new build has no bundle") {
		t.Fatalf("upgrade to an unverified build: %v", err)
	}
	if got, _ := m.Get(context.Background(), view.ID); got.Version != "0.1.0" || got.Trust != pinned {
		t.Errorf("after the refused upgrade: version %s, trust %s", got.Version, got.Trust)
	}
	v.set(registry.PluginTrustSignedPrefix+"ci", "")
	if got, err := m.Upgrade(context.Background(), view.ID); err != nil || got.Version != "0.2.0" || got.Trust != registry.PluginTrustSignedPrefix+"ci" {
		t.Errorf("upgrade to a verified build: %+v, %v", got, err)
	}
}

// At start: an executable replaced since it was installed verified is
// refused (its stand-in must not run); one installed unverified (with
// the opt-out, or before LOOM-218) still starts.
func TestStartChecksTheExecutable(t *testing.T) {
	t.Run("replaced since a verified install", func(t *testing.T) {
		pluginDir := t.TempDir()
		fakeDir(t, pluginDir)
		dbPath := filepath.Join(t.TempDir(), "m.db")
		store := newStore(t, dbPath)
		v := &switchVerifier{trust: pinned}
		first := dirManager(t, store, pluginDir, v, true)
		view, err := installDir(first, "dirfake")
		if err != nil {
			t.Fatal(err)
		}
		_ = first.Close(context.Background())

		v.set(registry.PluginTrustUnsigned, "its digest isn't pinned any more")
		m := dirManager(t, store, pluginDir, v, true)
		if err := m.StartAll(context.Background()); err == nil {
			t.Error("StartAll started a replaced executable")
		}
		got, _ := m.Get(context.Background(), view.ID)
		if got.Status != registry.PluginStatusError || !strings.Contains(got.StatusReason, "no longer verifies") || got.Instance.State == plugins.StateRunning {
			t.Errorf("after the start: %s (%s), %s", got.Status, got.StatusReason, got.Instance.State)
		}

		// With the opt-out (a local build replaced), it starts.
		_ = m.Close(context.Background())
		dev := dirManager(t, store, pluginDir, v, false)
		if err := dev.StartAll(context.Background()); err != nil {
			t.Errorf("StartAll with the opt-out: %v", err)
		}
	})
	t.Run("installed unverified", func(t *testing.T) {
		pluginDir := t.TempDir()
		fakeDir(t, pluginDir)
		store := newStore(t, filepath.Join(t.TempDir(), "m.db"))
		v := &switchVerifier{trust: registry.PluginTrustUnsigned, why: "no bundle"}
		first := dirManager(t, store, pluginDir, v, false)
		view, err := installDir(first, "dirfake")
		if err != nil {
			t.Fatal(err)
		}
		_ = first.Close(context.Background())
		m := dirManager(t, store, pluginDir, v, true)
		if err := m.StartAll(context.Background()); err != nil {
			t.Errorf("StartAll of a plugin installed unverified: %v", err)
		}
		if got, _ := m.Get(context.Background(), view.ID); got.Instance.State != plugins.StateRunning {
			t.Errorf("not running: %+v", got.Instance)
		}
	})
}
