package router

// Test-only exports: what an agent CLI probe prints, so router_test's
// fake executor can answer one without hard-coding the wire format.
const (
	AgentProbePresent = agentProbePresent
	AgentProbeAbsent  = agentProbeAbsent
)
