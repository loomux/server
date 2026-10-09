package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/Loomux/server/api"
	"github.com/Loomux/server/plugins"
	"github.com/Loomux/server/plugins/protocol"
	"github.com/Loomux/server/registry"
)

// stubPlugins scripts the manager: what each verb returns, and what it
// was given.
type stubPlugins struct {
	available []plugins.Available
	views     map[string]*plugins.View
	err       error // returned by every verb when set
	errView   *plugins.View

	lastInstall   plugins.InstallRequest
	lastConfig    map[string]any
	lastUninstall string
}

func (s *stubPlugins) view(id string) (*plugins.View, error) {
	if s.err != nil {
		return s.errView, s.err
	}
	v, ok := s.views[id]
	if !ok {
		return nil, registry.ErrNotFound
	}
	return v, nil
}

func (s *stubPlugins) Available(ctx context.Context) ([]plugins.Available, error) {
	return s.available, nil
}
func (s *stubPlugins) Install(ctx context.Context, r plugins.InstallRequest) (*plugins.View, error) {
	s.lastInstall = r
	if s.err != nil {
		return s.errView, s.err
	}
	return s.views["p1"], nil
}
func (s *stubPlugins) Get(ctx context.Context, id string) (*plugins.View, error) { return s.view(id) }
func (s *stubPlugins) List(ctx context.Context) ([]*plugins.View, error) {
	out := []*plugins.View{}
	for _, v := range s.views {
		out = append(out, v)
	}
	return out, nil
}
func (s *stubPlugins) SetConfig(ctx context.Context, id string, config map[string]any) (*plugins.View, error) {
	s.lastConfig = config
	return s.view(id)
}
func (s *stubPlugins) Enable(ctx context.Context, id string) (*plugins.View, error)  { return s.view(id) }
func (s *stubPlugins) Disable(ctx context.Context, id string) (*plugins.View, error) { return s.view(id) }
func (s *stubPlugins) Upgrade(ctx context.Context, id string) (*plugins.View, error) { return s.view(id) }
func (s *stubPlugins) Check(ctx context.Context, id string) (*plugins.View, error)   { return s.view(id) }
func (s *stubPlugins) Uninstall(ctx context.Context, id, targets string) error {
	s.lastUninstall = targets
	if s.err != nil {
		return s.err
	}
	if _, ok := s.views[id]; !ok {
		return registry.ErrNotFound
	}
	return nil
}

func fakeView() *plugins.View {
	return &plugins.View{
		Plugin: registry.Plugin{
			ID: "p1", Name: "fake", Label: "one", Version: "0.1.0", Protocol: plugins.ProtocolV1,
			Source: registry.PluginSourceBundled, Path: "/usr/local/lib/loomux/plugins/fake", Trust: registry.PluginTrustBundled,
			Status: registry.PluginStatusInstalled, Enabled: true, Capabilities: []string{},
			Config: map[string]any{"greeting": "hi", "token": map[string]any{"set": true}},
		},
		AvailableVersion: "0.1.0",
		Check:            &protocol.CheckResult{OK: true, Problems: []protocol.Problem{{Code: "token_set", Message: "a token is configured", Severity: "warning"}}},
		Instance:         plugins.InstanceStatus{State: plugins.StateRunning},
	}
}

func pluginServer(t *testing.T) (baseURL, token string, stub *stubPlugins) {
	t.Helper()
	stub = &stubPlugins{views: map[string]*plugins.View{"p1": fakeView()}}
	m := plugins.Manifest{Name: "fake", Title: "Fake", Version: "0.1.0", Protocol: plugins.ProtocolV1, Vendor: "Loomux",
		Capabilities: []string{}, Permissions: []plugins.Permission{{Scope: "none", Detail: "nothing"}},
		ConfigSchema: json.RawMessage(`{"type":"object","properties":{"token":{"type":"string","x-secret":true}}}`)}
	stub.available = []plugins.Available{{Manifest: m, Source: registry.PluginSourceBundled, Path: "/bundle/fake", Trust: registry.PluginTrustBundled, Isolation: plugins.IsolationNone}}
	srv, _, _ := newTestServer(t, api.WithPlugins(stub))
	token, _ = login(t, srv.URL, testPassword)
	return srv.URL, token, stub
}

func TestPlugins_NotConfigured(t *testing.T) {
	srv, _, _ := newTestServer(t)
	token, _ := login(t, srv.URL, testPassword)
	for _, r := range [][2]string{{"GET", "/api/v1/plugins"}, {"GET", "/api/v1/plugins/available"}, {"POST", "/api/v1/plugins/p1/enable"}, {"DELETE", "/api/v1/plugins/p1"}} {
		status, body := doJSON(t, r[0], srv.URL+r[1], token, nil)
		if status != http.StatusNotFound || !strings.Contains(body, "not available") {
			t.Errorf("%s %s without plugins: %d %s", r[0], r[1], status, body)
		}
	}
}

func TestPlugins_Unauthenticated(t *testing.T) {
	base, _, _ := pluginServer(t)
	if status, _ := doJSON(t, "GET", base+"/api/v1/plugins", "", nil); status != http.StatusUnauthorized {
		t.Errorf("unauthenticated: %d", status)
	}
}

func TestPlugins_AvailableAndList(t *testing.T) {
	base, token, _ := pluginServer(t)
	status, body := doJSON(t, "GET", base+"/api/v1/plugins/available", token, nil)
	if status != http.StatusOK {
		t.Fatalf("available: %d %s", status, body)
	}
	var avail struct {
		Plugins []struct {
			Name, Source, Trust, Isolation string
			Installed                      []string
			ConfigSchema                   map[string]any `json:"config_schema"`
			Permissions                    []map[string]string
		}
	}
	if err := json.Unmarshal([]byte(body), &avail); err != nil || len(avail.Plugins) != 1 {
		t.Fatalf("available body: %s (%v)", body, err)
	}
	a := avail.Plugins[0]
	if a.Name != "fake" || a.Source != "bundled" || a.Trust != "bundled" || a.Isolation != "none" || len(a.Installed) != 1 || a.Installed[0] != "p1" || a.ConfigSchema["type"] != "object" || a.Permissions[0]["scope"] != "none" {
		t.Errorf("available entry = %+v", a)
	}

	status, body = doJSON(t, "GET", base+"/api/v1/plugins", token, nil)
	if status != http.StatusOK || !strings.Contains(body, `"label":"one"`) || !strings.Contains(body, `"instance":"running"`) || !strings.Contains(body, `"token":{"set":true}`) {
		t.Fatalf("list: %d %s", status, body)
	}
	if !strings.Contains(body, `"check":{"ok":true,"problems":[{"code":"token_set"`) {
		t.Errorf("check not rendered: %s", body)
	}
}

func TestPlugins_Install(t *testing.T) {
	base, token, stub := pluginServer(t)
	status, body := doJSON(t, "POST", base+"/api/v1/plugins", token, map[string]any{"plugin": "fake", "label": "one", "config": map[string]any{"token": "s3cret"}})
	if status != http.StatusCreated || !strings.Contains(body, `"id":"p1"`) {
		t.Fatalf("install: %d %s", status, body)
	}
	if strings.Contains(body, "s3cret") {
		t.Error("the response carries the secret")
	}
	if stub.lastInstall.Source != registry.PluginSourceBundled || stub.lastInstall.Label != "one" || stub.lastInstall.Config["token"] != "s3cret" {
		t.Errorf("install request = %+v", stub.lastInstall)
	}
	if status, body := doJSON(t, "POST", base+"/api/v1/plugins", token, map[string]any{"plugin": "fake", "label": "x", "source": "registry"}); status != http.StatusBadRequest {
		t.Errorf("bad source: %d %s", status, body)
	}

	cases := []struct {
		err    error
		status int
		text   string
	}{
		{plugins.ErrNotAvailable, http.StatusNotFound, "available"},
		{&plugins.ConfigError{Field: "colour", Msg: "isn't a setting of this plugin"}, http.StatusBadRequest, "config.colour"},
		{plugins.ErrBadLabel, http.StatusBadRequest, "label"},
		{registry.ErrConflict, http.StatusConflict, "already installed"},
		{registry.ErrNoMasterKey, http.StatusServiceUnavailable, "LOOMUX_MASTER_KEY"},
		{errors.New("boom"), http.StatusInternalServerError, "failed"},
	}
	for _, c := range cases {
		stub.err = c.err
		status, body := doJSON(t, "POST", base+"/api/v1/plugins", token, map[string]any{"plugin": "fake", "label": "x"})
		if status != c.status || !strings.Contains(body, c.text) {
			t.Errorf("%v: %d %s", c.err, status, body)
		}
	}
	// A plugin that installed but can't work: 422 with the plugin.
	failed := fakeView()
	failed.Status, failed.StatusReason = registry.PluginStatusError, "check: the plugin crashed"
	failed.Instance = plugins.InstanceStatus{}
	stub.err, stub.errView = &plugins.CheckFailedError{Reason: "check: the plugin crashed"}, failed
	status, body = doJSON(t, "POST", base+"/api/v1/plugins", token, map[string]any{"plugin": "fake", "label": "x"})
	if status != http.StatusUnprocessableEntity || !strings.Contains(body, `"status":"error"`) || !strings.Contains(body, "crashed") {
		t.Errorf("check failed: %d %s", status, body)
	}
}

func TestPlugins_GetConfigAndLifecycle(t *testing.T) {
	base, token, stub := pluginServer(t)
	if status, body := doJSON(t, "GET", base+"/api/v1/plugins/p1", token, nil); status != http.StatusOK || !strings.Contains(body, `"label":"one"`) {
		t.Errorf("get: %d %s", status, body)
	}
	if status, _ := doJSON(t, "GET", base+"/api/v1/plugins/nope", token, nil); status != http.StatusNotFound {
		t.Errorf("get unknown: %d", status)
	}

	// TestSetPluginConfigKeepsOmittedSecret: the request's config goes
	// to the manager as given (the manager keeps omitted secrets), and
	// the answer never carries a value.
	status, body := doJSON(t, "PUT", base+"/api/v1/plugins/p1/config", token, map[string]any{"config": map[string]any{"greeting": "bye"}})
	if status != http.StatusOK || strings.Contains(body, "s3cret") {
		t.Errorf("set config: %d %s", status, body)
	}
	if _, given := stub.lastConfig["token"]; given || stub.lastConfig["greeting"] != "bye" {
		t.Errorf("config passed to the manager = %v", stub.lastConfig)
	}
	status, _ = doJSON(t, "PUT", base+"/api/v1/plugins/p1/config", token, map[string]any{"config": map[string]any{"token": ""}})
	if status != http.StatusOK || stub.lastConfig["token"] != "" {
		t.Errorf("clearing a secret: %d %v", status, stub.lastConfig)
	}

	for _, verb := range []string{"enable", "disable", "upgrade", "check"} {
		if status, body := doJSON(t, "POST", base+"/api/v1/plugins/p1/"+verb, token, nil); status != http.StatusOK || !strings.Contains(body, `"id":"p1"`) {
			t.Errorf("%s: %d %s", verb, status, body)
		}
		if status, _ := doJSON(t, "POST", base+"/api/v1/plugins/nope/"+verb, token, nil); status != http.StatusNotFound {
			t.Errorf("%s unknown: %d", verb, status)
		}
	}
	stub.err = &plugins.CapabilityDroppedError{Capability: "targets.create"}
	if status, body := doJSON(t, "POST", base+"/api/v1/plugins/p1/upgrade", token, nil); status != http.StatusConflict || !strings.Contains(body, "targets.create") {
		t.Errorf("upgrade dropping a capability: %d %s", status, body)
	}
	stub.err = plugins.ErrUnavailable
	if status, _ := doJSON(t, "POST", base+"/api/v1/plugins/p1/check", token, nil); status != http.StatusConflict {
		t.Errorf("check while not running: %d", status)
	}
}

func TestPlugins_Uninstall(t *testing.T) {
	base, token, stub := pluginServer(t)
	if status, _ := doJSON(t, "DELETE", base+"/api/v1/plugins/p1?targets=keep", token, nil); status != http.StatusNoContent || stub.lastUninstall != "keep" {
		t.Errorf("uninstall keep: %d %q", status, stub.lastUninstall)
	}
	if status, _ := doJSON(t, "DELETE", base+"/api/v1/plugins/nope", token, nil); status != http.StatusNotFound {
		t.Errorf("uninstall unknown: %d", status)
	}
	stub.err = plugins.ErrBadUninstallMode
	if status, body := doJSON(t, "DELETE", base+"/api/v1/plugins/p1?targets=sideways", token, nil); status != http.StatusBadRequest || !strings.Contains(body, "destroy or keep") {
		t.Errorf("bad mode: %d %s", status, body)
	}
	stub.err = &plugins.HasMachinesError{N: 2}
	if status, body := doJSON(t, "DELETE", base+"/api/v1/plugins/p1", token, nil); status != http.StatusConflict || !strings.Contains(body, "2 machine") {
		t.Errorf("has machines: %d %s", status, body)
	}
}
