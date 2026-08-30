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
	want := Tier{BaseURL: "https://api.groq.com/openai/v1", APIKey: "primary-key", Model: "llama-3.1-8b-instant"}
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
	want := Tier{BaseURL: "https://api.anthropic.com/v1", APIKey: "escalation-key", Model: "claude-haiku-4-5"}
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
