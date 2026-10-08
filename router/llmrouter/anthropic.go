package llmrouter

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	aoption "github.com/anthropics/anthropic-sdk-go/option"
)

// anthropicMaxTokens bounds one Decide/Relay reply, adaptive thinking
// included: a routing decision or condensed reply is a few hundred
// tokens, and low effort keeps thinking short, so this is headroom
// against a reply cut off mid-tool-call, not a target.
const anthropicMaxTokens = 8192

// anthropicMaxRetries is the SDK's own retry budget for 408/409/429/5xx
// and connection failures (honouring retry-after); a 400-class error
// isn't retried. Matches the OpenAI SDK's default.
const anthropicMaxRetries = 2

// anthropicExchange speaks Anthropic's native Messages API (LOOM-186).
//
// Current Claude models (Sonnet 5.5, Opus 5.5, Fable 5.1) reject a forced
// tool_choice ("any"/"tool") with a 400, so the tool is offered with
// tool_choice auto and strict: true — any call it makes matches the
// schema — and the system prompt says to call it. A reply without the
// call is caught by converse and retried once.
type anthropicExchange struct {
	client   anthropic.Client
	params   anthropic.MessageNewParams
	toolName string
	// last is the previous reply and lastToolUseID its tool call, if it
	// made one: reject replays the reply unchanged (thinking blocks
	// included) and answers that call.
	last          *anthropic.Message
	lastToolUseID string
}

func newAnthropicExchange(tier Tier, system, user string, tool toolSpec) *anthropicExchange {
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(tier.Model),
		MaxTokens: anthropicMaxTokens,
		// tools and system render first and only change when the agent
		// types, workspaces or targets do: the breakpoint on system
		// caches both across messages.
		System: []anthropic.TextBlockParam{{
			Text:         system + "\n\nAlways respond by calling the " + tool.name + " tool, exactly once; never answer in plain text.",
			CacheControl: anthropic.NewCacheControlEphemeralParam(),
		}},
		Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(user))},
		Tools: []anthropic.ToolUnionParam{{OfTool: &anthropic.ToolParam{
			Name:        tool.name,
			Description: anthropic.String(tool.description),
			Strict:      anthropic.Bool(true),
			InputSchema: anthropicInputSchema(tool.schema),
		}}},
		ToolChoice: anthropic.ToolChoiceUnionParam{OfAuto: &anthropic.ToolChoiceAutoParam{
			DisableParallelToolUse: anthropic.Bool(true),
		}},
	}
	if supportsEffort(tier.Model) {
		// Routing and condensing are simple: low effort keeps thinking
		// (and latency) down.
		params.OutputConfig = anthropic.OutputConfigParam{Effort: anthropic.OutputConfigEffortLow}
	}
	return &anthropicExchange{client: buildAnthropicClient(tier), params: params, toolName: tool.name}
}

// buildAnthropicClient is the Messages-API counterpart of buildClient.
// A base URL ending in /v1 (as an OpenAI-style one would) is trimmed:
// the SDK adds /v1/messages itself.
func buildAnthropicClient(tier Tier) anthropic.Client {
	opts := []aoption.RequestOption{
		aoption.WithAPIKey(tier.APIKey),
		aoption.WithMaxRetries(anthropicMaxRetries),
	}
	if base := strings.TrimSuffix(strings.TrimSuffix(tier.BaseURL, "/"), "/v1"); base != "" {
		opts = append(opts, aoption.WithBaseURL(base))
	}
	return anthropic.NewClient(opts...)
}

func (e *anthropicExchange) ask(ctx context.Context) (answer, error) {
	resp, err := e.client.Messages.New(ctx, e.params)
	if err != nil {
		return answer{}, describeAnthropicError(err)
	}
	e.last, e.lastToolUseID = resp, ""

	u := resp.Usage
	prompt := u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
	a := answer{prompt: prompt, completion: u.OutputTokens, total: prompt + u.OutputTokens}

	// A refusal's content may be partial or empty: check it first.
	if resp.StopReason == anthropic.StopReasonRefusal {
		a.refusal = "stop_reason refusal"
		if c := string(resp.StopDetails.Category); c != "" {
			a.refusal += " (" + c + ")"
		}
		if x := resp.StopDetails.Explanation; x != "" {
			a.refusal += ": " + x
		}
		return a, nil
	}
	for _, block := range resp.Content {
		if tu, ok := block.AsAny().(anthropic.ToolUseBlock); ok && tu.Name == e.toolName {
			a.called, a.args = true, tu.Input
			e.lastToolUseID = tu.ID
			return a, nil
		}
	}
	a.stop = string(resp.StopReason)
	return a, nil
}

// reject appends the rejected reply as it came back, then the
// correction: as the tool call's error result when there was a call, as
// plain text when there wasn't.
func (e *anthropicExchange) reject(reason string) {
	if e.last == nil {
		return
	}
	e.params.Messages = append(e.params.Messages, e.last.ToParam())
	if e.lastToolUseID != "" {
		e.params.Messages = append(e.params.Messages, anthropic.NewUserMessage(anthropic.NewToolResultBlock(e.lastToolUseID, reason, true)))
	} else {
		e.params.Messages = append(e.params.Messages, anthropic.NewUserMessage(anthropic.NewTextBlock(reason)))
	}
	e.last, e.lastToolUseID = nil, ""
}

// describeAnthropicError names an API error's status and type (e.g.
// "429 rate_limit_error", "400 invalid_request_error"); the SDK has
// already retried the retryable ones by the time it returns.
func describeAnthropicError(err error) error {
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		return fmt.Errorf("anthropic: %d %s: %w", apiErr.StatusCode, apiErr.Type(), err)
	}
	return fmt.Errorf("anthropic: %w", err)
}

// noEffortModels are the Claude models that reject output_config.effort
// (it errors on Haiku 4.5 and Sonnet 4.5, and older models predate it).
// Matched as substrings, so provider-prefixed ids are covered too.
var noEffortModels = []string{
	"claude-3",
	"claude-haiku-4-5",
	"claude-sonnet-4-5",
	"claude-sonnet-4-0", "claude-sonnet-4-2025",
	"claude-opus-4-0", "claude-opus-4-1", "claude-opus-4-2025",
}

// supportsEffort reports whether model takes output_config.effort.
// Newer models than this list knows are assumed to.
func supportsEffort(model string) bool {
	model = strings.ToLower(model)
	for _, m := range noEffortModels {
		if strings.Contains(model, m) {
			return false
		}
	}
	return true
}

// anthropicInputSchema converts a tool schema for strict tool use: every
// object closed with additionalProperties: false, as strict mode
// requires, and string patterns dropped, since the regex dialect strict
// mode compiles is narrower than JSON Schema's — the values are
// validated in Go after parsing anyway (router.ProvisionSpec.Validate).
func anthropicInputSchema(schema map[string]any) anthropic.ToolInputSchemaParam {
	s := strictSchema(schema).(map[string]any)
	in := anthropic.ToolInputSchemaParam{Properties: s["properties"], ExtraFields: map[string]any{"additionalProperties": false}}
	if req, ok := s["required"].([]string); ok {
		in.Required = req
	}
	return in
}

// strictSchema returns a copy of a JSON Schema node with objects closed
// and patterns removed, leaving the input untouched.
func strictSchema(node any) any {
	switch n := node.(type) {
	case map[string]any:
		out := make(map[string]any, len(n)+1)
		for k, v := range n {
			if _, isRegex := v.(string); k == "pattern" && isRegex {
				continue
			}
			out[k] = strictSchema(v)
		}
		if n["type"] == "object" {
			out["additionalProperties"] = false
		}
		return out
	case []any:
		out := make([]any, len(n))
		for i, v := range n {
			out[i] = strictSchema(v)
		}
		return out
	default:
		return node
	}
}
