// Package metrics owns Loomux's Prometheus instrumentation. All metric
// names are prefixed with "loomux_" and keep cardinality bounded: no
// conversation IDs, task IDs, workspace IDs, or free-form strings (host
// names, error messages) appear as label values.
package metrics

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Outcome values keep cardinality small and stable.
const (
	OutcomeSuccess = "success"
	OutcomeFailure = "failure"
)

// Router op names.
const (
	RouterOpDecide = "decide"
	RouterOpRelay  = "relay"
)

// Token kind values.
const (
	TokenKindPrompt     = "prompt"
	TokenKindCompletion = "completion"
	TokenKindTotal      = "total"
)

// Task status label for the terminal "failed" state.
const TaskStatusFailed = "failed"

// Metrics holds all Prometheus collectors used across loomuxd. A nil
// *Metrics is safe to use; every method no-ops so callers never need a
// nil check.
type Metrics struct {
	reg                *prometheus.Registry
	DispatchTotal      *prometheus.CounterVec
	DispatchDuration   *prometheus.HistogramVec
	RoutingDecisions   *prometheus.CounterVec
	RouterCallsTotal   *prometheus.CounterVec
	RouterCallDuration *prometheus.HistogramVec
	RouterTokensTotal  *prometheus.CounterVec
	RouterEscalations  *prometheus.CounterVec
	TaskTransitions    *prometheus.CounterVec
	TasksByStatus      *prometheus.GaugeVec
	TargetUp           *prometheus.GaugeVec
	TargetDiskFree     *prometheus.GaugeVec
	TargetOpDuration   *prometheus.HistogramVec
	TargetOpErrors     *prometheus.CounterVec
	ReaperTasksReaped  prometheus.Counter
}

// NewMetrics creates a Metrics bundle and registers its collectors with
// the provided registry.
func NewMetrics(reg *prometheus.Registry) *Metrics {
	factory := promauto.With(reg)
	return &Metrics{
		reg: reg,
		DispatchTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "loomux_dispatch_total",
			Help: "Total dispatch attempts by action, outcome, and error class.",
		}, []string{"action", "outcome", "error_class"}),
		DispatchDuration: factory.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "loomux_dispatch_stage_seconds",
			Help:    "Dispatch stage latency distribution in seconds.",
			Buckets: prometheus.DefBuckets,
		}, []string{"stage"}),
		RoutingDecisions: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "loomux_routing_decisions_total",
			Help: "Total routing decisions by action.",
		}, []string{"action"}),
		RouterCallsTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "loomux_router_calls_total",
			Help: "Total router model calls by op, tier, and outcome.",
		}, []string{"op", "tier", "outcome"}),
		RouterCallDuration: factory.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "loomux_router_call_seconds",
			Help:    "Router model call latency distribution in seconds by op.",
			Buckets: prometheus.DefBuckets,
		}, []string{"op"}),
		RouterTokensTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "loomux_router_tokens_total",
			Help: "Total LLM tokens consumed by tier and kind.",
		}, []string{"tier", "kind"}),
		RouterEscalations: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "loomux_router_escalations_total",
			Help: "Total router model escalations by op.",
		}, []string{"op"}),
		TaskTransitions: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "loomux_task_transitions_total",
			Help: "Total task status transitions by from/to status and task kind.",
		}, []string{"from", "to", "kind"}),
		TasksByStatus: factory.NewGaugeVec(prometheus.GaugeOpts{
			Name: "loomux_tasks",
			Help: "Current number of tasks by status.",
		}, []string{"status"}),
		TargetUp: factory.NewGaugeVec(prometheus.GaugeOpts{
			Name: "loomux_target_up",
			Help: "Whether a target is reachable (1) or not (0).",
		}, []string{"target", "kind"}),
		TargetDiskFree: factory.NewGaugeVec(prometheus.GaugeOpts{
			Name: "loomux_target_disk_free_bytes",
			Help: "Bytes free on the filesystem holding a target's workspace root, as last probed.",
		}, []string{"target"}),
		TargetOpDuration: factory.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "loomux_target_op_seconds",
			Help:    "Target operation latency distribution in seconds by target kind and operation.",
			Buckets: prometheus.DefBuckets,
		}, []string{"kind", "op"}),
		TargetOpErrors: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "loomux_target_op_errors_total",
			Help: "Total target operation errors by target kind, operation, and reason.",
		}, []string{"kind", "op", "reason"}),
		ReaperTasksReaped: factory.NewCounter(prometheus.CounterOpts{
			Name: "loomux_reaper_tasks_reaped_total",
			Help: "Total idle tasks reaped.",
		}),
	}
}

// NewDiscard creates a Metrics whose collectors are registered with a
// fresh, isolated registry. Useful in tests that must not touch the
// global default registry.
func NewDiscard() *Metrics {
	return NewMetrics(prometheus.NewRegistry())
}

// Handler returns an http.Handler that exposes the metrics on /metrics.
// A nil receiver returns a handler that always responds 503.
func (m *Metrics) Handler() http.Handler {
	if m == nil || m.reg == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "metrics unavailable", http.StatusServiceUnavailable)
		})
	}
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}

// RecordDispatch records the completion of a top-level dispatch.
func (m *Metrics) RecordDispatch(action, outcome, errorClass string) {
	if m == nil {
		return
	}
	if action == "" {
		action = "unknown"
	}
	if outcome == "" {
		outcome = OutcomeFailure
	}
	m.DispatchTotal.WithLabelValues(action, outcome, errorClass).Inc()
}

// RecordDispatchDuration records how long a dispatch stage took.
func (m *Metrics) RecordDispatchDuration(stage string, d time.Duration) {
	if m == nil || stage == "" {
		return
	}
	m.DispatchDuration.WithLabelValues(stage).Observe(d.Seconds())
}

// RecordRoutingDecision records that the router chose an action.
func (m *Metrics) RecordRoutingDecision(action string) {
	if m == nil {
		return
	}
	if action == "" {
		action = "unknown"
	}
	m.RoutingDecisions.WithLabelValues(action).Inc()
}

// RecordRouterCall records the completion of a single LLM router call.
func (m *Metrics) RecordRouterCall(op, tier, outcome string, d time.Duration) {
	if m == nil {
		return
	}
	if op == "" {
		op = "unknown"
	}
	if tier == "" {
		tier = "primary"
	}
	if outcome == "" {
		outcome = OutcomeFailure
	}
	m.RouterCallsTotal.WithLabelValues(op, tier, outcome).Inc()
	m.RouterCallDuration.WithLabelValues(op).Observe(d.Seconds())
}

// RecordRouterTokens records token consumption for a router call.
func (m *Metrics) RecordRouterTokens(tier string, prompt, completion, total int64) {
	if m == nil {
		return
	}
	if tier == "" {
		tier = "primary"
	}
	if prompt > 0 {
		m.RouterTokensTotal.WithLabelValues(tier, TokenKindPrompt).Add(float64(prompt))
	}
	if completion > 0 {
		m.RouterTokensTotal.WithLabelValues(tier, TokenKindCompletion).Add(float64(completion))
	}
	if total > 0 {
		m.RouterTokensTotal.WithLabelValues(tier, TokenKindTotal).Add(float64(total))
	}
}

// RecordRouterEscalation records that an op fell back from the primary
// tier to the escalation tier.
func (m *Metrics) RecordRouterEscalation(op string) {
	if m == nil {
		return
	}
	if op == "" {
		op = "unknown"
	}
	m.RouterEscalations.WithLabelValues(op).Inc()
}

// RecordTaskTransition records a task status change and updates the
// per-status gauge. The empty from status is used for newly created
// tasks.
func (m *Metrics) RecordTaskTransition(from, to string, kind string) {
	if m == nil {
		return
	}
	if kind == "" {
		kind = "unknown"
	}
	m.TaskTransitions.WithLabelValues(from, to, kind).Inc()
	if from != "" {
		m.TasksByStatus.WithLabelValues(from).Dec()
	}
	m.TasksByStatus.WithLabelValues(to).Inc()
}

// SetTargetUp sets the reachability gauge for a single target.
func (m *Metrics) SetTargetUp(target, kind string, up bool) {
	if m == nil {
		return
	}
	v := float64(0)
	if up {
		v = 1
	}
	m.TargetUp.WithLabelValues(target, kind).Set(v)
}

// SetTargetDiskFree sets a target's disk-free gauge (LOOM-86); a negative
// value — unknown — removes it.
func (m *Metrics) SetTargetDiskFree(target string, bytes int64) {
	if m == nil {
		return
	}
	if bytes < 0 {
		m.TargetDiskFree.DeleteLabelValues(target)
		return
	}
	m.TargetDiskFree.WithLabelValues(target).Set(float64(bytes))
}

// RecordTargetOp records the duration and (if any) error reason for a
// target operation. reason is a caller-chosen stable label such as
// "unreachable"; if empty and err is non-nil, a generic reason is used.
func (m *Metrics) RecordTargetOp(kind, op string, d time.Duration, reason string, err error) {
	if m == nil {
		return
	}
	if kind == "" {
		kind = "unknown"
	}
	if op == "" {
		op = "unknown"
	}
	m.TargetOpDuration.WithLabelValues(kind, op).Observe(d.Seconds())
	if err != nil {
		if reason == "" {
			reason = TargetOpErrorReason(err)
		}
		m.TargetOpErrors.WithLabelValues(kind, op, reason).Inc()
	}
}

// RecordReaperTask records that the idle reaper tore down one session.
func (m *Metrics) RecordReaperTask() {
	if m == nil {
		return
	}
	m.ReaperTasksReaped.Inc()
}

// TargetOpErrorReason maps common target errors to a stable reason label.
// It only uses the standard library so the metrics package stays free of
// domain import cycles.
func TargetOpErrorReason(err error) string {
	if err == nil {
		return "none"
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "context"
	}
	return "other"
}
