package llmrouter

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/openai/openai-go/v3"

	"github.com/Loomux/server/router"
)

// relayArguments mirrors router.RelayResult, the shape condense_output's
// tool schema asks the model to fill in.
type relayArguments struct {
	Reply string `json:"reply"`
	Done  bool   `json:"done"`
}

// Relay implements router.RoutingModel. It tries the primary tier first,
// escalating to the configured escalation tier (if any) when the primary
// fails (transport/rate-limit) or returns an unusable (unparseable/
// invalid tool call, or an empty reply).
func (m *Model) Relay(ctx context.Context, capturedOutput string) (router.RelayResult, error) {
	result, err := m.relayWith(ctx, m.cfg.Primary, m.primaryTimeout, capturedOutput)
	if err == nil {
		return result, nil
	}
	if m.cfg.Escalation == nil {
		return router.RelayResult{}, fmt.Errorf("llmrouter: relay: primary model: %w", err)
	}

	result, err2 := m.relayWith(ctx, *m.cfg.Escalation, m.escalationTimeout, capturedOutput)
	if err2 != nil {
		return router.RelayResult{}, fmt.Errorf(
			"llmrouter: relay: primary model failed (%v); escalation model also failed: %w", err, err2)
	}
	return result, nil
}

func (m *Model) relayWith(ctx context.Context, tier Tier, timeout time.Duration, capturedOutput string) (router.RelayResult, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	client := buildClient(tier)
	resp, err := client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model: tier.Model,
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(relaySystemPrompt),
			openai.UserMessage(capturedOutput),
		},
		Tools: []openai.ChatCompletionToolUnionParam{buildRelayTool()},
		ToolChoice: openai.ToolChoiceOptionFunctionToolChoice(openai.ChatCompletionNamedToolChoiceFunctionParam{
			Name: relayToolName,
		}),
	})
	if err != nil {
		return router.RelayResult{}, fmt.Errorf("call failed: %w", err)
	}
	if len(resp.Choices) == 0 {
		return router.RelayResult{}, fmt.Errorf("no choices returned")
	}

	toolCalls := resp.Choices[0].Message.ToolCalls
	if len(toolCalls) == 0 {
		return router.RelayResult{}, fmt.Errorf("model did not call %s", relayToolName)
	}

	var args relayArguments
	if err := json.Unmarshal([]byte(toolCalls[0].Function.Arguments), &args); err != nil {
		return router.RelayResult{}, fmt.Errorf("unparseable tool call arguments: %w", err)
	}

	reply := strings.TrimSpace(args.Reply)
	if reply == "" {
		return router.RelayResult{}, fmt.Errorf("empty reply")
	}
	return router.RelayResult{Reply: reply, Done: args.Done}, nil
}
