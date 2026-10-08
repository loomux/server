package llmrouter

import (
	"context"
	"errors"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
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

// openAIExchange speaks the Chat Completions protocol, with tool_choice
// pinned to the one function.
type openAIExchange struct {
	client openai.Client
	params openai.ChatCompletionNewParams
}

func newOpenAIExchange(tier Tier, system, user string, tool toolSpec) *openAIExchange {
	return &openAIExchange{
		client: buildClient(tier),
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
