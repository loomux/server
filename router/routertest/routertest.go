// Package routertest provides a deterministic RoutingModel stand-in for
// tests. The real "router model" (design spec §6) is a swappable LLM
// call in production; no such integration exists here, or anywhere in
// this repo yet — that's separate, later work.
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
