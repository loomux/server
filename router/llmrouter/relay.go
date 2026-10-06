package llmrouter

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/openai/openai-go/v3"

	"github.com/Loomux/server/internal/metrics"
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
func (m *Model) Relay(ctx context.Context, in router.RelayInput) (router.RelayResult, error) {
	var err error
	if m.skipPrimary() {
		err = errPrimarySkipped
	} else {
		var result router.RelayResult
		result, err = m.relayWith(ctx, "primary", m.cfg.Primary, m.primaryTimeout, in)
		m.recordPrimary(ctx, err)
		if err == nil {
			return result, nil
		}
	}
	if m.cfg.Escalation == nil {
		m.metrics.RecordRouterCall(metrics.RouterOpRelay, "primary", metrics.OutcomeFailure, m.primaryTimeout)
		return router.RelayResult{}, fmt.Errorf("llmrouter: relay: primary model: %w", err)
	}

	m.metrics.RecordRouterEscalation(metrics.RouterOpRelay)
	result, err2 := m.relayWith(ctx, "escalation", *m.cfg.Escalation, m.escalationTimeout, in)
	if err2 != nil {
		m.metrics.RecordRouterCall(metrics.RouterOpRelay, "escalation", metrics.OutcomeFailure, m.escalationTimeout)
		return router.RelayResult{}, fmt.Errorf(
			"llmrouter: relay: primary model failed (%v); escalation model also failed: %w", err, err2)
	}
	return result, nil
}

func (m *Model) relayWith(ctx context.Context, tierName string, tier Tier, timeout time.Duration, in router.RelayInput) (router.RelayResult, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	client := buildClient(tier)
	resp, err := client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model: tier.Model,
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(relaySystemPrompt),
			openai.UserMessage(relayUserPrompt(in)),
		},
		Tools: []openai.ChatCompletionToolUnionParam{buildRelayTool()},
		ToolChoice: openai.ToolChoiceOptionFunctionToolChoice(openai.ChatCompletionNamedToolChoiceFunctionParam{
			Name: relayToolName,
		}),
	})
	duration := time.Since(start)
	if err != nil {
		m.metrics.RecordRouterCall(metrics.RouterOpRelay, tierName, metrics.OutcomeFailure, duration)
		return router.RelayResult{}, &unavailableError{fmt.Errorf("call failed: %w", err)}
	}
	if len(resp.Choices) == 0 {
		m.metrics.RecordRouterCall(metrics.RouterOpRelay, tierName, metrics.OutcomeFailure, duration)
		return router.RelayResult{}, &unavailableError{fmt.Errorf("no choices returned")}
	}

	toolCalls := resp.Choices[0].Message.ToolCalls
	if len(toolCalls) == 0 {
		m.metrics.RecordRouterCall(metrics.RouterOpRelay, tierName, metrics.OutcomeFailure, duration)
		return router.RelayResult{}, fmt.Errorf("model did not call %s", relayToolName)
	}

	var args relayArguments
	if err := json.Unmarshal([]byte(toolCalls[0].Function.Arguments), &args); err != nil {
		m.metrics.RecordRouterCall(metrics.RouterOpRelay, tierName, metrics.OutcomeFailure, duration)
		return router.RelayResult{}, fmt.Errorf("unparseable tool call arguments: %w", err)
	}

	reply := strings.TrimSpace(args.Reply)
	if reply == "" {
		m.metrics.RecordRouterCall(metrics.RouterOpRelay, tierName, metrics.OutcomeFailure, duration)
		return router.RelayResult{}, fmt.Errorf("empty reply")
	}
	m.metrics.RecordRouterCall(metrics.RouterOpRelay, tierName, metrics.OutcomeSuccess, duration)
	if resp.Usage.TotalTokens > 0 {
		m.metrics.RecordRouterTokens(tierName, resp.Usage.PromptTokens, resp.Usage.CompletionTokens, resp.Usage.TotalTokens)
	}
	return router.RelayResult{Reply: reply, Done: args.Done}, nil
}

// relayUserPrompt lays out a relay call's input (LOOM-112): the turn's
// context first, the captured output last, each under a plain heading.
func relayUserPrompt(in router.RelayInput) string {
	var b strings.Builder
	if in.AgentType != "" {
		fmt.Fprintf(&b, "Agent: %s\n\n", in.AgentType)
	}
	if in.UserMessage != "" {
		fmt.Fprintf(&b, "The user's message that started this turn:\n%s\n\n", in.UserMessage)
	} else {
		b.WriteString("No new message started this: the agent wrote this output after its turn had ended.\n\n")
	}
	if in.PreviousSummary != "" {
		fmt.Fprintf(&b, "The workspace's summary before this turn:\n%s\n\n", in.PreviousSummary)
	}
	fmt.Fprintf(&b, "Captured output:\n%s", in.Captured)
	return b.String()
}
