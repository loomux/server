package storetest

import (
	"context"
	"errors"
	"testing"

	"github.com/Loomux/server/registry"
)

// Plugins and settings (LOOM-178).

func newPlugin(label string) *registry.Plugin {
	return &registry.Plugin{
		ID:           "plugin-" + label,
		Name:         "fake",
		Label:        label,
		Version:      "0.1.0",
		Protocol:     "loomux-plugin/1",
		Source:       registry.PluginSourceBundled,
		Path:         "/usr/local/lib/loomux/plugins/fake",
		Trust:        registry.PluginTrustBundled,
		Status:       registry.PluginStatusInstalling,
		Enabled:      true,
		Capabilities: []string{"targets.create"},
		Manifest:     []byte(`{"name":"fake","version":"0.1.0"}`),
		Config:       map[string]any{"greeting": "hi", "sizes": map[string]any{"small": map[string]any{"cpu": "1"}}},
		Secrets:      map[string]string{"token": "s3cret"},
	}
}

func testPluginCRUD(t *testing.T, store registry.Store) {
	ctx := context.Background()
	p := newPlugin("one")
	if err := store.CreatePlugin(ctx, p); err != nil {
		t.Fatalf("CreatePlugin: %v", err)
	}
	if p.InstalledAt.IsZero() || p.UpdatedAt.IsZero() {
		t.Fatal("CreatePlugin should stamp InstalledAt and UpdatedAt")
	}

	got, err := store.GetPlugin(ctx, p.ID)
	if err != nil {
		t.Fatalf("GetPlugin: %v", err)
	}
	if got.Label != "one" || got.Name != "fake" || got.Status != registry.PluginStatusInstalling || !got.Enabled {
		t.Errorf("GetPlugin = %+v", got)
	}
	if got.Secrets["token"] != "s3cret" {
		t.Errorf("GetPlugin should decrypt secrets, got %v", got.Secrets)
	}
	if got.Config["greeting"] != "hi" {
		t.Errorf("config not round-tripped: %v", got.Config)
	}
	sizes, _ := got.Config["sizes"].(map[string]any)
	small, _ := sizes["small"].(map[string]any)
	if small["cpu"] != "1" {
		t.Errorf("nested config not round-tripped: %v", got.Config)
	}
	if len(got.Capabilities) != 1 || got.Capabilities[0] != "targets.create" {
		t.Errorf("capabilities = %v", got.Capabilities)
	}
	if string(got.Manifest) != `{"name":"fake","version":"0.1.0"}` {
		t.Errorf("manifest = %s", got.Manifest)
	}

	list, err := store.ListPlugins(ctx)
	if err != nil {
		t.Fatalf("ListPlugins: %v", err)
	}
	if len(list) != 1 || list[0].ID != p.ID {
		t.Fatalf("ListPlugins = %+v", list)
	}
	if list[0].Secrets != nil {
		t.Error("ListPlugins must not decrypt secrets")
	}
	if list[0].Config["greeting"] != "hi" {
		t.Error("ListPlugins should carry the plain config")
	}

	got.Status = registry.PluginStatusError
	got.StatusReason = "check failed"
	got.Enabled = false
	got.Version = "0.2.0"
	got.Capabilities = []string{}
	got.Manifest = []byte(`{"name":"fake","version":"0.2.0"}`)
	got.Config["greeting"] = "changed through UpdatePlugin, which must not write config"
	if err := store.UpdatePlugin(ctx, got); err != nil {
		t.Fatalf("UpdatePlugin: %v", err)
	}
	again, err := store.GetPlugin(ctx, p.ID)
	if err != nil {
		t.Fatalf("GetPlugin after update: %v", err)
	}
	if again.Status != registry.PluginStatusError || again.StatusReason != "check failed" || again.Enabled || again.Version != "0.2.0" {
		t.Errorf("UpdatePlugin not applied: %+v", again)
	}
	if len(again.Capabilities) != 0 || string(again.Manifest) != `{"name":"fake","version":"0.2.0"}` {
		t.Errorf("capabilities/manifest after update = %v %s", again.Capabilities, again.Manifest)
	}
	if again.Config["greeting"] != "hi" || again.Secrets["token"] != "s3cret" {
		t.Error("UpdatePlugin must leave config and secrets alone")
	}
	if !again.UpdatedAt.After(p.UpdatedAt) && !again.UpdatedAt.Equal(p.UpdatedAt) {
		t.Error("UpdatedAt should not go backwards")
	}

	if err := store.DeletePlugin(ctx, p.ID); err != nil {
		t.Fatalf("DeletePlugin: %v", err)
	}
	if _, err := store.GetPlugin(ctx, p.ID); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("GetPlugin after delete: want ErrNotFound, got %v", err)
	}
	if err := store.DeletePlugin(ctx, p.ID); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("DeletePlugin twice: want ErrNotFound, got %v", err)
	}
	if err := store.UpdatePlugin(ctx, p); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("UpdatePlugin unknown: want ErrNotFound, got %v", err)
	}
}

func testPluginDuplicateLabel(t *testing.T, store registry.Store) {
	ctx := context.Background()
	if err := store.CreatePlugin(ctx, newPlugin("dup")); err != nil {
		t.Fatalf("CreatePlugin: %v", err)
	}
	second := newPlugin("dup")
	second.ID = "plugin-dup-2"
	if err := store.CreatePlugin(ctx, second); !errors.Is(err, registry.ErrConflict) {
		t.Fatalf("duplicate label: want ErrConflict, got %v", err)
	}
}

func testPluginConfigRoundTrip(t *testing.T, store registry.Store) {
	ctx := context.Background()
	p := newPlugin("cfg")
	if err := store.CreatePlugin(ctx, p); err != nil {
		t.Fatalf("CreatePlugin: %v", err)
	}
	if err := store.SetPluginConfig(ctx, p.ID, map[string]any{"greeting": "hello", "n": float64(3)}, map[string]string{"token": "new", "kubeconfig": "yaml"}); err != nil {
		t.Fatalf("SetPluginConfig: %v", err)
	}
	got, err := store.GetPlugin(ctx, p.ID)
	if err != nil {
		t.Fatalf("GetPlugin: %v", err)
	}
	if got.Config["greeting"] != "hello" || got.Config["n"] != float64(3) || len(got.Config) != 2 {
		t.Errorf("config = %v", got.Config)
	}
	if got.Secrets["token"] != "new" || got.Secrets["kubeconfig"] != "yaml" || len(got.Secrets) != 2 {
		t.Errorf("secrets = %v", got.Secrets)
	}
	// Clearing every secret stores no blob at all.
	if err := store.SetPluginConfig(ctx, p.ID, map[string]any{}, nil); err != nil {
		t.Fatalf("SetPluginConfig clear: %v", err)
	}
	got, err = store.GetPlugin(ctx, p.ID)
	if err != nil {
		t.Fatalf("GetPlugin after clear: %v", err)
	}
	if len(got.Secrets) != 0 || len(got.Config) != 0 {
		t.Errorf("after clear: config %v secrets %v", got.Config, got.Secrets)
	}
	if err := store.SetPluginConfig(ctx, "nope", nil, nil); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("SetPluginConfig unknown: want ErrNotFound, got %v", err)
	}
}

// testPluginSecretsNeedMasterKey runs on a store opened without a master
// key: secrets can't be stored, plain configuration can.
func testPluginSecretsNeedMasterKey(t *testing.T, store registry.Store) {
	ctx := context.Background()
	p := newPlugin("nokey")
	if err := store.CreatePlugin(ctx, p); !errors.Is(err, registry.ErrNoMasterKey) {
		t.Fatalf("CreatePlugin with secrets and no key: want ErrNoMasterKey, got %v", err)
	}
	p.Secrets = nil
	if err := store.CreatePlugin(ctx, p); err != nil {
		t.Fatalf("CreatePlugin without secrets: %v", err)
	}
	if err := store.SetPluginConfig(ctx, p.ID, map[string]any{"a": "b"}, map[string]string{"token": "x"}); !errors.Is(err, registry.ErrNoMasterKey) {
		t.Errorf("SetPluginConfig with secrets and no key: want ErrNoMasterKey, got %v", err)
	}
	got, err := store.GetPlugin(ctx, p.ID)
	if err != nil {
		t.Fatalf("GetPlugin: %v", err)
	}
	if got.Config["greeting"] != "hi" {
		t.Errorf("config = %v", got.Config)
	}
}

func testSettings(t *testing.T, store registry.Store) {
	ctx := context.Background()
	if _, err := store.GetSetting(ctx, registry.SettingInstanceID); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("GetSetting unset: want ErrNotFound, got %v", err)
	}
	if err := store.SetSetting(ctx, registry.SettingInstanceID, "abc"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	v, err := store.GetSetting(ctx, registry.SettingInstanceID)
	if err != nil || v != "abc" {
		t.Fatalf("GetSetting = %q, %v", v, err)
	}
	if err := store.SetSetting(ctx, registry.SettingInstanceID, "def"); err != nil {
		t.Fatalf("SetSetting again: %v", err)
	}
	if v, _ := store.GetSetting(ctx, registry.SettingInstanceID); v != "def" {
		t.Errorf("GetSetting after overwrite = %q", v)
	}
}
