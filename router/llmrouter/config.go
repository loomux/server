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

// Tier is one {base URL, API key, model} triple identifying an
// OpenAI-Chat-Completions-compatible endpoint to call (e.g. Groq, Gemini's
// OpenAI-compatible endpoint, or any other provider speaking the same wire
// protocol).
type Tier struct {
	BaseURL string
	APIKey  string
	Model   string
}

// Config is the full primary+escalation configuration for a Model.
// Escalation is nil when not configured — Decide/Relay then surface a
// primary-tier failure directly instead of falling back.
type Config struct {
	Primary    Tier
	Escalation *Tier
}

const (
	envPrimaryBaseURL = "LOOMUX_ROUTER_PRIMARY_BASE_URL"
	envPrimaryAPIKey  = "LOOMUX_ROUTER_PRIMARY_API_KEY"
	envPrimaryModel   = "LOOMUX_ROUTER_PRIMARY_MODEL"

	envEscalationBaseURL = "LOOMUX_ROUTER_ESCALATION_BASE_URL"
	envEscalationAPIKey  = "LOOMUX_ROUTER_ESCALATION_API_KEY"
	envEscalationModel   = "LOOMUX_ROUTER_ESCALATION_MODEL"
)

// ConfigFromEnv loads Config from environment variables, failing fast (per
// this repo's established KeyFromEnv convention, registry/sqlite/crypto.go)
// on missing primary configuration or a partially-set escalation tier.
// Escalation is entirely optional: if none of its three vars are set,
// Config.Escalation is nil rather than an error.
func ConfigFromEnv() (Config, error) {
	primary, err := tierFromEnv(envPrimaryBaseURL, envPrimaryAPIKey, envPrimaryModel)
	if err != nil {
		return Config{}, err
	}

	baseURL := os.Getenv(envEscalationBaseURL)
	apiKey := os.Getenv(envEscalationAPIKey)
	model := os.Getenv(envEscalationModel)
	switch boolCount(baseURL != "", apiKey != "", model != "") {
	case 0:
		return Config{Primary: primary}, nil
	case 3:
		return Config{Primary: primary, Escalation: &Tier{BaseURL: baseURL, APIKey: apiKey, Model: model}}, nil
	default:
		return Config{}, fmt.Errorf("%w: partial escalation-tier configuration (set all of %s/%s/%s, or none)",
			ErrConfigInvalid, envEscalationBaseURL, envEscalationAPIKey, envEscalationModel)
	}
}

func tierFromEnv(baseURLVar, apiKeyVar, modelVar string) (Tier, error) {
	baseURL := os.Getenv(baseURLVar)
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
	return Tier{BaseURL: baseURL, APIKey: apiKey, Model: model}, nil
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
