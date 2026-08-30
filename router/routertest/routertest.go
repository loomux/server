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
	RelayFunc  func(ctx context.Context, capturedOutput string) (string, error)
}

func (m *StubRoutingModel) Decide(ctx context.Context, message string, workspaces []router.WorkspaceSnapshot) (router.Decision, error) {
	return m.DecideFunc(ctx, message, workspaces)
}

func (m *StubRoutingModel) Relay(ctx context.Context, capturedOutput string) (string, error) {
	return m.RelayFunc(ctx, capturedOutput)
}

var _ router.RoutingModel = (*StubRoutingModel)(nil)
