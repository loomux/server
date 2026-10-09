package llmrouter

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// listingServer answers GET /models (OpenAI) and GET /v1/models
// (Anthropic) with ids, recording the keys it was called with.
type listingServer struct {
	url   string
	calls atomic.Int32
	mu    sync.Mutex
	keys  []string
	paths []string
}

func newListingServer(t *testing.T, status int, ids ...string) *listingServer {
	t.Helper()
	ls := &listingServer{}
	srv, _ := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		ls.calls.Add(1)
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if k := r.Header.Get("X-Api-Key"); k != "" {
			key = k
		}
		ls.mu.Lock()
		ls.keys, ls.paths = append(ls.keys, key), append(ls.paths, r.Method+" "+r.URL.Path)
		ls.mu.Unlock()
		if status != http.StatusOK {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":{"message":"Incorrect API key provided: ` + key + `","type":"authentication_error"}}`))
			return
		}
		data := make([]map[string]any, 0, len(ids))
		for _, id := range ids {
			data = append(data, map[string]any{"id": id, "object": "model", "created": 1, "owned_by": "x",
				"type": "model", "display_name": "Display " + id, "created_at": "2026-01-01T00:00:00Z"})
		}
		body := map[string]any{"object": "list", "data": data, "has_more": false}
		if len(ids) > 0 {
			body["first_id"], body["last_id"] = ids[0], ids[len(ids)-1]
		}
		jsonHandler(t, http.StatusOK, body)(w, r)
	})
	ls.url = srv.URL
	return ls
}

func (ls *listingServer) last() (key, path string) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	return ls.keys[len(ls.keys)-1], ls.paths[len(ls.paths)-1]
}

func modelIDs(ms []ModelInfo) string {
	ids := make([]string, len(ms))
	for i, m := range ms {
		ids[i] = m.ID
	}
	return strings.Join(ids, ",")
}

// LOOM-191: the tier's own provider is listed with its saved key, sorted.
func TestModels_ListsWithTheTiersKey(t *testing.T) {
	ls := newListingServer(t, http.StatusOK, "llama-70b", "gpt-oss-20b", "llama-8b")
	s, _ := newSettingsFor(t, settingsStore(t), Config{Primary: Tier{BaseURL: ls.url, APIKey: "env-key-0000000000", Model: "llama-8b"}})
	res, err := s.Models(context.Background(), "primary", ModelsRequest{})
	if err != nil || !res.OK || res.Cached {
		t.Fatalf("Models = %+v, %v", res, err)
	}
	if got := modelIDs(res.Models); got != "gpt-oss-20b,llama-70b,llama-8b" {
		t.Fatalf("models = %s", got)
	}
	if key, path := ls.last(); key != "env-key-0000000000" || path != "GET /models" {
		t.Fatalf("listed with key %q at %s", key, path)
	}
	// The same provider and base URL given explicitly is still the tier's own.
	if _, err := s.Models(context.Background(), "primary", ModelsRequest{Provider: "openai", BaseURL: ls.url}); err != nil {
		t.Fatal(err)
	}
}

// A key being entered lists an endpoint that isn't saved yet; Anthropic
// is listed through its own API, display names included.
func TestModels_EnteredKeyAndAnthropic(t *testing.T) {
	ls := newListingServer(t, http.StatusOK, "claude-sonnet-x", "claude-haiku-x")
	other := newListingServer(t, http.StatusOK)
	s, _ := newSettingsFor(t, settingsStore(t), Config{Primary: Tier{BaseURL: other.url, APIKey: "env-key-0000000000", Model: "m"}})
	res, err := s.Models(context.Background(), "escalation", ModelsRequest{Provider: "anthropic", BaseURL: ls.url, APIKey: "entered-key-000000000"})
	if err != nil || !res.OK {
		t.Fatalf("Models = %+v, %v", res, err)
	}
	if got := modelIDs(res.Models); got != "claude-haiku-x,claude-sonnet-x" || res.Models[0].Name != "Display claude-haiku-x" {
		t.Fatalf("models = %+v", res.Models)
	}
	if key, path := ls.last(); key != "entered-key-000000000" || path != "GET /v1/models" {
		t.Fatalf("listed with key %q at %s", key, path)
	}
	if other.calls.Load() != 0 {
		t.Fatal("the saved tier's endpoint was called")
	}
}

// The saved key is never sent to an endpoint it wasn't entered for, and
// an entered key goes only to an https (or loopback) endpoint.
func TestModels_SavedKeyStaysWithItsEndpoint(t *testing.T) {
	saved, elsewhere := newListingServer(t, http.StatusOK, "m"), newListingServer(t, http.StatusOK, "x")
	s, _ := newSettingsFor(t, settingsStore(t), Config{Primary: Tier{BaseURL: saved.url, APIKey: "env-key-0000000000", Model: "m"}})
	var invalid *InvalidSettingError
	for _, req := range []ModelsRequest{
		{BaseURL: elsewhere.url},
		{Provider: "anthropic"},
		{BaseURL: "http://models.example.com/v1", APIKey: "entered-key-000000000"},
		{BaseURL: elsewhere.url, APIKey: "short"},
		{Provider: "acme", BaseURL: elsewhere.url, APIKey: "entered-key-000000000"},
	} {
		if _, err := s.Models(context.Background(), "primary", req); !errors.As(err, &invalid) {
			t.Errorf("Models(%+v) = %v, want InvalidSettingError", req, err)
		}
	}
	if saved.calls.Load()+elsewhere.calls.Load() != 0 {
		t.Fatal("a refused listing called a provider")
	}
	if _, err := s.Models(context.Background(), "escalation", ModelsRequest{}); !errors.Is(err, ErrTierNotConfigured) {
		t.Fatalf("unconfigured escalation without a key = %v", err)
	}
	if _, err := s.Models(context.Background(), "tertiary", ModelsRequest{}); !errors.Is(err, ErrUnknownTier) {
		t.Fatalf("unknown tier = %v", err)
	}
}

// A provider failure is its status and class, never its body (which may
// quote the key back).
func TestModels_FailureIsStatusAndClass(t *testing.T) {
	ls := newListingServer(t, http.StatusUnauthorized)
	s, _ := newSettingsFor(t, settingsStore(t), Config{Primary: Tier{BaseURL: ls.url, APIKey: "env-key-ECHOED-000000", Model: "m"}})
	res, err := s.Models(context.Background(), "primary", ModelsRequest{})
	if err != nil || res.OK || res.Status != http.StatusUnauthorized || res.ErrorClass != TestErrorAuth || res.Error == "" {
		t.Fatalf("Models = %+v, %v", res, err)
	}
	if strings.Contains(res.Error, "ECHOED") || strings.Contains(res.Error, "Incorrect") {
		t.Fatalf("error carries the provider's body: %q", res.Error)
	}
	// Failures aren't cached.
	if _, err := s.Models(context.Background(), "primary", ModelsRequest{}); err != nil || ls.calls.Load() != 2 {
		t.Fatalf("calls = %d, want the failure asked again", ls.calls.Load())
	}
}

// A listing is reused for a few minutes, per endpoint and key.
func TestModels_Cache(t *testing.T) {
	ls := newListingServer(t, http.StatusOK, "a", "b")
	s, _ := newSettingsFor(t, settingsStore(t), Config{Primary: Tier{BaseURL: ls.url, APIKey: "env-key-0000000000", Model: "a"}})
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	ctx := context.Background()

	for i := range 3 {
		res, err := s.Models(ctx, "primary", ModelsRequest{})
		if err != nil || !res.OK || res.Cached != (i > 0) {
			t.Fatalf("call %d = %+v, %v", i, res, err)
		}
	}
	if ls.calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1", ls.calls.Load())
	}
	// Another key is another listing.
	if res, _ := s.Models(ctx, "primary", ModelsRequest{BaseURL: ls.url, APIKey: "another-key-0000000"}); res.Cached || ls.calls.Load() != 2 {
		t.Fatalf("another key: cached %v, calls %d", res.Cached, ls.calls.Load())
	}
	now = now.Add(modelsCacheTTL + time.Second)
	if res, _ := s.Models(ctx, "primary", ModelsRequest{}); res.Cached || ls.calls.Load() != 3 {
		t.Fatalf("after the TTL: cached %v, calls %d", res.Cached, ls.calls.Load())
	}
}

// The cache stays bounded, dropping the oldest listing.
func TestModelsCache_Bounded(t *testing.T) {
	var c modelsCache
	now := time.Now()
	for i := range modelsCacheSize + 5 {
		c.put(modelsCacheKey(Tier{APIKey: strings.Repeat("k", i+1)}), nil, now.Add(time.Duration(i)*time.Millisecond))
	}
	if len(c.entries) != modelsCacheSize {
		t.Fatalf("entries = %d", len(c.entries))
	}
	if _, ok := c.get(modelsCacheKey(Tier{APIKey: "k"}), now); ok {
		t.Fatal("the oldest listing is still cached")
	}
}

// A listing that hangs is bounded by the same timeout as the test call.
func TestModels_Timeout(t *testing.T) {
	release := make(chan struct{})
	hang, _ := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	t.Cleanup(func() { close(release) })
	m, err := New(Config{Primary: Tier{BaseURL: hang.URL, APIKey: "env-key-0000000000", Model: "m"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewSettings(context.Background(), settingsStore(t), m, m.Config(), WithTestTimeout(50*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.Models(context.Background(), "primary", ModelsRequest{})
	if err != nil || res.OK || res.ErrorClass != TestErrorTimeout {
		t.Fatalf("Models = %+v, %v", res, err)
	}
}
