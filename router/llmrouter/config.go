// Package llmrouter is a real, LLM-backed implementation of
// router.RoutingModel (design spec §6): a generic OpenAI-Chat-Completions
// -compatible client parameterized by a primary and an optional escalation
// tier, so swapping vendor/model is a config change, not a code change. See
// README.md for the escalation design and known risks.
package llmrouter

import (
	"errors"
	"fmt"
	"os"
)

// ErrConfigInvalid is wrapped by any error ConfigFromEnv or New returns due
// to missing/malformed configuration — distinct from a runtime Decide/Relay
// failure, so callers can tell "misconfigured" apart from "the model call
// failed" via errors.Is.
var ErrConfigInvalid = errors.New("llmrouter: invalid configuration")

// Tier is one {provider, base URL, API key, model} set identifying an
// endpoint to call. Provider picks the wire protocol (LOOM-186): the
// OpenAI Chat Completions protocol (Groq, Gemini's OpenAI-compatible
// endpoint, or anyone else speaking it) by default, or Anthropic's native
// Messages API.
type Tier struct {
	// Provider is the wire protocol; empty means ProviderOpenAI.
	Provider Provider
	BaseURL  string
	APIKey   string
	Model    string
}

// Validate reports whether t has what its provider needs: an API key and
// a model always, and a base URL for ProviderOpenAI (Anthropic's has a
// default). The error wraps ErrConfigInvalid.
func (t Tier) Validate() error {
	p, err := ParseProvider(string(t.Provider))
	if err != nil {
		return err
	}
	switch {
	case t.BaseURL == "" && p.defaultBaseURL() == "":
		return fmt.Errorf("%w: no base URL", ErrConfigInvalid)
	case t.APIKey == "":
		return fmt.Errorf("%w: no API key", ErrConfigInvalid)
	case t.Model == "":
		return fmt.Errorf("%w: no model", ErrConfigInvalid)
	}
	return nil
}

// Config is the full primary+escalation configuration for a Model.
// Escalation is nil when not configured — Decide/Relay then surface a
// primary-tier failure directly instead of falling back.
type Config struct {
	Primary    Tier
	Escalation *Tier
}

const (
	envPrimaryProvider = "LOOMUX_ROUTER_PRIMARY_PROVIDER"
	envPrimaryBaseURL  = "LOOMUX_ROUTER_PRIMARY_BASE_URL"
	envPrimaryAPIKey   = "LOOMUX_ROUTER_PRIMARY_API_KEY"
	envPrimaryModel    = "LOOMUX_ROUTER_PRIMARY_MODEL"

	envEscalationProvider = "LOOMUX_ROUTER_ESCALATION_PROVIDER"
	envEscalationBaseURL  = "LOOMUX_ROUTER_ESCALATION_BASE_URL"
	envEscalationAPIKey   = "LOOMUX_ROUTER_ESCALATION_API_KEY"
	envEscalationModel    = "LOOMUX_ROUTER_ESCALATION_MODEL"
)

// ConfigFromEnv loads Config from environment variables, failing fast (per
// this repo's established KeyFromEnv convention, registry/sqlite/crypto.go)
// on missing primary configuration or a partially-set escalation tier.
// Escalation is entirely optional: if none of its vars are set,
// Config.Escalation is nil rather than an error.
//
// Each tier's _PROVIDER var (LOOM-186) is "openai" (the default) or
// "anthropic"; an anthropic tier's _BASE_URL may be left unset, for
// Anthropic's own API.
func ConfigFromEnv() (Config, error) {
	primary, err := tierFromEnv(envPrimaryProvider, envPrimaryBaseURL, envPrimaryAPIKey, envPrimaryModel)
	if err != nil {
		return Config{}, err
	}

	set := boolCount(os.Getenv(envEscalationProvider) != "", os.Getenv(envEscalationBaseURL) != "",
		os.Getenv(envEscalationAPIKey) != "", os.Getenv(envEscalationModel) != "")
	if set == 0 {
		return Config{Primary: primary}, nil
	}
	escalation, err := tierFromEnv(envEscalationProvider, envEscalationBaseURL, envEscalationAPIKey, envEscalationModel)
	if err != nil {
		return Config{}, fmt.Errorf("%w (partial escalation-tier configuration: set all of %s/%s/%s, or none)",
			err, envEscalationBaseURL, envEscalationAPIKey, envEscalationModel)
	}
	return Config{Primary: primary, Escalation: &escalation}, nil
}

func tierFromEnv(providerVar, baseURLVar, apiKeyVar, modelVar string) (Tier, error) {
	provider, err := ParseProvider(os.Getenv(providerVar))
	if err != nil {
		return Tier{}, fmt.Errorf("%w (%s)", err, providerVar)
	}
	baseURL := os.Getenv(baseURLVar)
	if baseURL == "" {
		baseURL = provider.defaultBaseURL()
	}
	if baseURL == "" {
		return Tier{}, fmt.Errorf("%w: %s is not set", ErrConfigInvalid, baseURLVar)
	}
	apiKey := os.Getenv(apiKeyVar)
	if apiKey == "" {
		return Tier{}, fmt.Errorf("%w: %s is not set", ErrConfigInvalid, apiKeyVar)
	}
	model := os.Getenv(modelVar)
	if model == "" {
		return Tier{}, fmt.Errorf("%w: %s is not set", ErrConfigInvalid, modelVar)
	}
	return Tier{Provider: provider, BaseURL: baseURL, APIKey: apiKey, Model: model}, nil
}

func boolCount(bs ...bool) int {
	n := 0
	for _, b := range bs {
		if b {
			n++
		}
	}
	return n
}
