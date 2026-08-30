package llmrouter

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/openai/openai-go/v3"
)

// Relay implements router.RoutingModel. It tries the primary tier first,
// escalating to the configured escalation tier (if any) when the primary
// fails (transport/rate-limit) or returns an unusable (empty) response.
func (m *Model) Relay(ctx context.Context, capturedOutput string) (string, error) {
	out, err := m.relayWith(ctx, m.cfg.Primary, m.primaryTimeout, capturedOutput)
	if err == nil {
		return out, nil
	}
	if m.cfg.Escalation == nil {
		return "", fmt.Errorf("llmrouter: relay: primary model: %w", err)
	}

	out, err2 := m.relayWith(ctx, *m.cfg.Escalation, m.escalationTimeout, capturedOutput)
	if err2 != nil {
		return "", fmt.Errorf(
			"llmrouter: relay: primary model failed (%v); escalation model also failed: %w", err, err2)
	}
	return out, nil
}

func (m *Model) relayWith(ctx context.Context, tier Tier, timeout time.Duration, capturedOutput string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	client := buildClient(tier)
	resp, err := client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model: tier.Model,
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(relaySystemPrompt),
			openai.UserMessage(capturedOutput),
		},
	})
	if err != nil {
		return "", fmt.Errorf("call failed: %w", err)
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("no choices returned")
	}

	condensed := strings.TrimSpace(resp.Choices[0].Message.Content)
	if condensed == "" {
		return "", fmt.Errorf("empty response")
	}
	return condensed, nil
}
