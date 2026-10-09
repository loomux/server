package app

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/Loomux/server/plugins"
	"github.com/Loomux/server/plugins/fake"
	"github.com/Loomux/server/registry"
)

// buildFakePlugin builds cmd/loomux-plugin-fake into a bundle directory
// laid out as the image's.
func buildFakePlugin(t *testing.T) string {
	t.Helper()
	bundle := t.TempDir()
	dir := filepath.Join(bundle, "fake")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	build := exec.Command("go", "build", "-o", filepath.Join(dir, plugins.ExecutableName("fake")), "github.com/Loomux/server/cmd/loomux-plugin-fake")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("building the fake plugin: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, plugins.ManifestFile), fake.ManifestJSON, 0o644); err != nil {
		t.Fatal(err)
	}
	return bundle
}

// LOOM-178: the app builds the plugin system, installed plugins start
// again with it, the deep health report has a plugins component, and
// the instance id survives restarts.
func TestBuild_PluginsStartWithTheApp(t *testing.T) {
	bundle := buildFakePlugin(t)
	srv := fakeRouterServer(t)
	cfg := testConfig(t, srv.URL)
	cfg.MasterKey = []byte("01234567890123456789012345678901")
	cfg.PluginBundleDir = bundle
	ctx := context.Background()

	app, err := Build(cfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	firstID, err := app.Store().GetSetting(ctx, registry.SettingInstanceID)
	if err != nil || firstID == "" {
		t.Fatalf("instance id after first build: %q, %v", firstID, err)
	}
	v, err := app.Plugins().Install(ctx, plugins.InstallRequest{Name: "fake", Source: registry.PluginSourceBundled, Label: "one", Config: map[string]any{"token": "t"}})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if v.Status != registry.PluginStatusInstalled {
		t.Fatalf("installed view = %+v", v)
	}
	report := app.HealthChecker().Deep(ctx)
	comp, ok := report.Components["plugins"]
	if !ok || comp.Status != "healthy" {
		t.Errorf("deep health plugins component = %+v (present %v)", comp, ok)
	}
	if err := app.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	app2, err := Build(cfg)
	if err != nil {
		t.Fatalf("second Build: %v", err)
	}
	defer app2.Close()
	if id, _ := app2.Store().GetSetting(ctx, registry.SettingInstanceID); id != firstID {
		t.Errorf("instance id changed across restarts: %q then %q", firstID, id)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		views, err := app2.Plugins().List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(views) == 1 && views[0].Instance.State == plugins.StateRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the installed plugin didn't start with the app: %+v", views)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
