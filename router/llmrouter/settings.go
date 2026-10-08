package llmrouter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/openai/openai-go/v3"

	"github.com/Loomux/server/credentials"
	"github.com/Loomux/server/registry"
)

// Router settings (LOOM-185, docs/design/router-settings.md): each tier
// is either stored through the API, overriding the environment's for
// that tier wholly, or the environment's, as before. Settings keeps the
// stored tiers, applies the effective config to the Model without a
// restart, and audits every change.

// Tier sources, as Settings reports them.
const (
	SourceStored = "stored"
	SourceEnv    = "env"
	// SourceNone: the escalation tier is configured nowhere (off).
	SourceNone = "none"
)

// Errors Settings returns besides the store's own.
var (
	// ErrUnknownTier is a tier name other than primary and escalation.
	ErrUnknownTier = errors.New("llmrouter: unknown router tier")
	// ErrTierNotConfigured is a test of a tier configured nowhere.
	ErrTierNotConfigured = errors.New("llmrouter: router tier is not configured")
)

// InvalidSettingError is an update refused for what it says; its
// message is fit to show the owner, and never contains a key.
type InvalidSettingError struct{ Reason string }

func (e *InvalidSettingError) Error() string { return e.Reason }

// SettingsStore is what Settings needs of registry.Store.
type SettingsStore interface {
	SetRouterTier(ctx context.Context, t *registry.RouterTier, change *registry.RouterSettingsChange) error
	ListRouterTiers(ctx context.Context) ([]*registry.RouterTier, error)
	DeleteRouterTier(ctx context.Context, tier string, change *registry.RouterSettingsChange) error
	ListRouterSettingsChanges(ctx context.Context, limit int) ([]*registry.RouterSettingsChange, error)
}

// TierView is a tier as the owner sees it: everything but the key.
type TierView struct {
	Tier   string
	Source string
	// Provider, BaseURL, Model, KeyFingerprint and KeyLast4 describe the
	// active config (stored or env); empty for SourceNone.
	Provider       string
	BaseURL        string
	Model          string
	KeyFingerprint string
	KeyLast4       string
	// SetAt is when the stored tier was set; zero otherwise.
	SetAt time.Time
	// EnvConfigured says whether the environment has this tier, to fall
	// back to.
	EnvConfigured bool
	// StoredUnreadable: a stored tier exists but couldn't be decrypted
	// (the master key changed, or there is none), so the env's is used.
	StoredUnreadable bool
}

// TierUpdate is a PUT of a tier. An empty APIKey keeps the stored key.
type TierUpdate struct {
	Provider string
	BaseURL  string
	Model    string
	APIKey   string
}

// TestResult is the outcome of one test call to a tier's provider.
// A failure is described by Status and ErrorClass alone, never by the
// provider's response body, which may quote the request back.
type TestResult struct {
	OK bool
	// Status is the provider's HTTP status, 0 when none came back.
	Status int
	// ErrorClass is one of the TestError* classes; Error says it in words.
	ErrorClass string
	Error      string
	Model      string
	Source     string
	Duration   time.Duration
}

// Test error classes: what kind of failure a test call met.
const (
	TestErrorAuth        = "auth_failed"  // 401, 403: the key
	TestErrorNotFound    = "not_found"    // 404: the base URL or model
	TestErrorBadRequest  = "bad_request"  // other 4xx: the model or the request
	TestErrorRateLimited = "rate_limited" // 429
	TestErrorProvider    = "provider_error"
	TestErrorTimeout     = "timeout"
	TestErrorUnreachable = "unreachable"
)

const (
	defaultTestTimeout = 15 * time.Second
	maxModelLen        = 200
	maxBaseURLLen      = 2048
	maxAPIKeyLen       = 4 << 10
	// minAPIKeyLen keeps every stored key long enough to be redacted
	// (credentials' minimum is six).
	minAPIKeyLen = 8
	// minLast4KeyLen is the shortest key whose last four characters are
	// shown: of a shorter one they'd be too much of it.
	minLast4KeyLen = 16
)

var tierNames = []string{registry.RouterTierPrimary, registry.RouterTierEscalation}

// Settings serves the router settings API and keeps a Model's config
// current with them. Safe for concurrent use: changes are serialized,
// and each is in the store before the Model uses it.
type Settings struct {
	store       SettingsStore
	model       *Model
	env         Config
	testTimeout time.Duration
	now         func() time.Time

	mu sync.Mutex
	// stored are the readable stored tiers, by name; unreadable are the
	// stored tiers that didn't decrypt.
	stored     map[string]*registry.RouterTier
	unreadable map[string]bool
}

// SettingsOption configures Settings.
type SettingsOption func(*Settings)

// WithTestTimeout bounds Test's one call (default 15s).
func WithTestTimeout(d time.Duration) SettingsOption {
	return func(s *Settings) { s.testTimeout = d }
}

// NewSettings loads the stored tiers and applies the effective config
// (stored over env, per tier) to model. env is the environment's config,
// the fallback. A stored tier that can't be read or used is skipped
// (StoredUnreadable), leaving the env's in place; only a store that
// can't be read at all is an error.
func NewSettings(ctx context.Context, store SettingsStore, model *Model, env Config, opts ...SettingsOption) (*Settings, error) {
	s := &Settings{store: store, model: model, env: *cloneConfig(env), testTimeout: defaultTestTimeout, now: time.Now,
		stored: map[string]*registry.RouterTier{}, unreadable: map[string]bool{}}
	for _, opt := range opts {
		opt(s)
	}
	addSecrets(env)
	rows, err := store.ListRouterTiers(ctx)
	if rows == nil && err != nil {
		return nil, fmt.Errorf("llmrouter: load router settings: %w", err)
	}
	for _, t := range rows {
		if !slices.Contains(tierNames, t.Tier) {
			continue
		}
		if t.APIKey == "" || toTier(t).Validate() != nil {
			s.unreadable[t.Tier] = true
			continue
		}
		credentials.AddSystemSecret(t.APIKey)
		s.stored[t.Tier] = t
	}
	if err := model.SetConfig(s.effective()); err != nil {
		return nil, fmt.Errorf("llmrouter: apply router settings: %w", err)
	}
	return s, nil
}

func addSecrets(cfg Config) {
	credentials.AddSystemSecret(cfg.Primary.APIKey)
	if cfg.Escalation != nil {
		credentials.AddSystemSecret(cfg.Escalation.APIKey)
	}
}

func toTier(t *registry.RouterTier) Tier {
	return Tier{Provider: Provider(t.Provider), BaseURL: t.BaseURL, APIKey: t.APIKey, Model: t.Model}
}

// effective is the config in force: each stored tier over the env's.
// Callers hold s.mu.
func (s *Settings) effective() Config {
	cfg := *cloneConfig(s.env)
	if t, ok := s.stored[registry.RouterTierPrimary]; ok {
		cfg.Primary = toTier(t)
	}
	if t, ok := s.stored[registry.RouterTierEscalation]; ok {
		esc := toTier(t)
		cfg.Escalation = &esc
	}
	return cfg
}

// activeTier is name's config in force and where it's from; ok is false
// for an escalation tier configured nowhere. Callers hold s.mu.
func (s *Settings) activeTier(name string) (tier Tier, source string, ok bool) {
	if t, stored := s.stored[name]; stored {
		return toTier(t), SourceStored, true
	}
	if name == registry.RouterTierPrimary {
		return s.env.Primary, SourceEnv, true
	}
	if s.env.Escalation != nil {
		return *s.env.Escalation, SourceEnv, true
	}
	return Tier{}, SourceNone, false
}

// Tiers describes both tiers, primary first.
func (s *Settings) Tiers() []TierView {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]TierView, 0, len(tierNames))
	for _, name := range tierNames {
		out = append(out, s.view(name))
	}
	return out
}

// view describes one tier. Callers hold s.mu.
func (s *Settings) view(name string) TierView {
	v := TierView{Tier: name, StoredUnreadable: s.unreadable[name],
		EnvConfigured: name == registry.RouterTierPrimary || s.env.Escalation != nil}
	tier, source, ok := s.activeTier(name)
	v.Source = source
	if !ok {
		return v
	}
	provider, _ := ParseProvider(string(tier.Provider))
	v.Provider, v.BaseURL, v.Model = string(provider), tier.BaseURL, tier.Model
	v.KeyFingerprint, v.KeyLast4 = KeyFingerprint(tier.APIKey), keyLast4(tier.APIKey)
	if t, stored := s.stored[name]; stored {
		v.SetAt = t.SetAt
	}
	return v
}

// KeyFingerprint identifies key without revealing it: "sha256:" and the
// first 12 hex digits of its SHA-256.
func KeyFingerprint(key string) string {
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(key))
	return "sha256:" + hex.EncodeToString(sum[:])[:12]
}

func keyLast4(key string) string {
	if len(key) < minLast4KeyLen {
		return ""
	}
	return key[len(key)-4:]
}

func validTierName(name string) error {
	if !slices.Contains(tierNames, name) {
		return fmt.Errorf("%w: %q", ErrUnknownTier, name)
	}
	return nil
}

// validateUpdate normalizes u and says what's wrong with it, if anything.
// Messages never quote the key.
func validateUpdate(u *TierUpdate) error {
	provider, err := ParseProvider(u.Provider)
	if err != nil {
		names := make([]string, 0, len(Providers()))
		for _, p := range Providers() {
			names = append(names, string(p))
		}
		return &InvalidSettingError{"provider must be one of: " + strings.Join(names, ", ")}
	}
	u.Provider = string(provider)
	u.BaseURL = strings.TrimSpace(u.BaseURL)
	u.Model = strings.TrimSpace(u.Model)
	if u.BaseURL == "" {
		// Anthropic's own API when an anthropic tier names none, as for
		// the env (LOOM-186).
		u.BaseURL = provider.defaultBaseURL()
	}
	if u.BaseURL == "" {
		return &InvalidSettingError{"base_url is required"}
	}
	if reason := checkBaseURL(u.BaseURL); reason != "" {
		return &InvalidSettingError{reason}
	}
	if u.Model == "" {
		return &InvalidSettingError{"model is required"}
	}
	if len(u.Model) > maxModelLen || strings.IndexFunc(u.Model, unicode.IsControl) >= 0 {
		return &InvalidSettingError{"model must be at most 200 characters, without control characters"}
	}
	if u.APIKey != "" && len(u.APIKey) < minAPIKeyLen {
		return &InvalidSettingError{"api_key is too short (at least 8 characters)"}
	}
	if len(u.APIKey) > maxAPIKeyLen {
		return &InvalidSettingError{"api_key is too long (at most 4 KiB)"}
	}
	if strings.IndexFunc(u.APIKey, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return &InvalidSettingError{"api_key can't contain whitespace or control characters"}
	}
	return nil
}

// checkBaseURL says what's wrong with raw as a base URL, "" if nothing:
// https only, so a key never crosses the network in the clear; plain
// http only to a loopback host (a local model server), which also keeps
// the test call from being aimed at plain-http services elsewhere.
func checkBaseURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || len(raw) > maxBaseURLLen || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return "base_url must be an https URL"
	}
	if parsed.User != nil {
		return "base_url can't carry credentials; put the key in api_key"
	}
	if parsed.Scheme == "http" && !isLoopback(parsed.Hostname()) {
		return "base_url must be https (plain http only for localhost)"
	}
	return ""
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Set stores tier's config and applies it at once, auditing the fields
// it changed under actor. A change that changes nothing writes nothing.
// Errors: ErrUnknownTier, *InvalidSettingError, registry.ErrNoMasterKey,
// or the store's.
func (s *Settings) Set(ctx context.Context, tier string, u TierUpdate, actor string) (TierView, error) {
	if err := validTierName(tier); err != nil {
		return TierView{}, err
	}
	if err := validateUpdate(&u); err != nil {
		return TierView{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.stored[tier]
	key := u.APIKey
	if key == "" {
		switch {
		case old == nil:
			return TierView{}, &InvalidSettingError{"api_key is required: this tier has no saved key to keep"}
		case old.Provider != u.Provider || old.BaseURL != u.BaseURL:
			// The saved key never follows the tier to another endpoint.
			return TierView{}, &InvalidSettingError{"api_key is required when the provider or base_url changes: the saved key isn't sent anywhere new"}
		}
		key = old.APIKey
	}
	next := &registry.RouterTier{Tier: tier, Provider: u.Provider, BaseURL: u.BaseURL, Model: u.Model,
		APIKey: key, KeyFingerprint: KeyFingerprint(key), KeyLast4: keyLast4(key)}
	fields := changedFields(old, next)
	if len(fields) == 0 {
		return s.view(tier), nil
	}
	cfg := s.effective()
	if tier == registry.RouterTierPrimary {
		cfg.Primary = toTier(next)
	} else {
		esc := toTier(next)
		cfg.Escalation = &esc
	}
	if err := cfg.Validate(); err != nil {
		return TierView{}, &InvalidSettingError{"the tier is incomplete"}
	}
	change := &registry.RouterSettingsChange{Tier: tier, Action: registry.RouterSettingsSet, Fields: fields, Actor: actor}
	if err := s.store.SetRouterTier(ctx, next, change); err != nil {
		return TierView{}, err
	}
	credentials.AddSystemSecret(key)
	s.stored[tier] = next
	delete(s.unreadable, tier)
	if err := s.model.SetConfig(cfg); err != nil {
		// Validated above, so not reached; the store already has it,
		// and the next start applies it.
		return TierView{}, fmt.Errorf("llmrouter: apply router settings: %w", err)
	}
	return s.view(tier), nil
}

// changedFields names the fields next changes from old (every field, if
// there was no stored tier).
func changedFields(old, next *registry.RouterTier) []string {
	if old == nil {
		return []string{"provider", "base_url", "model", "api_key"}
	}
	var out []string
	if old.Provider != next.Provider {
		out = append(out, "provider")
	}
	if old.BaseURL != next.BaseURL {
		out = append(out, "base_url")
	}
	if old.Model != next.Model {
		out = append(out, "model")
	}
	if old.APIKey != next.APIKey {
		out = append(out, "api_key")
	}
	return out
}

// Clear deletes tier's stored config, going back to the env's (for an
// escalation tier the env doesn't have, escalation is then off), and
// audits it. registry.ErrNotFound if nothing is stored.
func (s *Settings) Clear(ctx context.Context, tier, actor string) error {
	if err := validTierName(tier); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	change := &registry.RouterSettingsChange{Tier: tier, Action: registry.RouterSettingsClear, Fields: []string{}, Actor: actor}
	if err := s.store.DeleteRouterTier(ctx, tier, change); err != nil {
		return err
	}
	delete(s.stored, tier)
	delete(s.unreadable, tier)
	if err := s.model.SetConfig(s.effective()); err != nil {
		return fmt.Errorf("llmrouter: apply router settings: %w", err)
	}
	return nil
}

// Changes is the audit trail, newest first, at most limit entries.
func (s *Settings) Changes(ctx context.Context, limit int) ([]*registry.RouterSettingsChange, error) {
	return s.store.ListRouterSettingsChanges(ctx, limit)
}

// Test makes one cheap call to tier's provider with the config in force
// for it. A provider failure is a TestResult with OK false, its status
// and error class; the error return is for ErrUnknownTier and
// ErrTierNotConfigured.
func (s *Settings) Test(ctx context.Context, tier string) (TestResult, error) {
	if err := validTierName(tier); err != nil {
		return TestResult{}, err
	}
	s.mu.Lock()
	t, source, ok := s.activeTier(tier)
	s.mu.Unlock()
	if !ok {
		return TestResult{}, fmt.Errorf("%w: %s", ErrTierNotConfigured, tier)
	}
	ctx, cancel := context.WithTimeout(ctx, s.testTimeout)
	defer cancel()
	start := s.now()
	err := pingTier(ctx, t)
	res := TestResult{OK: err == nil, Model: t.Model, Source: source, Duration: s.now().Sub(start)}
	if err != nil {
		res.Status, res.ErrorClass = classifyTestError(ctx, err)
		res.Error = testErrorText(res.Status, res.ErrorClass)
	}
	return res, nil
}

// pingTier is the cheapest call that proves base URL, key and model: one
// reply of at most one token, in the tier's protocol.
func pingTier(ctx context.Context, t Tier) error {
	if t.Provider == ProviderAnthropic {
		client := buildAnthropicClient(t)
		_, err := client.Messages.New(ctx, anthropic.MessageNewParams{
			Model:     anthropic.Model(t.Model),
			MaxTokens: 1,
			Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("ping"))},
		})
		return err
	}
	client := buildClient(t)
	_, err := client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model:     t.Model,
		Messages:  []openai.ChatCompletionMessageParamUnion{openai.UserMessage("ping")},
		MaxTokens: openai.Int(1),
	})
	return err
}

// classifyTestError reduces a test call's error to its HTTP status (0 if
// none) and class. The provider's body isn't kept: it may quote the
// request, the key with it.
func classifyTestError(ctx context.Context, err error) (status int, class string) {
	var oaErr *openai.Error
	var anErr *anthropic.Error
	switch {
	case errors.As(err, &oaErr):
		status = oaErr.StatusCode
	case errors.As(err, &anErr):
		status = anErr.StatusCode
	}
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return status, TestErrorAuth
	case status == http.StatusNotFound:
		return status, TestErrorNotFound
	case status == http.StatusTooManyRequests:
		return status, TestErrorRateLimited
	case status >= 500:
		return status, TestErrorProvider
	case status >= 400:
		return status, TestErrorBadRequest
	case status != 0:
		return status, TestErrorProvider
	case ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded):
		return 0, TestErrorTimeout
	default:
		return 0, TestErrorUnreachable
	}
}

// testErrorText says a test failure in words, from its status and class.
func testErrorText(status int, class string) string {
	var what string
	switch class {
	case TestErrorAuth:
		what = "the provider refused the key"
	case TestErrorNotFound:
		what = "not found: check the base URL and the model"
	case TestErrorRateLimited:
		what = "rate limited by the provider"
	case TestErrorBadRequest:
		what = "the provider refused the request: check the model"
	case TestErrorProvider:
		what = "the provider failed"
	case TestErrorTimeout:
		return "no answer before the timeout"
	default:
		return "couldn't reach the provider"
	}
	return fmt.Sprintf("%s (HTTP %d)", what, status)
}
