package api_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Loomux/server/api"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/registry/sqlite"
	"github.com/Loomux/server/router/llmrouter"
)

// fakeProvider is an OpenAI-compatible endpoint: it answers a chat
// completion, or, with reject set, a 401 that echoes the presented key
// back, as some providers do.
type fakeProvider struct {
	mu     sync.Mutex
	reject bool
	keys   []string
}

func (p *fakeProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	reject := p.reject
	p.keys = append(p.keys, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	p.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if reject {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"Incorrect API key provided: ` + r.Header.Get("Authorization") + `","type":"invalid_request_error"}}`))
		return
	}
	if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/models") {
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"model-b","object":"model","created":1,"owned_by":"x"},{"id":"model-a","object":"model","created":1,"owned_by":"x"}]}`))
		return
	}
	_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"length","message":{"role":"assistant","content":"p"}}]}`))
}

func (p *fakeProvider) lastKey() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.keys[len(p.keys)-1]
}

type routerSettingsFixture struct {
	base, token string
	model       *llmrouter.Model
	provider    *fakeProvider
	providerURL string
	store       registry.Store
}

func routerSettingsServer(t *testing.T, masterKey []byte) routerSettingsFixture {
	t.Helper()
	var opts []sqlite.Option
	if masterKey != nil {
		opts = append(opts, sqlite.WithMasterKey(masterKey))
	}
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "router.db"), opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	provider := &fakeProvider{}
	psrv := httptest.NewServer(provider)
	t.Cleanup(psrv.Close)
	env := llmrouter.Config{Primary: llmrouter.Tier{BaseURL: psrv.URL, APIKey: "env-primary-key-000000", Model: "env-model"}}
	model, err := llmrouter.New(env, nil)
	if err != nil {
		t.Fatal(err)
	}
	settings, err := llmrouter.NewSettings(t.Context(), store, model, env)
	if err != nil {
		t.Fatal(err)
	}
	srv, _, _ := newTestServerOn(t, store, func(registry.Store) []api.Option { return []api.Option{api.WithRouterSettings(settings)} })
	token, _ := login(t, srv.URL, testPassword)
	return routerSettingsFixture{base: srv.URL, token: token, model: model, provider: provider, providerURL: psrv.URL, store: store}
}

type routerTierJSON struct {
	Tier             string  `json:"tier"`
	Source           string  `json:"source"`
	Provider         string  `json:"provider"`
	BaseURL          string  `json:"base_url"`
	Model            string  `json:"model"`
	KeyFingerprint   string  `json:"key_fingerprint"`
	KeyLast4         string  `json:"key_last4"`
	SetAt            *string `json:"set_at"`
	EnvConfigured    bool    `json:"env_configured"`
	StoredUnreadable bool    `json:"stored_unreadable"`
}

type routerSettingsJSON struct {
	Providers []string         `json:"providers"`
	Tiers     []routerTierJSON `json:"tiers"`
}

// captureLogs sends the default logger to a buffer for the test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// LOOM-185: the owner sets, rotates, tests and removes a tier's key and
// model. The key goes in and never comes out — not in any response, the
// audit trail, an error, or a log line — and the router uses it at once.
func TestRouterSettings_WriteOnlyKeyAndHotReload(t *testing.T) {
	logs := captureLogs(t)
	f := routerSettingsServer(t, bytes.Repeat([]byte("k"), 32))
	const key1, key2 = "sk-stored-FIRST-key-0123456789", "sk-stored-SECOND-key-9876543210"
	var bodies []string
	call := func(method, path string, body any, want int) string {
		t.Helper()
		status, out := doJSON(t, method, f.base+path, f.token, body)
		if status != want {
			t.Fatalf("%s %s = %d %s, want %d", method, path, status, out, want)
		}
		bodies = append(bodies, out)
		return out
	}

	var got routerSettingsJSON
	_ = json.Unmarshal([]byte(call("GET", "/api/v1/settings/router", nil, 200)), &got)
	if strings.Join(got.Providers, ",") != "openai,anthropic" || len(got.Tiers) != 2 {
		t.Fatalf("GET = %+v", got)
	}
	if p := got.Tiers[0]; p.Tier != "primary" || p.Source != "env" || p.Model != "env-model" || p.KeyLast4 != "0000" ||
		!strings.HasPrefix(p.KeyFingerprint, "sha256:") || p.SetAt != nil || !p.EnvConfigured {
		t.Fatalf("env primary = %+v", p)
	}
	if e := got.Tiers[1]; e.Tier != "escalation" || e.Source != "none" || e.EnvConfigured {
		t.Fatalf("escalation = %+v", e)
	}

	var tier routerTierJSON
	_ = json.Unmarshal([]byte(call("PUT", "/api/v1/settings/router/primary",
		map[string]string{"base_url": f.providerURL, "model": "stored-model", "api_key": key1}, 200)), &tier)
	if tier.Source != "stored" || tier.Model != "stored-model" || tier.Provider != "openai" || tier.KeyLast4 != "6789" ||
		tier.KeyFingerprint != llmrouter.KeyFingerprint(key1) || tier.SetAt == nil {
		t.Fatalf("PUT = %+v", tier)
	}
	if c := f.model.Config().Primary; c.APIKey != key1 || c.Model != "stored-model" {
		t.Fatalf("router not reloaded: %+v", c.Model)
	}

	type testResult struct {
		OK         bool   `json:"ok"`
		Status     int    `json:"status"`
		ErrorClass string `json:"error_class"`
		Error      string `json:"error"`
		Model      string `json:"model"`
		Source     string `json:"source"`
	}
	var res testResult
	_ = json.Unmarshal([]byte(call("POST", "/api/v1/settings/router/primary/test", nil, 200)), &res)
	if !res.OK || res.Source != "stored" || res.Model != "stored-model" || f.provider.lastKey() != key1 {
		t.Fatalf("test = %+v (provider saw the stored key: %v)", res, f.provider.lastKey() == key1)
	}

	// Rotate: model unchanged, new key; then change the model keeping it.
	call("PUT", "/api/v1/settings/router/primary", map[string]string{"base_url": f.providerURL, "model": "stored-model", "api_key": key2}, 200)
	call("PUT", "/api/v1/settings/router/primary", map[string]string{"base_url": f.providerURL, "model": "stored-model-2"}, 200)
	// Not to a new endpoint, though: the saved key doesn't follow it.
	call("PUT", "/api/v1/settings/router/primary", map[string]string{"base_url": "https://elsewhere.example/v1", "model": "stored-model-2"}, 400)
	if c := f.model.Config().Primary; c.APIKey != key2 || c.Model != "stored-model-2" {
		t.Fatalf("after rotate: model %q, key rotated %v", c.Model, c.APIKey == key2)
	}

	f.provider.mu.Lock()
	f.provider.reject = true
	f.provider.mu.Unlock()
	res = testResult{}
	_ = json.Unmarshal([]byte(call("POST", "/api/v1/settings/router/primary/test", nil, 200)), &res)
	if res.OK || res.Status != http.StatusUnauthorized || res.ErrorClass != "auth_failed" || res.Error == "" || strings.Contains(res.Error, "Incorrect") {
		t.Fatalf("failing test = %+v", res)
	}

	var audit struct {
		Entries []struct {
			Tier   string   `json:"tier"`
			Action string   `json:"action"`
			Fields []string `json:"fields"`
			Actor  string   `json:"actor"`
		} `json:"entries"`
	}
	call("DELETE", "/api/v1/settings/router/primary", nil, 204)
	_ = json.Unmarshal([]byte(call("GET", "/api/v1/settings/router/audit", nil, 200)), &audit)
	if len(audit.Entries) != 4 || audit.Entries[0].Action != "clear" || strings.Join(audit.Entries[1].Fields, ",") != "model" ||
		strings.Join(audit.Entries[2].Fields, ",") != "api_key" || len(audit.Entries[3].Fields) != 4 ||
		!strings.HasPrefix(audit.Entries[0].Actor, "session:") {
		t.Fatalf("audit = %+v", audit.Entries)
	}
	_ = json.Unmarshal([]byte(call("GET", "/api/v1/settings/router/audit?limit=1", nil, 200)), &audit)
	if len(audit.Entries) != 1 {
		t.Fatalf("audit limit=1 = %d entries", len(audit.Entries))
	}
	if c := f.model.Config().Primary; c.Model != "env-model" {
		t.Fatalf("after DELETE: model %q, want the env's", c.Model)
	}
	_ = json.Unmarshal([]byte(call("GET", "/api/v1/settings/router", nil, 200)), &got)
	if got.Tiers[0].Source != "env" {
		t.Fatalf("after DELETE: %+v", got.Tiers[0])
	}

	for _, secret := range []string{key1, key2, "FIRST-key", "SECOND-key", "env-primary-key-000000"} {
		for i, b := range bodies {
			if strings.Contains(b, secret) {
				t.Errorf("response %d contains a key (%q): %s", i, secret, b)
			}
		}
		if strings.Contains(logs.String(), secret) {
			t.Errorf("a log line contains a key (%q): %s", secret, logs.String())
		}
	}
}

// Everything is behind auth; bad input is 400, an unknown tier 404, no
// master key 503, and nothing to clear or test 404.
func TestRouterSettings_Errors(t *testing.T) {
	f := routerSettingsServer(t, bytes.Repeat([]byte("k"), 32))
	for _, r := range []struct{ method, path string }{
		{"GET", "/api/v1/settings/router"}, {"GET", "/api/v1/settings/router/audit"},
		{"PUT", "/api/v1/settings/router/primary"}, {"DELETE", "/api/v1/settings/router/primary"},
		{"POST", "/api/v1/settings/router/primary/test"},
	} {
		if status, _ := doJSON(t, r.method, f.base+r.path, "", map[string]string{}); status != http.StatusUnauthorized {
			t.Errorf("%s %s without a token = %d", r.method, r.path, status)
		}
	}
	for _, c := range []struct {
		method, path string
		body         any
		want         int
	}{
		{"PUT", "/api/v1/settings/router/primary", map[string]string{"base_url": "ftp://x", "model": "m", "api_key": "a-long-enough-key"}, 400},
		{"PUT", "/api/v1/settings/router/primary", map[string]string{"base_url": "http://api.example.com/v1", "model": "m", "api_key": "a-long-enough-key"}, 400},
		{"PUT", "/api/v1/settings/router/primary", map[string]string{"base_url": f.providerURL, "model": "m"}, 400},
		{"PUT", "/api/v1/settings/router/primary", map[string]string{"provider": "acme", "base_url": f.providerURL, "model": "m", "api_key": "a-long-enough-key"}, 400},
		{"PUT", "/api/v1/settings/router/primary", "not an object", 400},
		{"PUT", "/api/v1/settings/router/tertiary", map[string]string{"base_url": f.providerURL, "model": "m", "api_key": "a-long-enough-key"}, 404},
		{"DELETE", "/api/v1/settings/router/primary", nil, 404},
		{"DELETE", "/api/v1/settings/router/audit", nil, 404},
		{"POST", "/api/v1/settings/router/escalation/test", nil, 404},
		{"POST", "/api/v1/settings/router/tertiary/test", nil, 404},
		{"GET", "/api/v1/settings/router/audit?limit=0", nil, 400},
		{"GET", "/api/v1/settings/router/audit?limit=201", nil, 400},
	} {
		status, body := doJSON(t, c.method, f.base+c.path, f.token, c.body)
		if status != c.want {
			t.Errorf("%s %s %v = %d %s, want %d", c.method, c.path, c.body, status, body, c.want)
		}
		if strings.Contains(body, "a-long-enough-key") {
			t.Errorf("%s %s error echoes the key: %s", c.method, c.path, body)
		}
	}

	none := routerSettingsServer(t, nil)
	status, body := doJSON(t, "PUT", none.base+"/api/v1/settings/router/primary", none.token,
		map[string]string{"base_url": none.providerURL, "model": "m", "api_key": "a-long-enough-key"})
	if status != http.StatusServiceUnavailable || !strings.Contains(body, "LOOMUX_MASTER_KEY") {
		t.Fatalf("PUT without a master key = %d %s", status, body)
	}
	if status, _ := doJSON(t, "GET", none.base+"/api/v1/settings/router", none.token, nil); status != http.StatusOK {
		t.Fatalf("GET without a master key = %d", status)
	}

	srv, _, _ := newTestServer(t)
	token, _ := login(t, srv.URL, testPassword)
	if status, _ := doJSON(t, "GET", srv.URL+"/api/v1/settings/router", token, nil); status != http.StatusNotFound {
		t.Fatalf("GET on a server without router settings = %d", status)
	}
}

// LOOM-191: the model picker's list, with the saved key or one being
// entered, which never comes back in the answer.
func TestRouterSettings_ListModels(t *testing.T) {
	logs := captureLogs(t)
	f := routerSettingsServer(t, bytes.Repeat([]byte("k"), 32))
	type modelsJSON struct {
		OK         bool                `json:"ok"`
		Models     []map[string]string `json:"models"`
		Status     int                 `json:"status"`
		ErrorClass string              `json:"error_class"`
		Cached     bool                `json:"cached"`
	}
	list := func(body any, want int) (modelsJSON, string) {
		t.Helper()
		status, out := doJSON(t, "POST", f.base+"/api/v1/settings/router/primary/models", f.token, body)
		if status != want {
			t.Fatalf("POST models %v = %d %s, want %d", body, status, out, want)
		}
		var got modelsJSON
		_ = json.Unmarshal([]byte(out), &got)
		return got, out
	}

	got, _ := list(nil, http.StatusOK)
	if !got.OK || len(got.Models) != 2 || got.Models[0]["id"] != "model-a" || got.Models[1]["id"] != "model-b" || got.Cached {
		t.Fatalf("saved tier's models = %+v", got)
	}
	if f.provider.lastKey() != "env-primary-key-000000" {
		t.Fatal("not listed with the tier's key")
	}
	if got, _ = list(map[string]string{}, http.StatusOK); !got.Cached {
		t.Fatalf("second listing not cached: %+v", got)
	}

	const entered = "sk-being-entered-0123456789"
	got, out := list(map[string]string{"base_url": f.providerURL + "/", "api_key": entered}, http.StatusOK)
	if !got.OK || f.provider.lastKey() != entered {
		t.Fatalf("entered key listing = %+v", got)
	}
	if strings.Contains(out, entered) || strings.Contains(logs.String(), entered) {
		t.Fatalf("the entered key came back: %s", out)
	}

	// The saved key isn't sent to another base URL.
	list(map[string]string{"base_url": "https://elsewhere.example/v1"}, http.StatusBadRequest)
	list(map[string]string{"base_url": "http://elsewhere.example/v1", "api_key": entered}, http.StatusBadRequest)

	f.provider.mu.Lock()
	f.provider.reject = true
	f.provider.mu.Unlock()
	got, out = list(map[string]string{"base_url": f.providerURL, "api_key": "sk-rejected-0123456789"}, http.StatusOK)
	if got.OK || got.Status != http.StatusUnauthorized || got.ErrorClass != "auth_failed" || strings.Contains(out, "Incorrect") || strings.Contains(out, "rejected") {
		t.Fatalf("rejected listing = %s", out)
	}

	status, _ := doJSON(t, "POST", f.base+"/api/v1/settings/router/escalation/models", f.token, nil)
	if status != http.StatusNotFound {
		t.Fatalf("unconfigured escalation = %d", status)
	}
	if status, _ := doJSON(t, "POST", f.base+"/api/v1/settings/router/primary/models", "", nil); status != http.StatusUnauthorized {
		t.Fatalf("without a token = %d", status)
	}
}
