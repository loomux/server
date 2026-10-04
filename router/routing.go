package router

import "time"

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
	// ActionRunCommand runs a plain shell command on a target with no AI
	// agent involved, relaying its output verbatim (LOOM-72). It runs at
	// once only if Command is exactly what the user wrote in backticks;
	// otherwise the user is asked to confirm the exact command first —
	// see Router's runCommand.
	ActionRunCommand DecisionAction = "run_command"
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
	// Status, TargetName, Summary and LastUsed say what state the
	// workspace is in, where it lives and what was last done there
	// (LOOM-88), so the routing model can tell a live workspace from a
	// stale one. TargetName is the target's name, never its host.
	// Summary is the rolling summary, cut to SnapshotSummaryRunes.
	Status     string
	TargetName string
	Summary    string
	LastUsed   *time.Time
}

// SnapshotSummaryRunes bounds a workspace's summary in the routing
// prompt; a longer one is cut and ends in "…".
const SnapshotSummaryRunes = 240

// TargetSnapshot is the compact projection of a registered execution
// target (design spec §2) a routing call sees, so a provision_workspace
// decision can name a real target_id instead of guessing one (LOOM-64).
// Deliberately id, name and kind only: the snapshot is sent to a
// third-party router-model vendor, so Host (an internal hostname), User
// and SSHKeyRef never go into it — none of them is needed to choose a
// target.
type TargetSnapshot struct {
	ID   string
	Name string
	Kind string
	// Agents is each probed agent-type's recorded availability on this
	// target (LOOM-71): true if its CLI was found, false if not. An
	// agent-type never probed here is absent from the map — unknown, not
	// unavailable.
	Agents map[string]bool
	// AgentVersions is the version line each available agent's CLI
	// printed when probed (LOOM-79), keyed like Agents; absent when
	// unknown.
	AgentVersions map[string]string
}

// ProvisionKind is how a new dynamic workspace's directory comes to be
// (LOOM-90).
type ProvisionKind string

const (
	// ProvisionEmpty creates an empty directory.
	ProvisionEmpty ProvisionKind = "empty"
	// ProvisionGitClone clones GitRemote into a new directory.
	ProvisionGitClone ProvisionKind = "git_clone"
	// ProvisionExistingDir adopts a directory that already exists under
	// the workspace root.
	ProvisionExistingDir ProvisionKind = "existing_dir"
)

// ProvisionSpec describes a new dynamic workspace to create (design spec
// §2). It is structured data only (LOOM-90): Loomux builds the commands
// that set the workspace up itself, from these validated fields (see
// provisioningRecipe) — no command or path from the routing model is ever
// run or used. The workspace's directory is <target's workspace
// root>/<Name>.
type ProvisionSpec struct {
	// Name is the workspace's name and its directory under the workspace
	// root: a slug, [a-z0-9][a-z0-9-]*, at most 63 characters.
	Name        string
	TargetID    string
	Kind        ProvisionKind
	GitRemote   string // required for ProvisionGitClone, and only then
	Description string
	Tags        []string
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

	// TargetID and Command are set when Action == ActionRunCommand: the
	// registered target to run on, and the command — copied from the
	// user's message, never composed by the model if it can help it.
	TargetID string
	Command  string

	// LeaveOpenTask is the model saying this message is unrelated to the
	// conversation's open task (LOOM-87). Without it, a decision that
	// doesn't continue the open task is overridden to do so.
	LeaveOpenTask bool
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

// DispatchOptions holds the optional, per-call knobs Dispatch/Decide
// accept beyond their required positional arguments (LOOM-46) — a
// struct-of-options rather than growing Dispatch's/Decide's own
// parameter lists, so a caller that doesn't need any of them (nearly
// every existing call site, including every test in this repo) is
// completely unaffected: DispatchOption is applied through a variadic
// tail, never a required argument.
type DispatchOptions struct {
	// WorkspaceHint is a client-supplied workspace ID the caller
	// believes this message likely belongs to (e.g. a chat UI already
	// focused on that workspace's conversation). It is advisory only —
	// the router model retains final authority over workspace selection
	// (design spec §6), the same as it already does for every workspace
	// named in the snapshot it's given. An empty string (the default)
	// means no hint was supplied.
	WorkspaceHint string
	// DispatchID is the dispatch job this call runs for (LOOM-80); the
	// messages the router writes are stamped with it. Empty outside one.
	DispatchID string
	// UserMessageLogged says the user message is already stored (a
	// dispatch job writes it at submit), so only the reply is logged.
	UserMessageLogged bool

	// History, OpenTask and LastWorkspaceID/Name are the conversation's
	// context (LOOM-87), filled in by the router itself — not callers —
	// before Decide. History is the turns before this message, oldest
	// first, bounded (see HistoryMessages). OpenTask is the conversation's
	// agent task awaiting input, if any. LastWorkspaceID is the workspace
	// of its most recent task when none is open.
	History           []ConversationTurn
	OpenTask          *OpenTaskSnapshot
	LastWorkspaceID   string
	LastWorkspaceName string
}

// ConversationTurn is one earlier message in a conversation.
type ConversationTurn struct {
	Role    string // "user" or "assistant"
	Content string
}

// OpenTaskSnapshot is a conversation's task still waiting on the user:
// its agent's last turn ended without finishing (relay done=false) — a
// question, a confirmation, or a step of a longer job.
type OpenTaskSnapshot struct {
	TaskID        string
	WorkspaceID   string
	WorkspaceName string
	AgentType     string
	Status        string
	// LastReply is what the agent last said, as relayed, keeping its end.
	LastReply string
}

// Bounds on the conversation context given to Decide (LOOM-87), in
// runes: at most HistoryMessages turns, each cut to HistoryMessageRunes,
// HistoryRunes in all; an open task's last reply cut to
// OpenTaskReplyRunes.
const (
	HistoryMessages     = 6
	HistoryMessageRunes = 400
	HistoryRunes        = 2400
	OpenTaskReplyRunes  = 600
)

// withConversation sets the conversation context on a Decide call.
func withConversation(history []ConversationTurn, open *OpenTaskSnapshot, lastWorkspaceID, lastWorkspaceName string) DispatchOption {
	return func(o *DispatchOptions) {
		o.History = history
		o.OpenTask = open
		o.LastWorkspaceID = lastWorkspaceID
		o.LastWorkspaceName = lastWorkspaceName
	}
}

// DispatchOption configures a DispatchOptions via With* constructors
// below — the functional-options pattern already used elsewhere in this
// codebase (see api.Option).
type DispatchOption func(*DispatchOptions)

// WithWorkspaceHint sets DispatchOptions.WorkspaceHint.
func WithWorkspaceHint(workspaceID string) DispatchOption {
	return func(o *DispatchOptions) { o.WorkspaceHint = workspaceID }
}

// WithDispatchID sets DispatchOptions.DispatchID.
func WithDispatchID(id string) DispatchOption {
	return func(o *DispatchOptions) { o.DispatchID = id }
}

// WithUserMessageLogged sets DispatchOptions.UserMessageLogged.
func WithUserMessageLogged() DispatchOption {
	return func(o *DispatchOptions) { o.UserMessageLogged = true }
}

// RoutingModel is the swappable "router model" seam (design spec §6) —
// a real LLM call in production. router/llmrouter is the real,
// LLM-backed implementation; router/routertest provides a deterministic
// test stand-in for tests that don't want a real model call in the loop.
type RoutingModel interface {
	// Decide returns a routing decision for an incoming chat message,
	// given a compact snapshot of the existing workspace registry and of
	// the registered execution targets a new workspace may be
	// provisioned on (LOOM-64).
	// opts carries optional advisory input such as WithWorkspaceHint
	// (LOOM-46) — an implementation is free to ignore any option it
	// doesn't understand.
	Decide(ctx context.Context, message string, workspaces []WorkspaceSnapshot, targets []TargetSnapshot, opts ...DispatchOption) (Decision, error)

	// Relay condenses captured agent output into a RelayResult — see
	// its doc comment for the Done distinction (design spec §3).
	Relay(ctx context.Context, capturedOutput string) (RelayResult, error)
}
