// Package routertest provides a deterministic RoutingModel stand-in for
// tests. A real, LLM-backed RoutingModel (design spec §6) lives in
// router/llmrouter; this stub stays deliberately dumb and deterministic
// for tests that don't want a real model call in the loop.
package routertest

import (
	"context"

	"github.com/Loomux/server/router"
)

// StubRoutingModel is a RoutingModel driven explicitly by test code via
// its func fields — a plain call-and-return double, not a
// signaled/blocking construct (Decide and Relay aren't "wait until
// told" operations the way orchestrator/detectortest.ManualDetector's
// Wait is).
type StubRoutingModel struct {
	DecideFunc func(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error)
	RelayFunc  func(ctx context.Context, capturedOutput string) (router.RelayResult, error)

	// LastDecideOptions captures the resolved DispatchOptions from the
	// most recent Decide call (LOOM-46's WithWorkspaceHint, etc.), so
	// tests can assert an option reached the routing model without
	// DecideFunc itself needing to know about the variadic tail.
	LastDecideOptions router.DispatchOptions
	// LastDecideTargets captures the target snapshot passed to the most
	// recent Decide call, so tests can assert what the router model was
	// told about registered targets without widening DecideFunc.
	LastDecideTargets []router.TargetSnapshot
}

func (m *StubRoutingModel) Decide(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot, targets []router.TargetSnapshot, opts ...router.DispatchOption) (router.Decision, error) {
	var o router.DispatchOptions
	for _, opt := range opts {
		opt(&o)
	}
	m.LastDecideOptions = o
	m.LastDecideTargets = targets
	return m.DecideFunc(ctx, message, workspaces)
}

func (m *StubRoutingModel) Relay(ctx context.Context, capturedOutput string) (router.RelayResult, error) {
	return m.RelayFunc(ctx, capturedOutput)
}

var _ router.RoutingModel = (*StubRoutingModel)(nil)
