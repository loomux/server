package router

import "time"

// Test-only exports: what an agent CLI probe prints, so router_test's
// fake executor can answer one without hard-coding the wire format.
const (
	AgentProbePathPrefix    = agentProbePathPrefix
	AgentProbeVersionPrefix = agentProbeVersionPrefix
	AgentProbeAbsent        = agentProbeAbsent
	AgentProbeAuthBegin     = agentProbeAuthBegin
	AgentProbeAuthEnd       = agentProbeAuthEnd
	HealthProbeTmuxPrefix   = healthProbeTmuxPrefix
	HealthProbeDiskPrefix   = healthProbeDiskPrefix
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

// LockConversation holds conversationID's turn lock, as a turn in flight
// does, until the returned func is called.
func LockConversation(r *Router, conversationID string) (unlock func()) {
	return r.conversations.lock(conversationID)
}
