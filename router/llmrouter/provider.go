package llmrouter

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Loomux/server/internal/metrics"
)

// Provider names the wire protocol a tier speaks (LOOM-186). Everything
// above one exchange — prompts, schemas, validation, the corrective
// retry, escalation, the breaker — is shared; only the request/response
// shape differs per provider.
type Provider string

const (
	// ProviderOpenAI is the OpenAI Chat Completions protocol, spoken by
	// OpenAI and by many others (Groq, Gemini's compatible endpoint,
	// OpenRouter, Ollama, vLLM, …). The default.
	ProviderOpenAI Provider = "openai"
	// ProviderAnthropic is Anthropic's native Messages API.
	ProviderAnthropic Provider = "anthropic"
)

// anthropicDefaultBaseURL is Anthropic's own API, used when an anthropic
// tier names no base URL.
const anthropicDefaultBaseURL = "https://api.anthropic.com"

// Providers lists the supported providers, e.g. for a settings form to
// offer (LOOM-185).
func Providers() []Provider { return []Provider{ProviderOpenAI, ProviderAnthropic} }

// ParseProvider reads a provider name, case-insensitively; empty means
// ProviderOpenAI. An unknown name wraps ErrConfigInvalid.
func ParseProvider(s string) (Provider, error) {
	switch p := Provider(strings.ToLower(strings.TrimSpace(s))); p {
	case "":
		return ProviderOpenAI, nil
	case ProviderOpenAI, ProviderAnthropic:
		return p, nil
	default:
		return "", fmt.Errorf("%w: unknown provider %q (want %q or %q)", ErrConfigInvalid, s, ProviderOpenAI, ProviderAnthropic)
	}
}

// defaultBaseURL is the base URL a tier gets when it names none, "" when
// it must name one.
func (p Provider) defaultBaseURL() string {
	if p == ProviderAnthropic {
		return anthropicDefaultBaseURL
	}
	return ""
}

// toolSpec is the one function a Decide or Relay call asks the model to
// call: its name, description and JSON Schema parameters, in the
// provider-neutral form each exchange translates.
type toolSpec struct {
	name        string
	description string
	schema      map[string]any
}

// exchange is one Decide or Relay conversation with one tier: the system
// prompt, the user turn and the tool, plus anything a corrective retry
// adds.
type exchange interface {
	// ask sends the conversation so far. An error means no answer came
	// back at all (after the SDK's own retries of 429/5xx/connection
	// failures); an answer that can't be used is reported in it instead.
	ask(ctx context.Context) (answer, error)
	// reject tells the model why its last answer couldn't be used, so
	// the next ask gets a corrected one.
	reject(reason string)
}

// answer is what one ask got back.
type answer struct {
	// args are the tool call's arguments; called is false when the model
	// answered without calling the tool.
	args   json.RawMessage
	called bool
	// refusal is set when the provider declined to answer (Anthropic's
	// stop_reason "refusal", OpenAI's message refusal): asking again
	// won't help, so it isn't retried on the same tier.
	refusal string
	// stop is the provider's stop reason, reported when there was no
	// tool call (e.g. a reply cut off at max_tokens).
	stop                      string
	prompt, completion, total int64
}

// newExchange starts an exchange in tier's protocol.
func newExchange(tier Tier, system, user string, tool toolSpec) exchange {
	if tier.Provider == ProviderAnthropic {
		return newAnthropicExchange(tier, system, user, tool)
	}
	return newOpenAIExchange(tier, system, user, tool)
}

// converse runs one tier's round trip for op: ask, hand the tool call to
// accept, and if the answer can't be used — no tool call, or accept
// refuses it — ask once more on the same tier, saying what was wrong
// (LOOM-107). A refusal isn't retried. correction words the retry. Both
// asks share ctx's deadline.
func (m *Model) converse(ctx context.Context, op, tierName string, ex exchange, toolName string, correction func(problem error) string, accept func(args json.RawMessage) error) error {
	for attempt := 0; ; attempt++ {
		start := time.Now()
		ans, err := ex.ask(ctx)
		duration := time.Since(start)
		if err != nil {
			m.metrics.RecordRouterCall(op, tierName, metrics.OutcomeFailure, duration)
			return &unavailableError{fmt.Errorf("call failed: %w", err)}
		}
		if ans.total > 0 {
			m.metrics.RecordRouterTokens(tierName, ans.prompt, ans.completion, ans.total)
		}

		var problem error
		switch {
		case ans.refusal != "":
			problem = fmt.Errorf("model refused: %s", ans.refusal)
		case !ans.called && ans.stop != "":
			problem = fmt.Errorf("model did not call %s (stop reason %s)", toolName, ans.stop)
		case !ans.called:
			problem = fmt.Errorf("model did not call %s", toolName)
		default:
			problem = accept(ans.args)
		}
		if problem == nil {
			m.metrics.RecordRouterCall(op, tierName, metrics.OutcomeSuccess, duration)
			return nil
		}
		m.metrics.RecordRouterCall(op, tierName, metrics.OutcomeFailure, duration)
		if ans.refusal != "" || attempt > 0 || ctx.Err() != nil {
			return problem
		}
		m.metrics.RecordRouterRetry(op, tierName)
		ex.reject(correction(problem))
	}
}
