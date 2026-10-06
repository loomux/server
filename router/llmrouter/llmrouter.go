package llmrouter

import (
	"fmt"
	"time"

	"github.com/Loomux/server/internal/metrics"
	"github.com/Loomux/server/router"
)

const (
	// defaultPrimaryTimeout bounds the hot-path tier's per-call latency —
	// Decide/Relay run once per chat message, so this stays tight.
	defaultPrimaryTimeout = 15 * time.Second
	// defaultEscalationTimeout is looser: escalation only runs when the
	// primary tier has already failed, so a slower/larger model is
	// tolerable here.
	defaultEscalationTimeout = 45 * time.Second
)

// Model is a real, LLM-backed router.RoutingModel: Decide/Relay each try a
// cheap/fast "primary" tier first, escalating to a configured "escalation"
// tier when the primary's output is unusable or the primary is
// unavailable. See README.md for the full design.
type Model struct {
	cfg               Config
	agentTypes        []string
	primaryTimeout    time.Duration
	escalationTimeout time.Duration
	metrics           *metrics.Metrics
	// agentDescriptions is a one-line description per agent type for the
	// system prompt (LOOM-88), so the model can tell them apart.
	agentDescriptions map[string]string
	// primaryBreaker skips the primary tier during an outage (LOOM-107).
	primaryBreaker *breaker
}

// Option configures a Model constructed via New.
type Option func(*Model)

// WithPrimaryTimeout overrides the default per-call timeout for the
// primary tier.
func WithPrimaryTimeout(d time.Duration) Option {
	return func(m *Model) { m.primaryTimeout = d }
}

// WithEscalationTimeout overrides the default per-call timeout for the
// escalation tier.
func WithEscalationTimeout(d time.Duration) Option {
	return func(m *Model) { m.escalationTimeout = d }
}

// WithAgentDescriptions sets each agent type's one-line description,
// listed in the routing system prompt (LOOM-88). An agent type without
// one is listed by name only.
func WithAgentDescriptions(d map[string]string) Option {
	return func(m *Model) { m.agentDescriptions = d }
}

// WithMetrics sets the Prometheus metrics bundle the model should record
// into (LOOM-103). A nil value is accepted and ignored.
func WithMetrics(met *metrics.Metrics) Option {
	return func(m *Model) { m.metrics = met }
}

// New constructs a Model. agentTypes is the set of registered agent-type
// names (router.AgentTypeRegistry's keys) — RoutingModel's interface never
// carries these, so the caller wiring up router.New(...) must supply the
// same names here too; Decide's agent_type choice is constrained to them.
// An empty agentTypes is a legal, if degenerate, configuration: Decide can
// then only ever legally return answer_directly or provision_workspace
// with no agent_type — any use_workspace/provision_workspace decision that
// names one fails validation.
func New(cfg Config, agentTypes []string, opts ...Option) (*Model, error) {
	if cfg.Primary.BaseURL == "" || cfg.Primary.APIKey == "" || cfg.Primary.Model == "" {
		return nil, fmt.Errorf("%w: primary tier is incompletely configured", ErrConfigInvalid)
	}

	m := &Model{
		cfg:               cfg,
		agentTypes:        agentTypes,
		primaryTimeout:    defaultPrimaryTimeout,
		escalationTimeout: defaultEscalationTimeout,
		primaryBreaker:    newBreaker(),
	}
	for _, opt := range opts {
		opt(m)
	}
	return m, nil
}

func (m *Model) isValidAgentType(agentType string) bool {
	for _, at := range m.agentTypes {
		if at == agentType {
			return true
		}
	}
	return false
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

var _ router.RoutingModel = (*Model)(nil)
