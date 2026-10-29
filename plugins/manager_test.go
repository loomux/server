package plugins_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Loomux/server/plugins"
	"github.com/Loomux/server/plugins/fake"
	"github.com/Loomux/server/plugins/protocol"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/registry/sqlite"
)

func testMasterKey() []byte { return []byte("01234567890123456789012345678901") }

func newStore(t *testing.T, path string) registry.Store {
	t.Helper()
	store, err := sqlite.Open(path, sqlite.WithMasterKey(testMasterKey()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// newManager makes a Manager over a bundle dir holding the fake and a
// fresh store; bundle is returned so a test can tamper with it.
func newManager(t *testing.T) (*plugins.Manager, registry.Store, string) {
	t.Helper()
	bundle := t.TempDir()
	fakeDir(t, bundle)
	store := newStore(t, filepath.Join(t.TempDir(), "m.db"))
	m := managerOn(t, store, bundle)
	return m, store, bundle
}

func managerOn(t *testing.T, store registry.Store, bundle string) *plugins.Manager {
	t.Helper()
	m := plugins.NewManager(store, &plugins.Catalog{BundleDir: bundle}, protocol.HostInfo{Version: "test", InstanceID: "inst"}, nil, nil)
	m.InstanceOptions = func(o *plugins.InstanceOptions) {
		o.RestartBackoff = 10 * time.Millisecond
		o.StopTimeout = 500 * time.Millisecond
	}
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	return m
}

func install(t *testing.T, m *plugins.Manager, label string, config map[string]any) *plugins.View {
	t.Helper()
	v, err := m.Install(context.Background(), plugins.InstallRequest{Name: "fake", Source: registry.PluginSourceBundled, Label: label, Config: config})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	return v
}

func TestManagerInstall(t *testing.T) {
	t.Setenv("LOOMUX_SECRET", "x")
	m, _, _ := newManager(t)
	v := install(t, m, "one", map[string]any{"token": "s3cret"})
	if v.Status != registry.PluginStatusInstalled || !v.Enabled || v.Instance.State != plugins.StateRunning {
		t.Fatalf("view = %+v", v)
	}
	if v.Secrets != nil {
		t.Error("a view must not carry secrets")
	}
	if set, _ := v.Config["token"].(map[string]any); set["set"] != true {
		t.Errorf("token in view = %v", v.Config["token"])
	}
	if v.Config["greeting"] != "hi" || v.Config["mode"] != "ok" {
		t.Errorf("defaults not applied: %v", v.Config)
	}
	if v.Check == nil || !v.Check.OK {
		t.Errorf("check = %+v", v.Check)
	}
	if v.AvailableVersion != "0.1.0" || v.Trust != registry.PluginTrustBundled {
		t.Errorf("view = %+v", v)
	}
	b, _ := json.Marshal(v)
	if strings.Contains(string(b), "s3cret") {
		t.Error("the view's JSON leaks the secret")
	}

	views, err := m.List(context.Background())
	if err != nil || len(views) != 1 || views[0].Label != "one" {
		t.Fatalf("List = %+v, %v", views, err)
	}
	got, err := m.Get(context.Background(), v.ID)
	if err != nil || got.Instance.State != plugins.StateRunning {
		t.Fatalf("Get = %+v, %v", got, err)
	}
}

func TestManagerInstallRefuses(t *testing.T) {
	m, _, _ := newManager(t)
	_, err := m.Install(context.Background(), plugins.InstallRequest{Name: "fake", Source: registry.PluginSourceBundled, Label: "x", Config: map[string]any{"colour": "red"}})
	var ce *plugins.ConfigError
	if !errors.As(err, &ce) || ce.Field != "colour" {
		t.Errorf("unknown key: %v", err)
	}
	_, err = m.Install(context.Background(), plugins.InstallRequest{Name: "nope", Source: registry.PluginSourceBundled, Label: "x"})
	if !errors.Is(err, plugins.ErrNotAvailable) {
		t.Errorf("unknown plugin: %v", err)
	}
	_, err = m.Install(context.Background(), plugins.InstallRequest{Name: "fake", Source: registry.PluginSourceBundled, Label: "Bad Label"})
	if !errors.Is(err, plugins.ErrBadLabel) {
		t.Errorf("bad label: %v", err)
	}
	install(t, m, "dup", nil)
	_, err = m.Install(context.Background(), plugins.InstallRequest{Name: "fake", Source: registry.PluginSourceBundled, Label: "dup"})
	if !errors.Is(err, registry.ErrConflict) {
		t.Errorf("duplicate label: %v", err)
	}
}

func TestManagerInstallCheckFails(t *testing.T) {
	m, _, _ := newManager(t)
	v, err := m.Install(context.Background(), plugins.InstallRequest{Name: "fake", Source: registry.PluginSourceBundled, Label: "crash", Config: map[string]any{"mode": fake.ModeCrashOnCheck}})
	var cf *plugins.CheckFailedError
	if !errors.As(err, &cf) {
		t.Fatalf("want *CheckFailedError, got %v", err)
	}
	if v == nil || v.Status != registry.PluginStatusError || v.StatusReason == "" {
		t.Fatalf("view after a failed check = %+v", v)
	}
	if v.Instance.State == plugins.StateRunning {
		t.Error("a failed plugin's instance should be stopped")
	}
	// The row stays, so the configuration can be fixed: set mode ok and
	// the plugin recovers.
	fixed, err := m.SetConfig(context.Background(), v.ID, map[string]any{"mode": fake.ModeOK})
	if err != nil {
		t.Fatalf("SetConfig after fix: %v", err)
	}
	if fixed.Status != registry.PluginStatusInstalled || fixed.Instance.State != plugins.StateRunning {
		t.Errorf("after fix = %+v", fixed)
	}
}

func TestManagerSetConfigKeepsOmittedSecret(t *testing.T) {
	m, store, _ := newManager(t)
	v := install(t, m, "cfg", map[string]any{"token": "s3cret", "greeting": "hello"})

	got, err := m.SetConfig(context.Background(), v.ID, map[string]any{"greeting": "bye"})
	if err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	if set, _ := got.Config["token"].(map[string]any); set["set"] != true || got.Config["greeting"] != "bye" {
		t.Errorf("omitted secret not kept: %v", got.Config)
	}
	row, err := store.GetPlugin(context.Background(), v.ID)
	if err != nil || row.Secrets["token"] != "s3cret" {
		t.Fatalf("stored secret = %v, %v", row.Secrets, err)
	}
	if got.Check == nil {
		t.Fatal("no check after reconfigure")
	}
	if _, ok := problems(*got.Check)[fake.ProblemTokenSet]; !ok {
		t.Error("the running plugin wasn't reconfigured with the kept token")
	}

	got, err = m.SetConfig(context.Background(), v.ID, map[string]any{"token": ""})
	if err != nil {
		t.Fatalf("SetConfig clear: %v", err)
	}
	if set, _ := got.Config["token"].(map[string]any); set["set"] != false {
		t.Errorf("cleared secret still set: %v", got.Config)
	}
	if _, ok := problems(*got.Check)[fake.ProblemTokenSet]; ok {
		t.Error("the plugin still has the cleared token")
	}
	if _, err := m.SetConfig(context.Background(), v.ID, map[string]any{"mode": "fly"}); err == nil {
		t.Error("an enum violation should be refused")
	}
}

func TestManagerDisableEnable(t *testing.T) {
	m, _, _ := newManager(t)
	v := install(t, m, "toggle", nil)
	d, err := m.Disable(context.Background(), v.ID)
	if err != nil || d.Status != registry.PluginStatusDisabled || d.Enabled || d.Instance.State != "" {
		t.Fatalf("Disable = %+v, %v", d, err)
	}
	if _, err := m.Check(context.Background(), v.ID); !errors.Is(err, plugins.ErrUnavailable) {
		t.Errorf("Check while disabled: %v", err)
	}
	e, err := m.Enable(context.Background(), v.ID)
	if err != nil || e.Status != registry.PluginStatusInstalled || !e.Enabled || e.Instance.State != plugins.StateRunning {
		t.Fatalf("Enable = %+v, %v", e, err)
	}
	if c, err := m.Check(context.Background(), v.ID); err != nil || c.Check == nil || !c.Check.OK {
		t.Errorf("Check = %+v, %v", c, err)
	}
}

func TestStartAllMissingBinary(t *testing.T) {
	bundle := t.TempDir()
	fakeDir(t, bundle)
	dbPath := filepath.Join(t.TempDir(), "m.db")
	store := newStore(t, dbPath)
	m := managerOn(t, store, bundle)
	v := install(t, m, "gone", nil)
	ok := install(t, m, "stays", nil)
	if err := m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()

	// The binary of "gone" is removed by making the whole bundle entry
	// unreadable for it: a second bundle with only a broken copy.
	if err := os.Remove(filepath.Join(bundle, "fake", plugins.ExecutableName("fake"))); err != nil {
		t.Fatal(err)
	}
	store2 := newStore(t, dbPath)
	m2 := managerOn(t, store2, bundle)
	err := m2.StartAll(context.Background())
	if err == nil {
		t.Fatal("StartAll should report the failure")
	}
	views, err := m2.List(context.Background())
	if err != nil || len(views) != 2 {
		t.Fatalf("List = %+v, %v", views, err)
	}
	for _, view := range views {
		if view.Status != registry.PluginStatusError || view.StatusReason == "" {
			t.Errorf("%s after StartAll without a binary = %s %q", view.Label, view.Status, view.StatusReason)
		}
		if view.AvailableVersion != "" {
			t.Errorf("%s should not be available any more", view.Label)
		}
	}
	_ = v
	_ = ok
	h := m2.Health(context.Background())
	if h.Status != "degraded" || len(h.Plugins) != 2 {
		t.Errorf("Health = %+v", h)
	}
}

func TestStartAllRestoresRunningPlugins(t *testing.T) {
	bundle := t.TempDir()
	fakeDir(t, bundle)
	dbPath := filepath.Join(t.TempDir(), "m.db")
	store := newStore(t, dbPath)
	m := managerOn(t, store, bundle)
	install(t, m, "a", map[string]any{"token": "t"})
	d := install(t, m, "b", nil)
	if _, err := m.Disable(context.Background(), d.ID); err != nil {
		t.Fatal(err)
	}
	_ = m.Close(context.Background())
	_ = store.Close()

	store2 := newStore(t, dbPath)
	m2 := managerOn(t, store2, bundle)
	if err := m2.StartAll(context.Background()); err != nil {
		t.Fatalf("StartAll: %v", err)
	}
	views, _ := m2.List(context.Background())
	byLabel := map[string]*plugins.View{}
	for _, v := range views {
		byLabel[v.Label] = v
	}
	if a := byLabel["a"]; a.Instance.State != plugins.StateRunning || a.Status != registry.PluginStatusInstalled {
		t.Errorf("a after restart = %+v", a)
	}
	if _, ok := problems(*byLabel["a"].Check)[fake.ProblemTokenSet]; !ok {
		t.Error("the secret didn't survive the restart")
	}
	if b := byLabel["b"]; b.Instance.State != "" || b.Status != registry.PluginStatusDisabled {
		t.Errorf("a disabled plugin was started: %+v", b)
	}
	if h := m2.Health(context.Background()); h.Status != "healthy" {
		t.Errorf("Health = %+v", h)
	}
}

func TestManagerUpgrade(t *testing.T) {
	m, store, bundle := newManager(t)
	v := install(t, m, "up", map[string]any{"token": "t"})
	same, err := m.Upgrade(context.Background(), v.ID)
	if err != nil || same.Version != "0.1.0" {
		t.Fatalf("Upgrade at the same version = %+v, %v", same, err)
	}
	// Bump the bundled manifest's version: the fake binary still reports
	// 0.1.0, so the handshake refuses it and the old row is marked.
	newer := strings.Replace(string(fake.ManifestJSON), `"version": "0.1.0"`, `"version": "0.2.0"`, 1)
	if err := os.WriteFile(filepath.Join(bundle, "fake", plugins.ManifestFile), []byte(newer), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := m.Upgrade(context.Background(), v.ID)
	if got == nil || got.AvailableVersion != "0.2.0" {
		t.Fatalf("Upgrade view = %+v, %v", got, err)
	}
	var cf *plugins.CheckFailedError
	if !errors.As(err, &cf) || !strings.Contains(cf.Reason, "0.2.0") {
		t.Errorf("a mismatching binary should fail the upgrade's start, naming the version: %v", err)
	}
	// The old version keeps running and the row keeps its version.
	if got.Version != "0.1.0" || got.Status != registry.PluginStatusInstalled || got.Instance.State != plugins.StateRunning {
		t.Errorf("after a failed upgrade = %+v", got)
	}
	row, _ := store.GetPlugin(context.Background(), v.ID)
	if row.Version != "0.1.0" || row.Secrets["token"] != "t" || row.Status != registry.PluginStatusInstalled {
		t.Errorf("row after a failed upgrade = %+v", row)
	}
	if c, err := m.Check(context.Background(), v.ID); err != nil || c.Check == nil || !c.Check.OK {
		t.Errorf("the old version should still answer: %+v, %v", c, err)
	}
}

func TestManagerUninstall(t *testing.T) {
	m, store, _ := newManager(t)
	v := install(t, m, "bye", map[string]any{"token": "t"})
	if err := m.Uninstall(context.Background(), v.ID, "sideways"); !errors.Is(err, plugins.ErrBadUninstallMode) {
		t.Errorf("bad mode: %v", err)
	}
	m.MachineCounter = func(ctx context.Context, id string) (int, error) { return 2, nil }
	var mode string
	m.UninstallMachines = func(ctx context.Context, id, mo string) error { mode = mo; return nil }
	err := m.Uninstall(context.Background(), v.ID, "")
	var hm *plugins.HasMachinesError
	if !errors.As(err, &hm) || hm.N != 2 {
		t.Fatalf("with machines and no choice: %v", err)
	}
	if err := m.Uninstall(context.Background(), v.ID, plugins.UninstallKeep); err != nil || mode != plugins.UninstallKeep {
		t.Fatalf("Uninstall keep: %v (mode %q)", err, mode)
	}
	if _, err := store.GetPlugin(context.Background(), v.ID); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("row after uninstall: %v", err)
	}
	if views, _ := m.List(context.Background()); len(views) != 0 {
		t.Errorf("List after uninstall = %+v", views)
	}
	if err := m.Uninstall(context.Background(), v.ID, ""); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("Uninstall twice: %v", err)
	}
}

// A plugin whose min_host_version this host doesn't meet is listed as
// incompatible and refused at install (LOOM-194).
func TestManagerInstallRefusesIncompatible(t *testing.T) {
	bundle := t.TempDir()
	dir := fakeDir(t, bundle)
	m := strings.Replace(string(fake.ManifestJSON), `"version": "0.1.0"`, `"version": "0.1.0", "min_host_version": "9.0.0"`, 1)
	if err := os.WriteFile(filepath.Join(dir, plugins.ManifestFile), []byte(m), 0o644); err != nil {
		t.Fatal(err)
	}
	store := newStore(t, filepath.Join(t.TempDir(), "m.db"))
	mgr := plugins.NewManager(store, &plugins.Catalog{BundleDir: bundle}, protocol.HostInfo{Version: "0.3.80 (web 0.3.12)", InstanceID: "inst"}, nil, nil)
	t.Cleanup(func() { _ = mgr.Close(context.Background()) })
	avail, err := mgr.Available(context.Background())
	if err != nil || len(avail) != 1 || !strings.Contains(avail[0].Incompatible, "needs Loomux 9.x") {
		t.Fatalf("Available = %+v, %v", avail, err)
	}
	_, err = mgr.Install(context.Background(), plugins.InstallRequest{Name: "fake", Source: registry.PluginSourceBundled, Label: "inc", Config: map[string]any{"token": "t"}})
	var inc *plugins.IncompatibleError
	if !errors.As(err, &inc) {
		t.Fatalf("Install = %v, want *IncompatibleError", err)
	}
	if rows, _ := store.ListPlugins(context.Background()); len(rows) != 0 {
		t.Errorf("a refused install left a row: %+v", rows)
	}
}

// A bundled plugin whose new version declares the same capabilities and
// configuration schema is adopted at startup, with no upgrade (D3): the
// row moves to the new version and the new binary runs.
func TestManagerAdoptsBundledVersionOnStart(t *testing.T) {
	m, store, bundle := newManager(t)
	v := install(t, m, "ad", map[string]any{"token": "t"})
	if err := m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	fakeDirVersion(t, bundle, "0.2.0", fakeBin2)
	m2 := managerOn(t, store, bundle)
	if err := m2.StartAll(context.Background()); err != nil {
		t.Fatalf("StartAll: %v", err)
	}
	got, err := m2.Get(context.Background(), v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != "0.2.0" || got.AvailableVersion != "0.2.0" || got.Status != registry.PluginStatusInstalled || got.Instance.State != plugins.StateRunning {
		t.Errorf("after start = version %s (available %s) %s %s", got.Version, got.AvailableVersion, got.Status, got.Instance.State)
	}
	row, _ := store.GetPlugin(context.Background(), v.ID)
	if row.Version != "0.2.0" || !strings.Contains(string(row.Manifest), `"0.2.0"`) || row.Secrets["token"] != "t" {
		t.Errorf("row = %+v", row)
	}
	if c, err := m2.Check(context.Background(), v.ID); err != nil || c.Check == nil || !c.Check.OK {
		t.Errorf("the adopted version should answer: %+v, %v", c, err)
	}
}

// A bundled plugin whose new version changed its configuration schema
// is not adopted: the row says so, nothing runs (the old binary is
// gone), and Upgrade moves it over.
func TestManagerBundledChangeNeedsUpgrade(t *testing.T) {
	m, store, bundle := newManager(t)
	v := install(t, m, "ch", map[string]any{"token": "t"})
	if err := m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	dir := fakeDirVersion(t, bundle, "0.2.0", fakeBin2)
	data, _ := os.ReadFile(filepath.Join(dir, plugins.ManifestFile))
	if strings.Count(string(data), `"properties": {`) != 1 {
		t.Fatal("the fake's schema changed shape; adjust the test")
	}
	changed := strings.Replace(string(data), `"properties": {`, `"properties": {"extra": {"type": "string", "default": "x"}, `, 1)
	// A default changes as well: the stored value that equalled the old
	// one follows on upgrade.
	changed = strings.Replace(changed, `"default": "hi"`, `"default": "hello"`, 1)
	if err := os.WriteFile(filepath.Join(dir, plugins.ManifestFile), []byte(changed), 0o644); err != nil {
		t.Fatal(err)
	}
	m2 := managerOn(t, store, bundle)
	if err := m2.StartAll(context.Background()); err == nil || !strings.Contains(err.Error(), "upgrade the plugin") {
		t.Errorf("StartAll = %v, want the upgrade reason", err)
	}
	got, err := m2.Get(context.Background(), v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != "0.1.0" || got.Status != registry.PluginStatusError || got.Instance.State != "" ||
		!strings.Contains(got.StatusReason, "0.2.0") || !strings.Contains(got.StatusReason, "upgrade the plugin") {
		t.Errorf("after start = version %s %s %q instance %q", got.Version, got.Status, got.StatusReason, got.Instance.State)
	}
	up, err := m2.Upgrade(context.Background(), v.ID)
	if err != nil || up.Version != "0.2.0" || up.Status != registry.PluginStatusInstalled || up.Instance.State != plugins.StateRunning {
		t.Fatalf("Upgrade = %+v, %v", up, err)
	}
	if up.Config["extra"] != "x" || up.Config["greeting"] != "hello" {
		t.Errorf("the new setting's default wasn't applied, or the changed default not followed: %+v", up.Config)
	}
}

// A bundled version whose only schema change is a default (the stamped
// agent pin after an agent rebuild) is adopted, and a stored setting
// that still equals the old default follows the new one; a setting the
// operator set stays (review of #352).
func TestManagerAdoptionFollowsAChangedDefault(t *testing.T) {
	m, store, bundle := newManager(t)
	kept := install(t, m, "kept", map[string]any{"token": "t"})
	custom := install(t, m, "custom", map[string]any{"token": "t", "greeting": "yo"})
	if kept.Config["greeting"] != "hi" {
		t.Fatalf("install didn't store the default: %+v", kept.Config)
	}
	if err := m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	dir := fakeDirVersion(t, bundle, "0.2.0", fakeBin2)
	data, _ := os.ReadFile(filepath.Join(dir, plugins.ManifestFile))
	if strings.Count(string(data), `"default": "hi"`) != 1 {
		t.Fatal("the fake's greeting default moved; adjust the test")
	}
	changed := strings.Replace(string(data), `"default": "hi"`, `"default": "hello"`, 1)
	if err := os.WriteFile(filepath.Join(dir, plugins.ManifestFile), []byte(changed), 0o644); err != nil {
		t.Fatal(err)
	}
	m2 := managerOn(t, store, bundle)
	if err := m2.StartAll(context.Background()); err != nil {
		t.Fatalf("StartAll: %v", err)
	}
	for _, tc := range []struct{ id, label, greeting string }{{kept.ID, "kept", "hello"}, {custom.ID, "custom", "yo"}} {
		got, err := m2.Get(context.Background(), tc.id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Version != "0.2.0" || got.Instance.State != plugins.StateRunning || got.Config["greeting"] != tc.greeting {
			t.Errorf("%s: version %s %s greeting %v (want %s)", tc.label, got.Version, got.Instance.State, got.Config["greeting"], tc.greeting)
		}
		row, _ := store.GetPlugin(context.Background(), tc.id)
		if row.Config["greeting"] != tc.greeting || row.Secrets["token"] != "t" {
			t.Errorf("%s row: %+v %v", tc.label, row.Config, row.Secrets)
		}
	}
}

// A sidecar (socket) plugin whose image was bumped is adopted at start
// the same way, from what it describes itself as (review of #352).
func TestManagerAdoptsSocketVersionOnStart(t *testing.T) {
	dir := shortTempDir(t)
	sock := filepath.Join(dir, "fake.sock")
	stop := serveFakeSocket(t, sock)
	store := newStore(t, filepath.Join(t.TempDir(), "m.db"))
	m := plugins.NewManager(store, &plugins.Catalog{SocketDir: dir}, protocol.HostInfo{Version: "test", InstanceID: "inst"}, nil, nil)
	v, err := m.Install(context.Background(), plugins.InstallRequest{Name: "fake", Source: registry.PluginSourceSocket, Label: "side", Config: map[string]any{"token": "t"}})
	if err != nil || v.Version != "0.1.0" || v.Instance.State != plugins.StateRunning {
		t.Fatalf("Install from the socket = %+v, %v", v, err)
	}
	if err := m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	stop()
	// The sidecar comes back as a newer image.
	fake.Version = "0.2.0"
	t.Cleanup(func() { fake.Version = "" })
	stop2 := serveFakeSocket(t, sock)
	t.Cleanup(stop2)
	m2 := plugins.NewManager(store, &plugins.Catalog{SocketDir: dir}, protocol.HostInfo{Version: "test", InstanceID: "inst"}, nil, nil)
	t.Cleanup(func() { _ = m2.Close(context.Background()) })
	if err := m2.StartAll(context.Background()); err != nil {
		t.Fatalf("StartAll: %v", err)
	}
	got, err := m2.Get(context.Background(), v.ID)
	if err != nil || got.Version != "0.2.0" || got.Status != registry.PluginStatusInstalled || got.Instance.State != plugins.StateRunning {
		t.Errorf("after start = %+v, %v; want 0.2.0 running", got, err)
	}
}

// A plugin built for an older 0.Y than this host's runs: listed as
// incompatible with the reason, warned about, but installed and
// started (user decision 2026-10-11).
func TestManagerRunsAnOlderMinorWithAWarning(t *testing.T) {
	bundle := t.TempDir()
	dir := fakeDir(t, bundle)
	m := strings.Replace(string(fake.ManifestJSON), `"version": "0.1.0"`, `"version": "0.1.0", "min_host_version": "0.3.0"`, 1)
	if err := os.WriteFile(filepath.Join(dir, plugins.ManifestFile), []byte(m), 0o644); err != nil {
		t.Fatal(err)
	}
	store := newStore(t, filepath.Join(t.TempDir(), "m.db"))
	mgr := plugins.NewManager(store, &plugins.Catalog{BundleDir: bundle}, protocol.HostInfo{Version: "0.4.2 (web 0.4.0)", InstanceID: "inst"}, nil, nil)
	t.Cleanup(func() { _ = mgr.Close(context.Background()) })
	avail, err := mgr.Available(context.Background())
	if err != nil || len(avail) != 1 || avail[0].Incompatible != "" || !strings.Contains(avail[0].Warning, "was built for Loomux 0.3.x") {
		t.Fatalf("Available = %+v, %v; want a warning, no refusal", avail, err)
	}
	v, err := mgr.Install(context.Background(), plugins.InstallRequest{Name: "fake", Source: registry.PluginSourceBundled, Label: "older", Config: map[string]any{"token": "t"}})
	if err != nil || v.Status != registry.PluginStatusInstalled || v.Instance.State != plugins.StateRunning {
		t.Fatalf("Install = %+v, %v; want installed and running", v, err)
	}
	if err := mgr.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	mgr2 := plugins.NewManager(store, &plugins.Catalog{BundleDir: bundle}, protocol.HostInfo{Version: "0.4.2 (web 0.4.0)", InstanceID: "inst"}, nil, nil)
	t.Cleanup(func() { _ = mgr2.Close(context.Background()) })
	if err := mgr2.StartAll(context.Background()); err != nil {
		t.Fatalf("StartAll: %v", err)
	}
	if got, err := mgr2.Get(context.Background(), v.ID); err != nil || got.Status != registry.PluginStatusInstalled || got.Instance.State != plugins.StateRunning {
		t.Errorf("after start = %+v, %v; want running", got, err)
	}
}

// A sidecar whose new image was built for an older 0.Y than this host
// is adopted all the same, with the warning (the host refuses only
// below the minimum or across majors).
func TestManagerAdoptsSocketBuiltForAnOlderMinor(t *testing.T) {
	dir := shortTempDir(t)
	sock := filepath.Join(dir, "fake.sock")
	host := protocol.HostInfo{Version: "0.4.2 (web 0.4.0)", InstanceID: "inst"}
	stop := serveFakeSocket(t, sock)
	store := newStore(t, filepath.Join(t.TempDir(), "m.db"))
	m := plugins.NewManager(store, &plugins.Catalog{SocketDir: dir}, host, nil, nil)
	v, err := m.Install(context.Background(), plugins.InstallRequest{Name: "fake", Source: registry.PluginSourceSocket, Label: "side", Config: map[string]any{"token": "t"}})
	if err != nil {
		t.Fatalf("Install from the socket: %v", err)
	}
	if err := m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	stop()
	fake.Version, fake.MinHostVersion = "0.2.0", "0.3.0"
	t.Cleanup(func() { fake.Version, fake.MinHostVersion = "", "" })
	stop2 := serveFakeSocket(t, sock)
	t.Cleanup(stop2)
	m2 := plugins.NewManager(store, &plugins.Catalog{SocketDir: dir}, host, nil, nil)
	t.Cleanup(func() { _ = m2.Close(context.Background()) })
	avail, err := m2.Available(context.Background())
	if err != nil || len(avail) != 1 || avail[0].Incompatible != "" || !strings.Contains(avail[0].Warning, "was built for Loomux 0.3.x") {
		t.Fatalf("Available = %+v, %v", avail, err)
	}
	if err := m2.StartAll(context.Background()); err != nil {
		t.Fatalf("StartAll: %v", err)
	}
	got, err := m2.Get(context.Background(), v.ID)
	if err != nil || got.Version != "0.2.0" || got.Status != registry.PluginStatusInstalled || got.Instance.State != plugins.StateRunning {
		t.Errorf("after start = %+v, %v; want 0.2.0 running", got, err)
	}
}

// A sidecar that wasn't listening when the server started, with the
// socket wait off, is in error; when it comes up on a new version,
// Enable (the retry) adopts it like a start does, so neither Upgrade
// nor a server restart is needed (review of #352).
func TestManagerEnableAdoptsALateSidecar(t *testing.T) {
	dir := shortTempDir(t)
	sock := filepath.Join(dir, "fake.sock")
	host := protocol.HostInfo{Version: "test", InstanceID: "inst"}
	stop := serveFakeSocket(t, sock)
	store := newStore(t, filepath.Join(t.TempDir(), "m.db"))
	m := plugins.NewManager(store, &plugins.Catalog{SocketDir: dir}, host, nil, nil)
	v, err := m.Install(context.Background(), plugins.InstallRequest{Name: "fake", Source: registry.PluginSourceSocket, Label: "side", Config: map[string]any{"token": "t"}})
	if err != nil {
		t.Fatalf("Install from the socket: %v", err)
	}
	if err := m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	stop()
	// The server comes up before the sidecar does.
	m2 := plugins.NewManager(store, &plugins.Catalog{SocketDir: dir}, host, nil, nil)
	m2.SocketWait = 0
	t.Cleanup(func() { _ = m2.Close(context.Background()) })
	if err := m2.StartAll(context.Background()); err == nil {
		t.Fatal("a start without the sidecar should fail")
	}
	if got, _ := m2.Get(context.Background(), v.ID); got.Status != registry.PluginStatusError {
		t.Fatalf("without the sidecar = %+v", got)
	}
	// Now it listens, on a new version.
	fake.Version = "0.2.0"
	t.Cleanup(func() { fake.Version = "" })
	stop2 := serveFakeSocket(t, sock)
	t.Cleanup(stop2)
	got, err := m2.Enable(context.Background(), v.ID)
	if err != nil || got.Version != "0.2.0" || got.Status != registry.PluginStatusInstalled || got.Instance.State != plugins.StateRunning {
		t.Errorf("Enable = version %s %s %q instance %q, %v; want 0.2.0 running", got.Version, got.Status, got.StatusReason, got.Instance.State, err)
	}
}

// installSidecar installs the fake from a sidecar socket in dir, then
// stops the sidecar and the Manager: the next Manager on store starts
// it again from the row.
func installSidecar(t *testing.T, store registry.Store, dir string) *plugins.View {
	t.Helper()
	stop := serveFakeSocket(t, filepath.Join(dir, "fake.sock"))
	m := plugins.NewManager(store, &plugins.Catalog{SocketDir: dir}, protocol.HostInfo{Version: "test", InstanceID: "inst"}, nil, nil)
	v, err := m.Install(context.Background(), plugins.InstallRequest{Name: "fake", Source: registry.PluginSourceSocket, Label: "side", Config: map[string]any{"token": "t"}})
	if err != nil {
		t.Fatalf("Install from the socket: %v", err)
	}
	if err := m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	stop()
	return v
}

// waitingManager is a Manager on store whose socket waits retry fast.
func waitingManager(t *testing.T, store registry.Store, dir string, wait time.Duration) *plugins.Manager {
	t.Helper()
	m := plugins.NewManager(store, &plugins.Catalog{SocketDir: dir}, protocol.HostInfo{Version: "test", InstanceID: "inst"}, nil, nil)
	m.SocketWait, m.SocketWaitBackoff, m.SocketWaitMaxBackoff = wait, 10*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	return m
}

// eventuallyView polls plugin id's view until ok, or fails the test.
func eventuallyView(t *testing.T, m *plugins.Manager, id string, ok func(*plugins.View) bool) *plugins.View {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		v, err := m.Get(context.Background(), id)
		if err == nil && ok(v) {
			return v
		}
		if time.Now().After(deadline) {
			t.Fatalf("view never got there: %+v, %v", v, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A sidecar that isn't listening yet when the server starts is waited
// for (#465): the instance says restarting, waiting for the socket, not
// error; once the sidecar comes up, on a new version here, it is
// adopted, started and checked with nobody calling Enable.
func TestManagerWaitsForALateSidecar(t *testing.T) {
	dir := shortTempDir(t)
	store := newStore(t, filepath.Join(t.TempDir(), "m.db"))
	v := installSidecar(t, store, dir)

	m := waitingManager(t, store, dir, time.Minute)
	if err := m.StartAll(context.Background()); err != nil {
		t.Fatalf("StartAll without the sidecar yet: %v", err)
	}
	got, err := m.Get(context.Background(), v.ID)
	if err != nil || got.Status != registry.PluginStatusInstalled || got.Instance.State != plugins.StateRestarting ||
		got.Instance.Reason != plugins.ReasonWaitingForSocket || m.Running(v.ID) {
		t.Fatalf("while waiting = %s %q instance %+v, %v", got.Status, got.StatusReason, got.Instance, err)
	}
	if h := m.Health(context.Background()); h.Status != "degraded" || h.Plugins["side"].Reason != plugins.ReasonWaitingForSocket {
		t.Errorf("health while waiting = %+v", h)
	}
	if err := m.Call(context.Background(), v.ID, protocol.MethodCheck, nil, nil); !errors.Is(err, plugins.ErrUnavailable) {
		t.Errorf("call while waiting = %v", err)
	}
	// Retries without the socket keep waiting.
	time.Sleep(100 * time.Millisecond)
	if got, _ := m.Get(context.Background(), v.ID); got.Instance.Reason != plugins.ReasonWaitingForSocket {
		t.Fatalf("after a few retries = %s instance %+v", got.Status, got.Instance)
	}

	fake.Version = "0.2.0"
	t.Cleanup(func() { fake.Version = "" })
	t.Cleanup(serveFakeSocket(t, filepath.Join(dir, "fake.sock")))
	// Running comes a moment before the check's result is recorded.
	got = eventuallyView(t, m, v.ID, func(v *plugins.View) bool {
		return v.Instance.State == plugins.StateRunning && v.Check != nil && v.Status != registry.PluginStatusInstalling
	})
	if got.Version != "0.2.0" || got.Status != registry.PluginStatusInstalled || got.Check == nil || !got.Check.OK {
		t.Errorf("once the sidecar is up = version %s %s %q check %+v", got.Version, got.Status, got.StatusReason, got.Check)
	}
	var serving plugins.Manifest
	if err := m.Call(context.Background(), v.ID, protocol.MethodDescribe, nil, &serving); err != nil || serving.Version != "0.2.0" {
		t.Errorf("serving = %q, %v", serving.Version, err)
	}
}

// A sidecar that doesn't come up within SocketWait fails as before the
// wait existed; Enable then waits again, the row moving from error to
// installing, and the sidecar coming up finishes the install.
func TestManagerSocketWaitRunsOut(t *testing.T) {
	dir := shortTempDir(t)
	store := newStore(t, filepath.Join(t.TempDir(), "m.db"))
	v := installSidecar(t, store, dir)

	m := waitingManager(t, store, dir, 200*time.Millisecond)
	if err := m.StartAll(context.Background()); err != nil {
		t.Fatalf("StartAll: %v", err)
	}
	got := eventuallyView(t, m, v.ID, func(v *plugins.View) bool { return v.Status == registry.PluginStatusError })
	if !strings.Contains(got.StatusReason, "the sidecar's socket wasn't there after 200ms") ||
		!strings.Contains(got.StatusReason, "no such file or directory") || got.Instance.State != "" {
		t.Errorf("after the wait = %q instance %+v", got.StatusReason, got.Instance)
	}

	m.SocketWait = time.Minute
	got, err := m.Enable(context.Background(), v.ID)
	if err != nil || got.Status != registry.PluginStatusInstalling || got.Instance.Reason != plugins.ReasonWaitingForSocket {
		t.Fatalf("Enable without the sidecar = %s %q instance %+v, %v", got.Status, got.StatusReason, got.Instance, err)
	}
	t.Cleanup(serveFakeSocket(t, filepath.Join(dir, "fake.sock")))
	// Running comes a moment before the check's result is recorded.
	got = eventuallyView(t, m, v.ID, func(v *plugins.View) bool {
		return v.Instance.State == plugins.StateRunning && v.Check != nil && v.Status != registry.PluginStatusInstalling
	})
	if got.Status != registry.PluginStatusInstalled {
		t.Errorf("once the sidecar is up = %s %q", got.Status, got.StatusReason)
	}
}

// Only a socket that isn't there yet is waited for: one that answers
// but fails the handshake fails the start at once.
func TestManagerSocketHandshakeFailureDoesNotWait(t *testing.T) {
	dir := shortTempDir(t)
	store := newStore(t, filepath.Join(t.TempDir(), "m.db"))
	v := installSidecar(t, store, dir)
	// Something listens on the socket and hangs up on every connection.
	sock := filepath.Join(dir, "fake.sock")
	_ = os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	m := waitingManager(t, store, dir, time.Minute)
	if err := m.StartAll(context.Background()); err == nil {
		t.Fatal("a handshake that fails should fail the start")
	}
	got, err := m.Get(context.Background(), v.ID)
	if err != nil || got.Status != registry.PluginStatusError || got.Instance.State != "" {
		t.Errorf("after a failed handshake = %s %q instance %+v, %v", got.Status, got.StatusReason, got.Instance, err)
	}
}

// Disabling a plugin that waits for its socket ends the wait: the
// sidecar coming up later doesn't start it.
func TestManagerDisableEndsSocketWait(t *testing.T) {
	dir := shortTempDir(t)
	store := newStore(t, filepath.Join(t.TempDir(), "m.db"))
	v := installSidecar(t, store, dir)

	m := waitingManager(t, store, dir, time.Minute)
	if err := m.StartAll(context.Background()); err != nil {
		t.Fatalf("StartAll: %v", err)
	}
	got, err := m.Disable(context.Background(), v.ID)
	if err != nil || got.Status != registry.PluginStatusDisabled || got.Instance.State != "" {
		t.Fatalf("Disable while waiting = %s instance %+v, %v", got.Status, got.Instance, err)
	}
	t.Cleanup(serveFakeSocket(t, filepath.Join(dir, "fake.sock")))
	time.Sleep(200 * time.Millisecond)
	if got, _ := m.Get(context.Background(), v.ID); got.Status != registry.PluginStatusDisabled || got.Instance.State != "" || m.Running(v.ID) {
		t.Errorf("after the sidecar came up = %s instance %+v", got.Status, got.Instance)
	}
}

// An Upgrade that starts the plugin while it waits for its socket ends
// the wait: the wait's deadline passing later must not fail the plugin
// the Upgrade started (review of #485).
func TestManagerUpgradeEndsSocketWait(t *testing.T) {
	dir := shortTempDir(t)
	store := newStore(t, filepath.Join(t.TempDir(), "m.db"))
	v := installSidecar(t, store, dir)

	// Retries far enough apart that the Upgrade comes between two.
	const wait = time.Second
	m := waitingManager(t, store, dir, wait)
	m.SocketWaitBackoff, m.SocketWaitMaxBackoff = 500*time.Millisecond, 500*time.Millisecond
	if err := m.StartAll(context.Background()); err != nil {
		t.Fatalf("StartAll: %v", err)
	}
	fake.Version = "0.2.0"
	t.Cleanup(func() { fake.Version = "" })
	t.Cleanup(serveFakeSocket(t, filepath.Join(dir, "fake.sock")))
	up, err := m.Upgrade(context.Background(), v.ID)
	if err != nil || up.Version != "0.2.0" || up.Instance.State != plugins.StateRunning {
		t.Fatalf("Upgrade while waiting = version %s instance %+v, %v", up.Version, up.Instance, err)
	}
	time.Sleep(2 * wait)
	got, err := m.Get(context.Background(), v.ID)
	if err != nil || got.Status != registry.PluginStatusInstalled || got.Instance.State != plugins.StateRunning || !m.Running(v.ID) {
		t.Errorf("after the wait's deadline = %s %q instance %+v, %v", got.Status, got.StatusReason, got.Instance, err)
	}
}

// A start after Close doesn't begin a socket wait: nothing would end it
// (review of #485).
func TestManagerClosedDoesNotWaitForSocket(t *testing.T) {
	dir := shortTempDir(t)
	store := newStore(t, filepath.Join(t.TempDir(), "m.db"))
	v := installSidecar(t, store, dir)

	m := waitingManager(t, store, dir, time.Minute)
	if err := m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := m.StartAll(context.Background()); err == nil {
		t.Error("a start after Close should fail")
	}
	got, err := m.Get(context.Background(), v.ID)
	if err != nil || got.Instance.Reason == plugins.ReasonWaitingForSocket {
		t.Errorf("after Close = %s instance %+v, %v; want no wait", got.Status, got.Instance, err)
	}
}

// failingStore fails SetPluginConfig or UpdatePlugin while armed.
type failingStore struct {
	registry.Store
	failConfig, failRow atomic.Bool
}

var errStoreDown = errors.New("disk full")

func (s *failingStore) SetPluginConfig(ctx context.Context, id string, config map[string]any, secrets map[string]string) error {
	if s.failConfig.Load() {
		return errStoreDown
	}
	return s.Store.SetPluginConfig(ctx, id, config, secrets)
}

func (s *failingStore) UpdatePlugin(ctx context.Context, p *registry.Plugin) error {
	if s.failRow.Load() {
		return errStoreDown
	}
	return s.Store.UpdatePlugin(ctx, p)
}

// An upgrade whose new version checks out but can't be stored, its
// configuration or its row, fails and leaves the old version serving
// and stored as it was (LOOM-245); the next upgrade goes through.
func TestManagerUpgradeStoreFailure(t *testing.T) {
	for _, write := range []string{"config", "row"} {
		t.Run(write, func(t *testing.T) {
			ctx := context.Background()
			bundle := t.TempDir()
			fakeDir(t, bundle)
			store := &failingStore{Store: newStore(t, filepath.Join(t.TempDir(), "m.db"))}
			m := managerOn(t, store, bundle)
			v := install(t, m, "up", map[string]any{"token": "t"})
			dir := fakeDirVersion(t, bundle, "0.2.0", fakeBin2)
			// The new version adds a setting, so its configuration
			// differs from the stored one.
			data, _ := os.ReadFile(filepath.Join(dir, plugins.ManifestFile))
			changed := strings.Replace(string(data), `"properties": {`, `"properties": {"extra": {"type": "string", "default": "x"}, `, 1)
			if err := os.WriteFile(filepath.Join(dir, plugins.ManifestFile), []byte(changed), 0o644); err != nil {
				t.Fatal(err)
			}
			if write == "config" {
				store.failConfig.Store(true)
			} else {
				store.failRow.Store(true)
			}
			if _, err := m.Upgrade(ctx, v.ID); !errors.Is(err, errStoreDown) {
				t.Fatalf("Upgrade with the %s write failing = %v, want the store's error", write, err)
			}
			store.failConfig.Store(false)
			store.failRow.Store(false)

			var serving plugins.Manifest
			if err := m.Call(ctx, v.ID, protocol.MethodDescribe, nil, &serving); err != nil || serving.Version != "0.1.0" {
				t.Errorf("serving after the failed upgrade = %q, %v; want the old 0.1.0", serving.Version, err)
			}
			row, err := store.GetPlugin(ctx, v.ID)
			if err != nil {
				t.Fatal(err)
			}
			if row.Version != "0.1.0" || row.Status != registry.PluginStatusInstalled || row.Secrets["token"] != "t" {
				t.Errorf("row after the failed upgrade = version %s %s secrets %v", row.Version, row.Status, row.Secrets)
			}
			if _, ok := row.Config["extra"]; ok {
				t.Errorf("the new version's configuration was stored: %v", row.Config)
			}

			up, err := m.Upgrade(ctx, v.ID)
			if err != nil || up.Version != "0.2.0" || up.Instance.State != plugins.StateRunning || up.Config["extra"] != "x" {
				t.Fatalf("Upgrade once the store is back = %+v, %v", up, err)
			}
			if err := m.Call(ctx, v.ID, protocol.MethodDescribe, nil, &serving); err != nil || serving.Version != "0.2.0" {
				t.Errorf("serving after the upgrade = %q, %v", serving.Version, err)
			}
		})
	}
}
