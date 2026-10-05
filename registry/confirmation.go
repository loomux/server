package registry

import "time"

// ConfirmationStatus is where an offer awaiting the user's yes stands
// (LOOM-123).
type ConfirmationStatus string

const (
	ConfirmationPending  ConfirmationStatus = "pending"
	ConfirmationApproved ConfirmationStatus = "approved"
	// ConfirmationDenied: the user said no, or sent anything else, which
	// cancels an offer.
	ConfirmationDenied ConfirmationStatus = "denied"
	// ConfirmationExpired: it timed out, or Loomux restarted, which forgets
	// every offer (fails closed).
	ConfirmationExpired ConfirmationStatus = "expired"
)

// Confirmation kinds: what an offer will do once approved.
const (
	ConfirmationRunCommand   = "run_command"
	ConfirmationInstallAgent = "install_agent"
	ConfirmationCloneRemote  = "clone_remote"
	ConfirmationPolicy       = "policy"
)

// Confirmation is the record of an offer the router made and is waiting
// on a yes for (LOOM-123): what it would do and where, for the web UI to
// show as a card with Approve and Deny, and how it was answered. What a
// yes actually runs lives with the router, in memory; this row only
// describes it.
type Confirmation struct {
	ID             string
	ConversationID string
	// DispatchID is the turn that made the offer; empty outside a
	// dispatch job.
	DispatchID string
	Kind       string
	TargetID   string
	TargetName string
	AgentType  string
	// Command is the shell command or install command that would run.
	Command string
	// Workspace and GitRemote describe a workspace the offer would create.
	Workspace string
	GitRemote string
	Status    ConfirmationStatus
	CreatedAt time.Time
	ExpiresAt time.Time
	// ResolvedAt is set once Status leaves pending.
	ResolvedAt *time.Time
}
