package router

import "context"

// DecisionAction identifies what a RoutingModel decided to do with an
// incoming message.
type DecisionAction string

const (
	// ActionAnswerDirectly means no dispatch is needed at all.
	ActionAnswerDirectly DecisionAction = "answer_directly"
	// ActionUseWorkspace dispatches into an existing workspace.
	ActionUseWorkspace DecisionAction = "use_workspace"
	// ActionProvisionWorkspace creates a new workspace before dispatching.
	ActionProvisionWorkspace DecisionAction = "provision_workspace"
)

// WorkspaceSnapshot is the compact workspace projection design spec §6
// says routing calls should see — tags/description/capabilities, not
// full history — keeping a routing call's context small and cheap
// regardless of how many workspaces exist.
type WorkspaceSnapshot struct {
	ID           string
	Name         string
	Description  string
	Tags         []string
	Capabilities []string
}

// ProvisionSpec describes a new dynamic workspace to create and set up
// (design spec §2: workspaces are "provisioned on demand... via a
// shell-kind pane that runs the provisioning script").
type ProvisionSpec struct {
	Name        string
	Path        string
	TargetID    string
	GitRemote   string
	Description string
	Tags        []string
	// ProvisionCommand is run via a shell-kind orchestrator.Launch to
	// set the workspace up (e.g. cloning a repo).
	ProvisionCommand string
}

// Decision is what a RoutingModel returns for an incoming message.
type Decision struct {
	Action DecisionAction

	// DirectAnswer is set when Action == ActionAnswerDirectly.
	DirectAnswer string

	// WorkspaceID is set when Action == ActionUseWorkspace.
	WorkspaceID string

	// NewWorkspace is set when Action == ActionProvisionWorkspace.
	NewWorkspace ProvisionSpec

	// AgentType is set when Action == ActionUseWorkspace or
	// ActionProvisionWorkspace — which registered agent-type to
	// dispatch to.
	AgentType string
}

// RelayResult is what RoutingModel.Relay returns.
type RelayResult struct {
	// Reply is the condensed text — used as both the chat-appropriate
	// reply and the workspace's new rolling summary, regardless of
	// Done.
	Reply string

	// Done distinguishes "the turn is finished" (always true, since
	// Relay only runs after a turn's completion signal fires) from "the
	// task itself is finished" (design spec §3, step 3: "if the task
	// itself (not just the turn) is finished, the pane is torn down").
	// true: the task is fully done — Router.Dispatch tears the pane
	// down via orchestrator.Complete, and the workspace reverts to
	// idle. false: the conversation is expected to continue — the task
	// stays open (registry.TaskStatusAwaitingInput) so the next message
	// in the same conversation is sent into the same pane via
	// orchestrator.SendMessage (spec §3, step 2: "If one's already
	// running, the message is sent into it as the next turn") rather
	// than launching a fresh one.
	Done bool
}

// RoutingModel is the swappable "router model" seam (design spec §6) —
// a real LLM call in production. router/llmrouter is the real,
// LLM-backed implementation; router/routertest provides a deterministic
// test stand-in for tests that don't want a real model call in the loop.
type RoutingModel interface {
	// Decide returns a routing decision for an incoming chat message,
	// given a compact snapshot of the existing workspace registry.
	Decide(ctx context.Context, message string, workspaces []WorkspaceSnapshot) (Decision, error)

	// Relay condenses captured agent output into a RelayResult — see
	// its doc comment for the Done distinction (design spec §3).
	Relay(ctx context.Context, capturedOutput string) (RelayResult, error)
}
