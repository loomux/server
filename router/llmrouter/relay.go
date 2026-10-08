package llmrouter

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

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
	// One config for the whole call: a change (LOOM-185) applies from
	// the next one.
	state := m.cfg.Load()
	cfg := &state.Config
	var err error
	if m.skipPrimary(cfg) {
		err = errPrimarySkipped
	} else {
		var result router.RelayResult
		result, err = m.relayWith(ctx, "primary", cfg.Primary, m.primaryTimeout, in)
		m.recordPrimary(ctx, err, state.gen)
		if err == nil {
			return result, nil
		}
	}
	if cfg.Escalation == nil {
		m.metrics.RecordRouterCall(metrics.RouterOpRelay, "primary", metrics.OutcomeFailure, m.primaryTimeout)
		return router.RelayResult{}, scrubKeys(fmt.Errorf("llmrouter: relay: primary model: %w", err))
	}

	m.metrics.RecordRouterEscalation(metrics.RouterOpRelay)
	result, err2 := m.relayWith(ctx, "escalation", *cfg.Escalation, m.escalationTimeout, in)
	if err2 != nil {
		m.metrics.RecordRouterCall(metrics.RouterOpRelay, "escalation", metrics.OutcomeFailure, m.escalationTimeout)
		return router.RelayResult{}, scrubKeys(fmt.Errorf(
			"llmrouter: relay: primary model failed (%v); escalation model also failed: %w", err, err2))
	}
	return result, nil
}

func (m *Model) relayWith(ctx context.Context, tierName string, tier Tier, timeout time.Duration, in router.RelayInput) (router.RelayResult, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ex := newExchange(tier, relaySystemPrompt, relayUserPrompt(in), relayTool())
	var result router.RelayResult
	err := m.converse(ctx, metrics.RouterOpRelay, tierName, ex, relayToolName, relayCorrectionPrompt, func(raw json.RawMessage) error {
		var args relayArguments
		if err := json.Unmarshal(raw, &args); err != nil {
			return fmt.Errorf("unparseable tool call arguments: %w", err)
		}
		reply := strings.TrimSpace(args.Reply)
		if reply == "" {
			return fmt.Errorf("empty reply")
		}
		result = router.RelayResult{Reply: reply, Done: args.Done, Model: tier.Model, Tier: tierName}
		return nil
	})
	if err != nil {
		return router.RelayResult{}, err
	}
	return result, nil
}

// relayCorrectionPrompt tells the model why its condense_output call was
// refused, for the corrective retry.
func relayCorrectionPrompt(problem error) string {
	return "Your previous answer was rejected: " + problem.Error() +
		". Call " + relayToolName + " again with a non-empty reply and done."
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
