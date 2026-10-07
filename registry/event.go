package registry

import "time"

// Dispatch event kinds (LOOM-110): what a turn did, kept after the fact
// in place of pod logs, whose retention is short.
const (
	// EventDecision: the routing model's decision — action, model and
	// tier, and the workspace, target or agent it named.
	EventDecision = "decision"
	// EventCommand: a shell command ran on a target (run_command).
	EventCommand = "command"
	// EventProvision: a workspace's provisioning ran on a target.
	EventProvision = "provision"
	// EventOffer: the router offered something and waits for a yes;
	// EventOfferAnswered: how the offer was answered.
	EventOffer         = "offer"
	EventOfferAnswered = "offer_answered"
	// EventAgentTurn: an agent's turn in a workspace ended.
	EventAgentTurn = "agent_turn"
	// EventRelay: the relay model condensed a turn's output.
	EventRelay = "relay"
	// EventOutcome: the dispatch ended.
	EventOutcome = "outcome"
)

// DispatchEvent is one entry of a conversation's audit trail (LOOM-110):
// what was decided, run and where, and how it ended. Command text is
// stored with credential values and secret-shaped strings redacted. Rows
// are append-only and deleted after the retention period.
type DispatchEvent struct {
	ID             string
	ConversationID string
	// DispatchID is the turn it belongs to; empty outside a dispatch job.
	DispatchID string
	CreatedAt  time.Time
	Kind       string
	// Model and Tier name the router model that decided or relayed
	// ("primary", "escalation"), when one did.
	Model       string
	Tier        string
	TargetID    string
	WorkspaceID string
	TaskID      string
	// Command is the command that ran, redacted.
	Command string
	// Outcome is how it ended in a word or two: an action, "exit 0",
	// "succeeded", "approved".
	Outcome    string
	ErrorClass ErrorClass
	Duration   time.Duration
	// Detail is a short free-text note (an agent type, an override).
	Detail string
}
