package plugins_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
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
