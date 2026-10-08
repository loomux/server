package llmrouter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Loomux/server/credentials"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/registry/sqlite"
	"github.com/Loomux/server/router"
)

func settingsStore(t *testing.T) *sqlite.Store {
	t.Helper()
	s, err := sqlite.Open(filepath.Join(t.TempDir(), "settings.db"), sqlite.WithMasterKey(bytes.Repeat([]byte("k"), 32)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// modelServer answers every Decide with answer_directly naming which
// server it is, and records the bearer keys and models it was called
// with.
type modelServer struct {
	url    string
	calls  atomic.Int32
	mu     sync.Mutex
	keys   []string
	models []string
}

func newModelServer(t *testing.T, name string) *modelServer {
	t.Helper()
	ms := &modelServer{}
	answer := toolCallHandler(t, decideToolName, map[string]any{"action": "answer_directly", "direct_answer": name})
	srv, _ := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		ms.calls.Add(1)
		var body struct {
			Model string `json:"model"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		ms.mu.Lock()
		ms.keys = append(ms.keys, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		ms.models = append(ms.models, body.Model)
		ms.mu.Unlock()
		answer(w, r)
	})
	ms.url = srv.URL
	return ms
}

func (ms *modelServer) last() (key, model string) {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	return ms.keys[len(ms.keys)-1], ms.models[len(ms.models)-1]
}

func decideAnswer(t *testing.T, m *Model) string {
	t.Helper()
	dec, err := m.Decide(context.Background(), "hi", nil, nil)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	return dec.DirectAnswer
}

func newSettingsFor(t *testing.T, store SettingsStore, env Config) (*Settings, *Model) {
	t.Helper()
	m, err := New(env, nil)
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewSettings(context.Background(), store, m, env)
	if err != nil {
		t.Fatal(err)
	}
	return s, m
}

func tierView(t *testing.T, s *Settings, name string) TierView {
	t.Helper()
	for _, v := range s.Tiers() {
		if v.Tier == name {
			return v
		}
	}
	t.Fatalf("no tier %q", name)
	return TierView{}
}

// LOOM-185: a stored tier overrides the env's at once (no restart), the
// view says which source is active, and clearing it goes back to the
// env. The stored key is what the provider is called with.
func TestSettings_PrecedenceAndHotReload(t *testing.T) {
	envSrv, storedSrv := newModelServer(t, "env"), newModelServer(t, "stored")
	store := settingsStore(t)
	env := Config{Primary: Tier{BaseURL: envSrv.url, APIKey: "env-key-0000000000", Model: "env-model"}}
	s, m := newSettingsFor(t, store, env)

	if v := tierView(t, s, "primary"); v.Source != SourceEnv || v.Model != "env-model" || v.Provider != string(ProviderOpenAI) ||
		v.KeyFingerprint != KeyFingerprint("env-key-0000000000") || v.KeyLast4 != "0000" || !v.SetAt.IsZero() || !v.EnvConfigured {
		t.Fatalf("env view = %+v", v)
	}
	if v := tierView(t, s, "escalation"); v.Source != SourceNone || v.EnvConfigured || v.Model != "" {
		t.Fatalf("escalation view = %+v", v)
	}
	if got := decideAnswer(t, m); got != "env" {
		t.Fatalf("before: answered by %q", got)
	}

	v, err := s.Set(context.Background(), "primary", TierUpdate{BaseURL: storedSrv.url, Model: "stored-model", APIKey: "stored-key-123456789"}, "session:a")
	if err != nil {
		t.Fatal(err)
	}
	if v.Source != SourceStored || v.Model != "stored-model" || v.KeyLast4 != "6789" || v.SetAt.IsZero() {
		t.Fatalf("stored view = %+v", v)
	}
	if got := decideAnswer(t, m); got != "stored" {
		t.Fatalf("after Set: answered by %q, want the stored tier without a restart", got)
	}
	if key, model := storedSrv.last(); key != "stored-key-123456789" || model != "stored-model" {
		t.Fatalf("stored tier called with key %q model %q", key, model)
	}

	// A restart loads the stored tier over the env again.
	_, m2 := newSettingsFor(t, store, env)
	if got := decideAnswer(t, m2); got != "stored" {
		t.Fatalf("after restart: answered by %q", got)
	}

	if err := s.Clear(context.Background(), "primary", "session:b"); err != nil {
		t.Fatal(err)
	}
	if got := decideAnswer(t, m); got != "env" {
		t.Fatalf("after Clear: answered by %q, want the env tier", got)
	}
	if v := tierView(t, s, "primary"); v.Source != SourceEnv {
		t.Fatalf("after Clear: %+v", v)
	}
	if err := s.Clear(context.Background(), "primary", "session:b"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("Clear with nothing stored = %v", err)
	}
}

// An escalation tier stored with no env escalation turns escalation on;
// clearing it turns it off again.
func TestSettings_StoredEscalation(t *testing.T) {
	failing, _ := newFakeServer(t, errorHandler(http.StatusBadRequest))
	escSrv := newModelServer(t, "escalation")
	s, m := newSettingsFor(t, settingsStore(t), Config{Primary: Tier{BaseURL: failing.URL, APIKey: "env-key-0000000000", Model: "p"}})
	if _, err := m.Decide(context.Background(), "hi", nil, nil); err == nil {
		t.Fatal("Decide succeeded with a failing primary and no escalation")
	}
	if _, err := s.Set(context.Background(), "escalation", TierUpdate{BaseURL: escSrv.url, Model: "e", APIKey: "esc-key-0000000000"}, "a"); err != nil {
		t.Fatal(err)
	}
	if got := decideAnswer(t, m); got != "escalation" {
		t.Fatalf("answered by %q", got)
	}
	if err := s.Clear(context.Background(), "escalation", "a"); err != nil {
		t.Fatal(err)
	}
	if m.Config().Escalation != nil {
		t.Fatal("escalation still on after Clear with no env escalation")
	}
}

// An update without api_key keeps the stored key; with no stored key it
// is refused. Only changed fields are audited, a no-op not at all, and
// never with a value.
func TestSettings_KeepKeyAndAudit(t *testing.T) {
	srv := newModelServer(t, "x")
	store := settingsStore(t)
	s, m := newSettingsFor(t, store, Config{Primary: Tier{BaseURL: srv.url, APIKey: "env-key-0000000000", Model: "env-model"}})
	ctx := context.Background()

	var invalid *InvalidSettingError
	if _, err := s.Set(ctx, "primary", TierUpdate{BaseURL: srv.url, Model: "m1"}, "a"); !errors.As(err, &invalid) {
		t.Fatalf("Set without a key and none stored = %v", err)
	}
	if _, err := s.Set(ctx, "primary", TierUpdate{BaseURL: srv.url, Model: "m1", APIKey: "first-key-00000000"}, "session:a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Set(ctx, "primary", TierUpdate{BaseURL: srv.url, Model: "m2"}, "session:b"); err != nil {
		t.Fatal(err)
	}
	if got := m.Config().Primary; got.APIKey != "first-key-00000000" || got.Model != "m2" {
		t.Fatalf("after a keyless update: %+v", got)
	}
	if _, err := s.Set(ctx, "primary", TierUpdate{Provider: "openai", BaseURL: srv.url, Model: "m2"}, "session:c"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Set(ctx, "primary", TierUpdate{BaseURL: srv.url, Model: "m2", APIKey: "second-key-0000000"}, "session:d"); err != nil {
		t.Fatal(err)
	}
	changes, err := s.Changes(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 3 {
		t.Fatalf("changes = %d, want 3 (the no-op unaudited)", len(changes))
	}
	want := [][]string{{"api_key"}, {"model"}, {"provider", "base_url", "model", "api_key"}}
	actors := []string{"session:d", "session:b", "session:a"}
	for i, c := range changes {
		if strings.Join(c.Fields, ",") != strings.Join(want[i], ",") || c.Actor != actors[i] || c.Tier != "primary" || c.Action != registry.RouterSettingsSet {
			t.Errorf("change %d = %+v, want fields %v by %s", i, c, want[i], actors[i])
		}
		if raw, _ := json.Marshal(c); strings.Contains(string(raw), "key-0000") {
			t.Errorf("change %d carries a key: %s", i, raw)
		}
	}
}

func TestSettings_Validation(t *testing.T) {
	srv := newModelServer(t, "x")
	s, m := newSettingsFor(t, settingsStore(t), Config{Primary: Tier{BaseURL: srv.url, APIKey: "env-key-0000000000", Model: "m"}})
	before := m.Config()
	for _, u := range []TierUpdate{
		{Provider: "nope", BaseURL: srv.url, Model: "m", APIKey: "a-long-enough-key"},
		{BaseURL: "", Model: "m", APIKey: "a-long-enough-key"},
		{BaseURL: "ftp://x", Model: "m", APIKey: "a-long-enough-key"},
		{BaseURL: "http://api.example.com/v1", Model: "m", APIKey: "a-long-enough-key"},
		{BaseURL: "http://10.0.0.5:8000/v1", Model: "m", APIKey: "a-long-enough-key"},
		{Provider: "openai", BaseURL: "", Model: "m", APIKey: "a-long-enough-key"},
		{BaseURL: "not a url", Model: "m", APIKey: "a-long-enough-key"},
		{BaseURL: "https://user:pw@host/v1", Model: "m", APIKey: "a-long-enough-key"},
		{BaseURL: srv.url, Model: "", APIKey: "a-long-enough-key"},
		{BaseURL: srv.url, Model: "m\x01x", APIKey: "a-long-enough-key"},
		{BaseURL: srv.url, Model: strings.Repeat("m", 201), APIKey: "a-long-enough-key"},
		{BaseURL: srv.url, Model: "m", APIKey: "k3y9z"},
		{BaseURL: srv.url, Model: "m", APIKey: "has a space in it"},
		{BaseURL: srv.url, Model: "m", APIKey: strings.Repeat("k", 4097)},
	} {
		var invalid *InvalidSettingError
		_, err := s.Set(context.Background(), "primary", u, "a")
		if !errors.As(err, &invalid) {
			t.Errorf("Set(%+v) = %v, want InvalidSettingError", u, err)
		} else if u.APIKey != "" && strings.Contains(invalid.Reason, u.APIKey) {
			t.Errorf("error %q quotes the key", invalid.Reason)
		}
	}
	if _, err := s.Set(context.Background(), "tertiary", TierUpdate{BaseURL: srv.url, Model: "m", APIKey: "a-long-enough-key"}, "a"); !errors.Is(err, ErrUnknownTier) {
		t.Errorf("unknown tier = %v", err)
	}
	if m.Config() != before {
		t.Fatalf("a refused update changed the config: %+v -> %+v", before, m.Config())
	}
}

// Changing the config while calls run is race-free, and every call runs
// on one whole config: the key and model it's called with always belong
// together.
func TestSettings_ConcurrentSetAndDecide(t *testing.T) {
	srv := newModelServer(t, "x")
	s, m := newSettingsFor(t, settingsStore(t), Config{Primary: Tier{BaseURL: srv.url, APIKey: "key-for-model-0", Model: "model-0"}})
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := m.Decide(context.Background(), "hi", nil, nil); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	for i := 1; i <= 20; i++ {
		n := string(rune('a' + i))
		if _, err := s.Set(context.Background(), "primary", TierUpdate{BaseURL: srv.url, Model: "model-" + n, APIKey: "key-for-model-" + n}, "a"); err != nil {
			t.Fatal(err)
		}
		_ = s.Tiers()
	}
	close(stop)
	wg.Wait()
	srv.mu.Lock()
	defer srv.mu.Unlock()
	for i, key := range srv.keys {
		if strings.TrimPrefix(key, "key-for-") != srv.models[i] {
			t.Fatalf("call %d: key %q with model %q — a torn config", i, key, srv.models[i])
		}
	}
}

// A new primary tier closes the breaker the old one opened.
func TestSettings_NewPrimaryResetsBreaker(t *testing.T) {
	failing, _ := newFakeServer(t, errorHandler(http.StatusBadRequest))
	escSrv, good := newModelServer(t, "escalation"), newModelServer(t, "new-primary")
	env := Config{Primary: Tier{BaseURL: failing.URL, APIKey: "env-key-0000000000", Model: "p"},
		Escalation: &Tier{BaseURL: escSrv.url, APIKey: "esc-key-0000000000", Model: "e"}}
	s, m := newSettingsFor(t, settingsStore(t), env)
	for range breakerThreshold {
		decideAnswer(t, m)
	}
	if !m.primaryBreaker.open() {
		t.Fatal("breaker not open after the primary kept failing")
	}
	if _, err := s.Set(context.Background(), "primary", TierUpdate{BaseURL: good.url, Model: "p2", APIKey: "new-key-0000000000"}, "a"); err != nil {
		t.Fatal(err)
	}
	if got := decideAnswer(t, m); got != "new-primary" {
		t.Fatalf("answered by %q, want the new primary tried at once", got)
	}
}

// Test makes one call with the tier's config in force; a provider error
// comes back scrubbed of the key, even when the provider echoes it.
func TestSettings_Test(t *testing.T) {
	const key = "sk-live-ECHOED-0123456789abcdef"
	var maxTokens atomic.Int64
	okSrv, okCalls := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			MaxTokens int64 `json:"max_tokens"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		maxTokens.Store(body.MaxTokens)
		textHandler(t, "p")(w, r)
	})
	echo, _ := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"Incorrect API key provided: ` + r.Header.Get("Authorization") + `","type":"invalid_request_error"}}`))
	})
	s, _ := newSettingsFor(t, settingsStore(t), Config{Primary: Tier{BaseURL: okSrv.URL, APIKey: "env-key-0000000000", Model: "env-model"}})
	ctx := context.Background()

	res, err := s.Test(ctx, "primary")
	if err != nil || !res.OK || res.Error != "" || res.Source != SourceEnv || res.Model != "env-model" {
		t.Fatalf("Test env primary = %+v, %v", res, err)
	}
	if atomic.LoadInt32(okCalls) != 1 || maxTokens.Load() != 1 {
		t.Fatalf("calls = %d, max_tokens = %d; want one call of one token", atomic.LoadInt32(okCalls), maxTokens.Load())
	}

	if _, err := s.Set(ctx, "primary", TierUpdate{BaseURL: echo.URL, Model: "stored-model", APIKey: key}, "a"); err != nil {
		t.Fatal(err)
	}
	res, err = s.Test(ctx, "primary")
	if err != nil || res.OK || res.Source != SourceStored || res.Error == "" {
		t.Fatalf("Test failing stored primary = %+v, %v", res, err)
	}
	if strings.Contains(res.Error, key) || strings.Contains(res.Error, "ECHOED") {
		t.Fatalf("test error leaks the key: %q", res.Error)
	}
	if res.Status != http.StatusUnauthorized || res.ErrorClass != TestErrorAuth || !strings.Contains(res.Error, "HTTP 401") {
		t.Errorf("test failure = status %d class %q error %q; want 401 auth_failed", res.Status, res.ErrorClass, res.Error)
	}
	if strings.Contains(res.Error, "Incorrect") {
		t.Errorf("test error carries the provider's body: %q", res.Error)
	}

	if _, err := s.Test(ctx, "escalation"); !errors.Is(err, ErrTierNotConfigured) {
		t.Fatalf("Test unconfigured escalation = %v", err)
	}
	if _, err := s.Test(ctx, "nope"); !errors.Is(err, ErrUnknownTier) {
		t.Fatalf("Test unknown tier = %v", err)
	}
}

// A test call that hangs is bounded by the test timeout.
func TestSettings_TestTimeout(t *testing.T) {
	release := make(chan struct{})
	hang, _ := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	t.Cleanup(func() { close(release) }) // before the server's Close
	m, err := New(Config{Primary: Tier{BaseURL: hang.URL, APIKey: "env-key-0000000000", Model: "m"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewSettings(context.Background(), settingsStore(t), m, m.Config(), WithTestTimeout(50*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	res, err := s.Test(context.Background(), "primary")
	if err != nil || res.OK || res.ErrorClass != TestErrorTimeout || res.Status != 0 {
		t.Fatalf("Test = %+v, %v", res, err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("Test took %v", time.Since(start))
	}
}

// Every router key Settings has held — env, stored, and rotated away
// from — is in the redaction set.
func TestSettings_KeysJoinRedaction(t *testing.T) {
	srv := newModelServer(t, "x")
	s, _ := newSettingsFor(t, settingsStore(t), Config{Primary: Tier{BaseURL: srv.url, APIKey: "env-redact-key-1111", Model: "m"}})
	for _, k := range []string{"stored-redact-key-2222", "rotated-redact-key-3333"} {
		if _, err := s.Set(context.Background(), "primary", TierUpdate{BaseURL: srv.url, Model: "m", APIKey: k}, "a"); err != nil {
			t.Fatal(err)
		}
	}
	text := "env-redact-key-1111 stored-redact-key-2222 rotated-redact-key-3333"
	if got := credentials.RedactValues(text, nil); got != "[redacted] [redacted] [redacted]" {
		t.Fatalf("RedactValues = %q", got)
	}
}

// fakeSettingsStore returns a stored primary that didn't decrypt.
type fakeSettingsStore struct {
	SettingsStore
	rows []*registry.RouterTier
	err  error
}

func (f fakeSettingsStore) ListRouterTiers(context.Context) ([]*registry.RouterTier, error) {
	return f.rows, f.err
}

// A stored tier that doesn't decrypt is skipped: the env's is used and
// the view says so, rather than the server refusing to start.
func TestSettings_UnreadableStoredTierFallsBack(t *testing.T) {
	srv := newModelServer(t, "env")
	store := fakeSettingsStore{rows: []*registry.RouterTier{{Tier: "primary", Provider: "openai", BaseURL: "https://x", Model: "stored"}},
		err: errors.New("decrypt failed")}
	s, m := newSettingsFor(t, store, Config{Primary: Tier{BaseURL: srv.url, APIKey: "env-key-0000000000", Model: "env-model"}})
	if v := tierView(t, s, "primary"); v.Source != SourceEnv || !v.StoredUnreadable || v.Model != "env-model" {
		t.Fatalf("view = %+v", v)
	}
	if got := decideAnswer(t, m); got != "env" {
		t.Fatalf("answered by %q", got)
	}
	if _, err := NewSettings(context.Background(), fakeSettingsStore{err: errors.New("db down")}, m, m.Config()); err == nil {
		t.Fatal("NewSettings with an unreadable store succeeded")
	}
}

// A provider that echoes the key back in its error doesn't get it into
// Decide's error (which is logged and stored on the dispatch), and the
// error still unwraps.
func TestDecide_ErrorScrubbedOfKey(t *testing.T) {
	const key = "echoed-decide-key-0123456789"
	echo, _ := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"bad key ` + r.Header.Get("Authorization") + `","type":"invalid_request_error"}}`))
	})
	_, m := newSettingsFor(t, settingsStore(t), Config{Primary: Tier{BaseURL: echo.URL, APIKey: key, Model: "m"}})
	_, err := m.Decide(context.Background(), "hi", nil, nil)
	if err == nil {
		t.Fatal("Decide succeeded")
	}
	if strings.Contains(err.Error(), key) {
		t.Fatalf("Decide error leaks the key: %v", err)
	}
	if !isUnavailable(err) {
		t.Fatalf("scrubbed error no longer unwraps: %v", err)
	}
	if _, err := m.Relay(context.Background(), router.RelayInput{Captured: "x"}); err == nil || strings.Contains(err.Error(), key) {
		t.Fatalf("Relay error = %v", err)
	}
}

// LOOM-185 review: a saved key never follows the tier to a new
// endpoint. Changing base_url or provider without a new api_key is
// refused, and nothing is sent anywhere; changing only the model keeps
// the key.
func TestSettings_KeyDoesNotFollowNewDestination(t *testing.T) {
	first, other := newModelServer(t, "first"), newModelServer(t, "other")
	s, m := newSettingsFor(t, settingsStore(t), Config{Primary: Tier{BaseURL: first.url, APIKey: "env-key-0000000000", Model: "m"}})
	ctx := context.Background()
	const key = "saved-key-for-first-0000"
	if _, err := s.Set(ctx, "primary", TierUpdate{BaseURL: first.url, Model: "m1", APIKey: key}, "a"); err != nil {
		t.Fatal(err)
	}
	var invalid *InvalidSettingError
	for _, u := range []TierUpdate{
		{BaseURL: other.url, Model: "m1"},
		{BaseURL: first.url + "/", Model: "m1"},
		{Provider: "anthropic", BaseURL: first.url, Model: "m1"},
	} {
		if _, err := s.Set(ctx, "primary", u, "a"); !errors.As(err, &invalid) || !strings.Contains(invalid.Reason, "api_key is required") {
			t.Errorf("Set(%+v) without a key = %v, want api_key required", u, err)
		}
	}
	if c := m.Config().Primary; c.BaseURL != first.url || c.APIKey != key {
		t.Fatalf("a refused change moved the key: %+v", c.BaseURL)
	}
	if _, err := s.Test(ctx, "primary"); err != nil {
		t.Fatal(err)
	}
	if other.calls.Load() != 0 {
		t.Fatal("the other endpoint was called")
	}
	// The model alone may change without re-entering the key.
	if _, err := s.Set(ctx, "primary", TierUpdate{BaseURL: first.url, Model: "m2"}, "a"); err != nil {
		t.Fatal(err)
	}
	// A new endpoint with a new key is fine.
	if _, err := s.Set(ctx, "primary", TierUpdate{BaseURL: other.url, Model: "m2", APIKey: "new-key-for-other-000"}, "a"); err != nil {
		t.Fatal(err)
	}
	decideAnswer(t, m)
	if k, _ := other.last(); k != "new-key-for-other-000" {
		t.Fatalf("other endpoint got key %q", k)
	}
	if k, _ := first.last(); k == "new-key-for-other-000" {
		t.Fatal("the first endpoint got the new key")
	}
}

// Base URLs are https; plain http only to a loopback host.
func TestCheckBaseURL(t *testing.T) {
	for raw, ok := range map[string]bool{
		"https://api.groq.com/openai/v1": true,
		"http://localhost:11434/v1":      true,
		"http://127.0.0.1:8000/v1":       true,
		"http://[::1]:8000/v1":           true,
		"http://api.groq.com/openai/v1":  false,
		"http://192.168.1.10:11434/v1":   false,
		"http://169.254.169.254/latest":  false,
		"https://u:p@api.example/v1":     false,
		"ftp://api.example/v1":           false,
		"https://":                       false,
	} {
		if got := checkBaseURL(raw) == ""; got != ok {
			t.Errorf("checkBaseURL(%q) ok = %v, want %v (%s)", raw, got, ok, checkBaseURL(raw))
		}
	}
}

// An anthropic tier may leave base_url out (Anthropic's own API), and its
// test call is one Messages request of one token with the stored key.
func TestSettings_AnthropicTier(t *testing.T) {
	var got struct {
		path, key, version string
		body               anthropicRequest
	}
	var mu sync.Mutex
	srv, _ := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		got.path, got.key, got.version = r.URL.Path, r.Header.Get("X-Api-Key"), r.Header.Get("Anthropic-Version")
		got.body = decodeAnthropicRequest(t, string(raw))
		mu.Unlock()
		jsonHandler(t, http.StatusOK, map[string]any{
			"id": "msg_test", "type": "message", "role": "assistant", "model": "m",
			"content": []any{map[string]any{"type": "text", "text": "p"}}, "stop_reason": "max_tokens", "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 1, "output_tokens": 1},
		})(w, r)
	})
	s, m := newSettingsFor(t, settingsStore(t), Config{Primary: Tier{BaseURL: srv.URL, APIKey: "env-key-0000000000", Model: "m"}})
	ctx := context.Background()

	v, err := s.Set(ctx, "escalation", TierUpdate{Provider: "Anthropic", Model: "claude-haiku-4-5", APIKey: "anthropic-key-000000000"}, "a")
	if err != nil {
		t.Fatal(err)
	}
	if v.Provider != "anthropic" || v.BaseURL != anthropicDefaultBaseURL {
		t.Fatalf("anthropic tier without a base URL = %+v", v)
	}
	if esc := m.Config().Escalation; esc == nil || esc.Provider != ProviderAnthropic {
		t.Fatalf("escalation = %+v", esc)
	}

	if _, err := s.Set(ctx, "escalation", TierUpdate{Provider: "anthropic", BaseURL: srv.URL, Model: "claude-haiku-4-5", APIKey: "anthropic-key-000000000"}, "a"); err != nil {
		t.Fatal(err)
	}
	res, err := s.Test(ctx, "escalation")
	if err != nil || !res.OK {
		t.Fatalf("anthropic test = %+v, %v", res, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if got.path != "/v1/messages" || got.key != "anthropic-key-000000000" || got.version == "" ||
		got.body.Model != "claude-haiku-4-5" || got.body.MaxTokens != 1 {
		t.Fatalf("anthropic test call = %+v", got)
	}
}

// An anthropic test failure reports its status and class too.
func TestSettings_AnthropicTestFailure(t *testing.T) {
	srv, _ := newFakeServer(t, anthropicErrorHandler(http.StatusNotFound))
	s, _ := newSettingsFor(t, settingsStore(t), Config{Primary: Tier{Provider: ProviderAnthropic, BaseURL: srv.URL, APIKey: "env-key-0000000000", Model: "nope"}})
	res, err := s.Test(context.Background(), "primary")
	if err != nil || res.OK || res.Status != http.StatusNotFound || res.ErrorClass != TestErrorNotFound {
		t.Fatalf("Test = %+v, %v", res, err)
	}
}

// A call still running on the old primary when SetConfig replaces it
// doesn't count against the new primary's breaker.
func TestSetConfig_StaleCallDoesNotTripNewBreaker(t *testing.T) {
	release, entered := make(chan struct{}), make(chan struct{}, 4)
	slow, _ := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-release
		errorHandler(http.StatusBadRequest)(w, r)
	})
	esc := newModelServer(t, "escalation")
	good := newModelServer(t, "new-primary")
	m, err := New(Config{Primary: Tier{BaseURL: slow.URL, APIKey: "old-key-0000000000", Model: "p"},
		Escalation: &Tier{BaseURL: esc.url, APIKey: "esc-key-0000000000", Model: "e"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Two failures on the old primary, one short of opening.
	for range breakerThreshold - 1 {
		m.primaryBreaker.record(true)
	}
	var wg sync.WaitGroup
	wg.Go(func() { _, _ = m.Decide(context.Background(), "hi", nil, nil) })
	<-entered // the call is at the old primary
	if err := m.SetConfig(Config{Primary: Tier{BaseURL: good.url, APIKey: "new-key-0000000000", Model: "p2"},
		Escalation: &Tier{BaseURL: esc.url, APIKey: "esc-key-0000000000", Model: "e"}}); err != nil {
		t.Fatal(err)
	}
	for range breakerThreshold - 1 {
		m.primaryBreaker.record(true)
	}
	close(release)
	wg.Wait()
	if m.primaryBreaker.open() {
		t.Fatal("the old primary's late failure opened the new primary's breaker")
	}
}
