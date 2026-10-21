package llmrouter

import (
	"context"
	"errors"
	"net/http"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
)

// buildClient constructs a Chat-Completions client pointed at tier — the
// one place option.WithBaseURL/WithAPIKey are applied, so Decide and Relay
// share identical client construction for both the primary and escalation
// tiers. Relies on the SDK's default retry policy (MaxRetries: 2) for
// transient 429/5xx errors rather than a hand-rolled retry loop. hc, if
// set, carries the requests (WithHTTPClient).
func buildClient(tier Tier, hc *http.Client) openai.Client {
	opts := []option.RequestOption{
		option.WithBaseURL(tier.BaseURL),
		option.WithAPIKey(tier.APIKey),
	}
	if hc != nil {
		opts = append(opts, option.WithHTTPClient(hc))
	}
	return openai.NewClient(opts...)
}

// openAIExchange speaks the Chat Completions protocol, with tool_choice
// pinned to the one function.
//
// On Anthropic's OpenAI-compatible endpoint (LOOM-187) it also sets
// max_completion_tokens, as the Messages API behind it needs a bound
// (anthropicMaxTokens, the native provider's). Nothing else differs:
// the endpoint takes a forced tool_choice on models that take one
// natively, and the request sends none of the fields it ignores
// (strict, response_format, seed, …) — so, unlike the native provider,
// a tool call isn't held to the schema, and validation plus the
// corrective retry catch what doesn't fit.
type openAIExchange struct {
	client openai.Client
	params openai.ChatCompletionNewParams
}

func newOpenAIExchange(tier Tier, system, user string, tool toolSpec, hc *http.Client) *openAIExchange {
	e := &openAIExchange{
		client: buildClient(tier, hc),
		params: openai.ChatCompletionNewParams{
			Model: tier.Model,
			Messages: []openai.ChatCompletionMessageParamUnion{
				openai.SystemMessage(system),
				openai.UserMessage(user),
			},
			Tools: []openai.ChatCompletionToolUnionParam{tool.openAI()},
			ToolChoice: openai.ToolChoiceOptionFunctionToolChoice(openai.ChatCompletionNamedToolChoiceFunctionParam{
				Name: tool.name,
			}),
		},
	}
	if tier.anthropicCompat() {
		e.params.MaxCompletionTokens = openai.Int(anthropicMaxTokens)
	}
	return e
}

func (e *openAIExchange) ask(ctx context.Context) (answer, error) {
	resp, err := e.client.Chat.Completions.New(ctx, e.params)
	if err != nil {
		return answer{}, err
	}
	if len(resp.Choices) == 0 {
		return answer{}, errors.New("no choices returned")
	}
	a := answer{prompt: resp.Usage.PromptTokens, completion: resp.Usage.CompletionTokens, total: resp.Usage.TotalTokens}
	msg := resp.Choices[0].Message
	if len(msg.ToolCalls) == 0 {
		a.refusal = msg.Refusal
		return a, nil
	}
	a.called, a.args = true, []byte(msg.ToolCalls[0].Function.Arguments)
	return a, nil
}

// reject adds the correction as a user turn; the rejected tool call
// itself isn't replayed.
func (e *openAIExchange) reject(reason string) {
	e.params.Messages = append(e.params.Messages, openai.UserMessage(reason))
}

// openAI is the tool as a Chat Completions function definition.
func (s toolSpec) openAI() openai.ChatCompletionToolUnionParam {
	return openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
		Name:        s.name,
		Description: openai.String(s.description),
		Parameters:  shared.FunctionParameters(s.schema),
	})
}
