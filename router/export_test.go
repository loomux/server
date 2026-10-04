package router

import "time"

// Test-only exports: what an agent CLI probe prints, so router_test's
// fake executor can answer one without hard-coding the wire format.
const (
	AgentProbePathPrefix    = agentProbePathPrefix
	AgentProbeVersionPrefix = agentProbeVersionPrefix
	AgentProbeAbsent        = agentProbeAbsent
)

// ProvisioningMarker starts every provisioning recipe; tests use it to
// recognise one.
const ProvisioningMarker = provisioningMarker

// SetBootRetry shortens startup reconciliation's retry pacing for a test.
func SetBootRetry(every, upTo time.Duration) (restore func()) {
	oldEvery, oldUpTo := bootRetry, bootRetryFor
	bootRetry, bootRetryFor = every, upTo
	return func() { bootRetry, bootRetryFor = oldEvery, oldUpTo }
}
