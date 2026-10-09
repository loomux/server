package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/router/llmrouter"
)

// RouterSettings backs /api/v1/settings/router (LOOM-185,
// docs/design/router-settings.md): the router model's provider, base URL,
// model and key per tier, changed without a restart. Satisfied by
// *llmrouter.Settings. Keys go in and never come out.
type RouterSettings interface {
	Tiers() []llmrouter.TierView
	Set(ctx context.Context, tier string, u llmrouter.TierUpdate, actor string) (llmrouter.TierView, error)
	Clear(ctx context.Context, tier, actor string) error
	Test(ctx context.Context, tier string) (llmrouter.TestResult, error)
	// Models lists the models the tier's provider offers (LOOM-191).
	Models(ctx context.Context, tier string, req llmrouter.ModelsRequest) (llmrouter.ModelsResult, error)
	Changes(ctx context.Context, limit int) ([]*registry.RouterSettingsChange, error)
}

// WithRouterSettings enables /api/v1/settings/router (LOOM-185).
func WithRouterSettings(rs RouterSettings) Option {
	return func(s *Server) { s.routerSettings = rs }
}

const (
	defaultRouterAuditLimit = 50
	maxRouterAuditLimit     = 200
)

// routerTierResponse is a tier without its key: there is deliberately no
// field the key could go in.
type routerTierResponse struct {
	Tier string `json:"tier"`
	// Source is "stored" (set here), "env" (the environment's) or "none"
	// (escalation configured nowhere: off).
	Source         string `json:"source"`
	Provider       string `json:"provider,omitempty"`
	BaseURL        string `json:"base_url,omitempty"`
	Model          string `json:"model,omitempty"`
	KeyFingerprint string `json:"key_fingerprint,omitempty"`
	// KeyLast4 is the key's last four characters, for keys long enough
	// that they don't give much of it away.
	KeyLast4 string `json:"key_last4,omitempty"`
	// SetAt is when a stored tier was set.
	SetAt *time.Time `json:"set_at,omitempty"`
	// EnvConfigured says whether the environment has this tier to fall
	// back to.
	EnvConfigured bool `json:"env_configured"`
	// StoredUnreadable: a stored tier exists but doesn't decrypt with
	// this server's master key, so the environment's is in use.
	StoredUnreadable bool `json:"stored_unreadable"`
}

func newRouterTierResponse(v llmrouter.TierView) routerTierResponse {
	out := routerTierResponse{Tier: v.Tier, Source: v.Source, Provider: v.Provider, BaseURL: v.BaseURL, Model: v.Model,
		KeyFingerprint: v.KeyFingerprint, KeyLast4: v.KeyLast4, EnvConfigured: v.EnvConfigured, StoredUnreadable: v.StoredUnreadable}
	if !v.SetAt.IsZero() {
		at := v.SetAt.UTC()
		out.SetAt = &at
	}
	return out
}

type routerSettingsResponse struct {
	// Providers are the provider names a tier may have.
	Providers []string             `json:"providers"`
	Tiers     []routerTierResponse `json:"tiers"`
}

// setRouterTierRequest: api_key may be left out to keep the stored key.
type setRouterTierRequest struct {
	Provider string `json:"provider"`
	BaseURL  string `json:"base_url"`
	Model    string `json:"model"`
	APIKey   string `json:"api_key"`
}

type testRouterTierResponse struct {
	OK bool `json:"ok"`
	// Status is the provider's HTTP status on a failure, 0 when none came
	// back (a timeout, an unreachable host).
	Status int `json:"status,omitempty"`
	// ErrorClass is auth_failed, not_found, bad_request, rate_limited,
	// provider_error, timeout or unreachable.
	ErrorClass string `json:"error_class,omitempty"`
	// Error says the failure in words, from Status and ErrorClass alone:
	// never the provider's response body.
	Error      string `json:"error,omitempty"`
	Model      string `json:"model"`
	Source     string `json:"source"`
	DurationMS int64  `json:"duration_ms"`
}

type routerSettingsChangeResponse struct {
	ID     string `json:"id"`
	Tier   string `json:"tier"`
	Action string `json:"action"`
	// Fields name what changed, never the values.
	Fields    []string  `json:"fields"`
	Actor     string    `json:"actor"`
	CreatedAt time.Time `json:"created_at"`
}

type listRouterSettingsChangesResponse struct {
	Entries []routerSettingsChangeResponse `json:"entries"`
}

func (s *Server) routerSettingsEnabled(w http.ResponseWriter) bool {
	if s.routerSettings == nil {
		writeError(w, http.StatusNotFound, "router settings are not available on this server")
		return false
	}
	return true
}

// routerSettingsActor names who made a change for the audit trail: the
// session (device) of the one owner account.
func routerSettingsActor(r *http.Request) string {
	if sess := sessionFromContext(r.Context()); sess != nil {
		return "session:" + sess.ID
	}
	return "unknown"
}

// handleGetRouterSettings: GET /api/v1/settings/router — both tiers,
// which source each comes from, and never a key.
func (s *Server) handleGetRouterSettings(w http.ResponseWriter, r *http.Request) {
	if !s.routerSettingsEnabled(w) {
		return
	}
	views := s.routerSettings.Tiers()
	out := routerSettingsResponse{Providers: []string{}, Tiers: make([]routerTierResponse, 0, len(views))}
	for _, p := range llmrouter.Providers() {
		out.Providers = append(out.Providers, string(p))
	}
	for _, v := range views {
		out.Tiers = append(out.Tiers, newRouterTierResponse(v))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleSetRouterTier: PUT /api/v1/settings/router/{tier} stores the
// tier's config and applies it at once.
func (s *Server) handleSetRouterTier(w http.ResponseWriter, r *http.Request) {
	if !s.routerSettingsEnabled(w) {
		return
	}
	var req setRouterTierRequest
	if !readJSON(w, r, &req) {
		return
	}
	v, err := s.routerSettings.Set(r.Context(), r.PathValue("tier"),
		llmrouter.TierUpdate{Provider: req.Provider, BaseURL: req.BaseURL, Model: req.Model, APIKey: req.APIKey},
		routerSettingsActor(r))
	var invalid *llmrouter.InvalidSettingError
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, newRouterTierResponse(v))
	case errors.Is(err, llmrouter.ErrUnknownTier):
		writeError(w, http.StatusNotFound, "no such router tier (primary or escalation)")
	case errors.As(err, &invalid):
		writeError(w, http.StatusBadRequest, invalid.Reason)
	case errors.Is(err, registry.ErrNoMasterKey):
		writeError(w, http.StatusServiceUnavailable, "storing router keys needs LOOMUX_MASTER_KEY, which this server doesn't have")
	default:
		writeError(w, http.StatusInternalServerError, "could not store router settings")
	}
}

// handleClearRouterTier: DELETE /api/v1/settings/router/{tier} goes back
// to the environment's config for the tier.
func (s *Server) handleClearRouterTier(w http.ResponseWriter, r *http.Request) {
	if !s.routerSettingsEnabled(w) {
		return
	}
	err := s.routerSettings.Clear(r.Context(), r.PathValue("tier"), routerSettingsActor(r))
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, llmrouter.ErrUnknownTier):
		writeError(w, http.StatusNotFound, "no such router tier (primary or escalation)")
	case errors.Is(err, registry.ErrNotFound):
		writeError(w, http.StatusNotFound, "this tier has no stored settings")
	default:
		writeError(w, http.StatusInternalServerError, "could not remove router settings")
	}
}

// handleTestRouterTier: POST /api/v1/settings/router/{tier}/test makes
// one cheap call with the tier's config in force. 200 whether or not the
// provider answered; ok says which.
func (s *Server) handleTestRouterTier(w http.ResponseWriter, r *http.Request) {
	if !s.routerSettingsEnabled(w) {
		return
	}
	res, err := s.routerSettings.Test(r.Context(), r.PathValue("tier"))
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, testRouterTierResponse{OK: res.OK, Status: res.Status, ErrorClass: res.ErrorClass, Error: res.Error, Model: res.Model,
			Source: res.Source, DurationMS: res.Duration.Milliseconds()})
	case errors.Is(err, llmrouter.ErrUnknownTier):
		writeError(w, http.StatusNotFound, "no such router tier (primary or escalation)")
	case errors.Is(err, llmrouter.ErrTierNotConfigured):
		writeError(w, http.StatusNotFound, "this tier is not configured")
	default:
		writeError(w, http.StatusInternalServerError, "could not test the router tier")
	}
}

// listRouterModelsRequest: everything optional. Empty means the tier's
// own provider and base URL with its saved key; listing anywhere else
// needs api_key, which is used for this call and not stored.
type listRouterModelsRequest struct {
	Provider string `json:"provider"`
	BaseURL  string `json:"base_url"`
	APIKey   string `json:"api_key"`
}

type routerModelResponse struct {
	ID string `json:"id"`
	// Name is the provider's display name, when it has one.
	Name string `json:"name,omitempty"`
}

// listRouterModelsResponse: like the test call, a failure is its status
// and class alone, never the provider's body.
type listRouterModelsResponse struct {
	OK         bool                  `json:"ok"`
	Models     []routerModelResponse `json:"models"`
	Status     int                   `json:"status,omitempty"`
	ErrorClass string                `json:"error_class,omitempty"`
	Error      string                `json:"error,omitempty"`
	// Cached says the list is from a call made in the last few minutes.
	Cached bool `json:"cached"`
}

// handleListRouterModels: POST /api/v1/settings/router/{tier}/models
// lists the models the tier's provider offers, for the model picker
// (LOOM-191). POST, not GET, because a key being entered travels in the
// body, never in a URL. 200 whether or not the provider answered.
func (s *Server) handleListRouterModels(w http.ResponseWriter, r *http.Request) {
	if !s.routerSettingsEnabled(w) {
		return
	}
	var req listRouterModelsRequest
	if r.ContentLength != 0 && !readJSON(w, r, &req) {
		return
	}
	res, err := s.routerSettings.Models(r.Context(), r.PathValue("tier"),
		llmrouter.ModelsRequest{Provider: req.Provider, BaseURL: req.BaseURL, APIKey: req.APIKey})
	var invalid *llmrouter.InvalidSettingError
	switch {
	case err == nil:
		out := listRouterModelsResponse{OK: res.OK, Models: make([]routerModelResponse, 0, len(res.Models)),
			Status: res.Status, ErrorClass: res.ErrorClass, Error: res.Error, Cached: res.Cached}
		for _, m := range res.Models {
			out.Models = append(out.Models, routerModelResponse{ID: m.ID, Name: m.Name})
		}
		writeJSON(w, http.StatusOK, out)
	case errors.Is(err, llmrouter.ErrUnknownTier):
		writeError(w, http.StatusNotFound, "no such router tier (primary or escalation)")
	case errors.Is(err, llmrouter.ErrTierNotConfigured):
		writeError(w, http.StatusNotFound, "this tier is not configured; enter an API key to list models")
	case errors.As(err, &invalid):
		writeError(w, http.StatusBadRequest, invalid.Reason)
	default:
		writeError(w, http.StatusInternalServerError, "could not list models")
	}
}

// handleListRouterSettingsChanges: GET /api/v1/settings/router/audit
// ?limit=N — the latest changes, newest first.
func (s *Server) handleListRouterSettingsChanges(w http.ResponseWriter, r *http.Request) {
	if !s.routerSettingsEnabled(w) {
		return
	}
	limit := defaultRouterAuditLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxRouterAuditLimit {
			writeError(w, http.StatusBadRequest, "limit must be a number from 1 to 200")
			return
		}
		limit = n
	}
	changes, err := s.routerSettings.Changes(r.Context(), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list router settings changes")
		return
	}
	out := listRouterSettingsChangesResponse{Entries: make([]routerSettingsChangeResponse, 0, len(changes))}
	for _, c := range changes {
		fields := c.Fields
		if fields == nil {
			fields = []string{}
		}
		out.Entries = append(out.Entries, routerSettingsChangeResponse{ID: c.ID, Tier: c.Tier, Action: c.Action,
			Fields: fields, Actor: c.Actor, CreatedAt: c.CreatedAt})
	}
	writeJSON(w, http.StatusOK, out)
}
