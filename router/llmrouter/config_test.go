package llmrouter

import (
	"errors"
	"testing"
)

func setEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

func TestConfigFromEnv_PrimaryOnly(t *testing.T) {
	setEnv(t, map[string]string{
		envPrimaryBaseURL: "https://api.groq.com/openai/v1",
		envPrimaryAPIKey:  "primary-key",
		envPrimaryModel:   "llama-3.1-8b-instant",
	})

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	want := Tier{Provider: ProviderOpenAI, BaseURL: "https://api.groq.com/openai/v1", APIKey: "primary-key", Model: "llama-3.1-8b-instant"}
	if cfg.Primary != want {
		t.Errorf("Primary = %+v, want %+v", cfg.Primary, want)
	}
	if cfg.Escalation != nil {
		t.Errorf("Escalation = %+v, want nil", cfg.Escalation)
	}
}

func TestConfigFromEnv_PrimaryAndEscalation(t *testing.T) {
	setEnv(t, map[string]string{
		envPrimaryBaseURL:    "https://api.groq.com/openai/v1",
		envPrimaryAPIKey:     "primary-key",
		envPrimaryModel:      "llama-3.1-8b-instant",
		envEscalationBaseURL: "https://api.anthropic.com/v1",
		envEscalationAPIKey:  "escalation-key",
		envEscalationModel:   "claude-haiku-4-5",
	})

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if cfg.Escalation == nil {
		t.Fatal("Escalation = nil, want set")
	}
	want := Tier{Provider: ProviderOpenAI, BaseURL: "https://api.anthropic.com/v1", APIKey: "escalation-key", Model: "claude-haiku-4-5"}
	if *cfg.Escalation != want {
		t.Errorf("Escalation = %+v, want %+v", *cfg.Escalation, want)
	}
}

func TestConfigFromEnv_MissingPrimaryVar(t *testing.T) {
	setEnv(t, map[string]string{
		envPrimaryBaseURL: "https://api.groq.com/openai/v1",
		envPrimaryModel:   "llama-3.1-8b-instant",
		// envPrimaryAPIKey deliberately unset
	})

	_, err := ConfigFromEnv()
	if !errors.Is(err, ErrConfigInvalid) {
		t.Fatalf("ConfigFromEnv err = %v, want wrapping ErrConfigInvalid", err)
	}
}

func TestConfigFromEnv_PartialEscalation(t *testing.T) {
	setEnv(t, map[string]string{
		envPrimaryBaseURL:    "https://api.groq.com/openai/v1",
		envPrimaryAPIKey:     "primary-key",
		envPrimaryModel:      "llama-3.1-8b-instant",
		envEscalationBaseURL: "https://api.anthropic.com/v1",
		// envEscalationAPIKey and envEscalationModel deliberately unset
	})

	_, err := ConfigFromEnv()
	if !errors.Is(err, ErrConfigInvalid) {
		t.Fatalf("ConfigFromEnv err = %v, want wrapping ErrConfigInvalid", err)
	}
}

// LOOM-186: an anthropic tier needs no base URL, and the escalation tier
// can speak a different provider from the primary.
func TestConfigFromEnv_AnthropicProvider(t *testing.T) {
	setEnv(t, map[string]string{
		envPrimaryProvider:    "anthropic",
		envPrimaryAPIKey:      "primary-key",
		envPrimaryModel:       "claude-haiku-4-5",
		envEscalationProvider: "anthropic",
		envEscalationAPIKey:   "escalation-key",
		envEscalationModel:    "claude-sonnet-5-5",
	})

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	want := Tier{Provider: ProviderAnthropic, BaseURL: "https://api.anthropic.com", APIKey: "primary-key", Model: "claude-haiku-4-5"}
	if cfg.Primary != want {
		t.Errorf("Primary = %+v, want %+v", cfg.Primary, want)
	}
	if cfg.Escalation == nil || cfg.Escalation.Provider != ProviderAnthropic || cfg.Escalation.Model != "claude-sonnet-5-5" {
		t.Errorf("Escalation = %+v", cfg.Escalation)
	}
}

func TestConfigFromEnv_MixedProviders(t *testing.T) {
	setEnv(t, map[string]string{
		envPrimaryBaseURL:     "https://api.groq.com/openai/v1",
		envPrimaryAPIKey:      "primary-key",
		envPrimaryModel:       "llama-3.1-8b-instant",
		envEscalationProvider: "anthropic",
		envEscalationBaseURL:  "https://llm-proxy.internal",
		envEscalationAPIKey:   "escalation-key",
		envEscalationModel:    "claude-sonnet-5-5",
	})

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if cfg.Primary.Provider != ProviderOpenAI {
		t.Errorf("Primary.Provider = %q, want openai", cfg.Primary.Provider)
	}
	if cfg.Escalation == nil || cfg.Escalation.Provider != ProviderAnthropic || cfg.Escalation.BaseURL != "https://llm-proxy.internal" {
		t.Errorf("Escalation = %+v", cfg.Escalation)
	}
}

func TestConfigFromEnv_UnknownProvider(t *testing.T) {
	setEnv(t, map[string]string{
		envPrimaryProvider: "gemini",
		envPrimaryBaseURL:  "https://example.com",
		envPrimaryAPIKey:   "k",
		envPrimaryModel:    "m",
	})

	if _, err := ConfigFromEnv(); !errors.Is(err, ErrConfigInvalid) {
		t.Fatalf("ConfigFromEnv err = %v, want wrapping ErrConfigInvalid", err)
	}
}

// An openai tier still needs its base URL.
func TestConfigFromEnv_OpenAINeedsBaseURL(t *testing.T) {
	setEnv(t, map[string]string{
		envPrimaryProvider: "openai",
		envPrimaryAPIKey:   "k",
		envPrimaryModel:    "m",
	})

	if _, err := ConfigFromEnv(); !errors.Is(err, ErrConfigInvalid) {
		t.Fatalf("ConfigFromEnv err = %v, want wrapping ErrConfigInvalid", err)
	}
}

// Setting only an escalation provider is a partial configuration.
func TestConfigFromEnv_EscalationProviderOnly(t *testing.T) {
	setEnv(t, map[string]string{
		envPrimaryBaseURL:     "https://api.groq.com/openai/v1",
		envPrimaryAPIKey:      "primary-key",
		envPrimaryModel:       "llama-3.1-8b-instant",
		envEscalationProvider: "anthropic",
	})

	if _, err := ConfigFromEnv(); !errors.Is(err, ErrConfigInvalid) {
		t.Fatalf("ConfigFromEnv err = %v, want wrapping ErrConfigInvalid", err)
	}
}
