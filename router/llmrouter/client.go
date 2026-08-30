package llmrouter

import (
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

// buildClient constructs a Chat-Completions client pointed at tier — the
// one place option.WithBaseURL/WithAPIKey are applied, so Decide and Relay
// share identical client construction for both the primary and escalation
// tiers. Relies on the SDK's default retry policy (MaxRetries: 2) for
// transient 429/5xx errors rather than a hand-rolled retry loop.
func buildClient(tier Tier) openai.Client {
	return openai.NewClient(
		option.WithBaseURL(tier.BaseURL),
		option.WithAPIKey(tier.APIKey),
	)
}
