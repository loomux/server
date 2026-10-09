package llmrouter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/anthropics/anthropic-sdk-go"
)

// Model lists (LOOM-191): what a tier's provider offers, for Settings'
// model picker. Listed through the provider's own GET /v1/models with the
// tier's key, or a key being entered that isn't saved yet.

const (
	// modelsCacheTTL is how long a listing is reused: long enough that
	// typing in the picker doesn't call the provider again, short enough
	// that a new model shows up the same day.
	modelsCacheTTL = 5 * time.Minute
	// modelsCacheSize bounds the cache; the oldest listing goes first.
	modelsCacheSize = 32
	// maxModels bounds one listing.
	maxModels = 1000
)

// ModelsRequest says which endpoint to list. Empty fields mean the
// tier's own (stored or env). A saved key is only used for the endpoint
// it belongs to: listing another provider or base URL needs APIKey.
type ModelsRequest struct {
	Provider string
	BaseURL  string
	APIKey   string
}

// ModelInfo is one model a provider lists.
type ModelInfo struct {
	ID string
	// Name is the provider's display name, when it has one.
	Name string
}

// ModelsResult is a listing, or why there isn't one: like TestResult, a
// failure is its status and class alone.
type ModelsResult struct {
	OK         bool
	Models     []ModelInfo
	Status     int
	ErrorClass string
	Error      string
	// Cached says the listing came from the last few minutes' call.
	Cached bool
}

type modelsEntry struct {
	models []ModelInfo
	at     time.Time
}

// modelsCache keeps recent successful listings, keyed by a hash of
// provider, base URL and key, so the key itself isn't held as a map key.
type modelsCache struct {
	mu      sync.Mutex
	entries map[string]modelsEntry
}

func modelsCacheKey(t Tier) string {
	sum := sha256.Sum256([]byte(string(t.Provider) + "\x00" + t.BaseURL + "\x00" + t.APIKey))
	return hex.EncodeToString(sum[:])
}

func (c *modelsCache) get(key string, now time.Time) ([]ModelInfo, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || now.Sub(e.at) > modelsCacheTTL {
		return nil, false
	}
	return e.models, true
}

func (c *modelsCache) put(key string, models []ModelInfo, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]modelsEntry{}
	}
	for k, e := range c.entries {
		if now.Sub(e.at) > modelsCacheTTL {
			delete(c.entries, k)
		}
	}
	if len(c.entries) >= modelsCacheSize {
		oldest := ""
		for k, e := range c.entries {
			if oldest == "" || e.at.Before(c.entries[oldest].at) {
				oldest = k
			}
		}
		delete(c.entries, oldest)
	}
	c.entries[key] = modelsEntry{models: models, at: now}
}

// Models lists the models tier's provider offers. The error return is for
// ErrUnknownTier, ErrTierNotConfigured (no key to list with) and
// *InvalidSettingError; a provider failure is a ModelsResult with OK
// false.
func (s *Settings) Models(ctx context.Context, tier string, req ModelsRequest) (ModelsResult, error) {
	if err := validTierName(tier); err != nil {
		return ModelsResult{}, err
	}
	t, err := s.modelsTier(tier, req)
	if err != nil {
		return ModelsResult{}, err
	}
	key := modelsCacheKey(t)
	if models, ok := s.models.get(key, s.now()); ok {
		return ModelsResult{OK: true, Models: models, Cached: true}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, s.testTimeout)
	defer cancel()
	models, err := listModels(ctx, t)
	if err != nil {
		res := ModelsResult{}
		res.Status, res.ErrorClass = classifyTestError(ctx, err)
		res.Error = testErrorText(res.Status, res.ErrorClass)
		return res, nil
	}
	s.models.put(key, models, s.now())
	return ModelsResult{OK: true, Models: models}, nil
}

// modelsTier is the endpoint and key to list with, from req over the
// tier in force.
func (s *Settings) modelsTier(tier string, req ModelsRequest) (Tier, error) {
	var provider Provider
	if strings.TrimSpace(req.Provider) != "" {
		p, err := ParseProvider(req.Provider)
		if err != nil {
			return Tier{}, &InvalidSettingError{"provider must be one of: " + providerNames()}
		}
		provider = p
	}
	baseURL := strings.TrimSpace(req.BaseURL)

	if req.APIKey != "" {
		if reason := checkAPIKey(req.APIKey); reason != "" {
			return Tier{}, &InvalidSettingError{reason}
		}
		if provider == "" {
			provider = ProviderOpenAI
		}
		if baseURL == "" {
			baseURL = provider.defaultBaseURL()
		}
		if baseURL == "" {
			return Tier{}, &InvalidSettingError{"base_url is required"}
		}
		if reason := checkBaseURL(baseURL); reason != "" {
			return Tier{}, &InvalidSettingError{reason}
		}
		return Tier{Provider: provider, BaseURL: baseURL, APIKey: req.APIKey}, nil
	}

	s.mu.Lock()
	active, _, ok := s.activeTier(tier)
	s.mu.Unlock()
	if !ok {
		return Tier{}, fmt.Errorf("%w: %s", ErrTierNotConfigured, tier)
	}
	activeProvider, _ := ParseProvider(string(active.Provider))
	if (provider != "" && provider != activeProvider) || (baseURL != "" && baseURL != active.BaseURL) {
		// The saved key never goes anywhere it wasn't entered for.
		return Tier{}, &InvalidSettingError{"api_key is required to list models for another provider or base_url"}
	}
	active.Provider = activeProvider
	return active, nil
}

func providerNames() string {
	names := make([]string, 0, len(Providers()))
	for _, p := range Providers() {
		names = append(names, string(p))
	}
	return strings.Join(names, ", ")
}

// checkAPIKey says what's wrong with a key being entered, "" if nothing.
func checkAPIKey(key string) string {
	switch {
	case len(key) < minAPIKeyLen:
		return "api_key is too short (at least 8 characters)"
	case len(key) > maxAPIKeyLen:
		return "api_key is too long (at most 4 KiB)"
	case strings.IndexFunc(key, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0:
		return "api_key can't contain whitespace or control characters"
	}
	return ""
}

// listModels asks t's provider for its models, sorted by id.
func listModels(ctx context.Context, t Tier) ([]ModelInfo, error) {
	var out []ModelInfo
	if t.Provider == ProviderAnthropic {
		client := buildAnthropicClient(t)
		pager := client.Models.ListAutoPaging(ctx, anthropic.ModelListParams{Limit: anthropic.Int(maxModels)})
		for pager.Next() && len(out) < maxModels {
			m := pager.Current()
			out = append(out, ModelInfo{ID: m.ID, Name: m.DisplayName})
		}
		if err := pager.Err(); err != nil {
			return nil, err
		}
	} else {
		client := buildClient(t)
		page, err := client.Models.List(ctx)
		if err != nil {
			return nil, err
		}
		for _, m := range page.Data {
			if m.ID != "" && len(out) < maxModels {
				out = append(out, ModelInfo{ID: m.ID})
			}
		}
	}
	slices.SortFunc(out, func(a, b ModelInfo) int { return strings.Compare(a.ID, b.ID) })
	if out == nil {
		out = []ModelInfo{}
	}
	return out, nil
}
