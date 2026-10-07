// Package registry defines the workspace registry's domain types and the
// Store interface that backend implementations satisfy. See design spec
// §2, §8.
package registry

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"
)

// TargetKind identifies where a target's tmux sessions run.
type TargetKind string

const (
	TargetKindLocal  TargetKind = "local"
	TargetKindRemote TargetKind = "remote"
)

// Target is a host Loomux can run tmux sessions on.
type Target struct {
	ID        string
	Name      string
	Kind      TargetKind
	Host      string // empty for local
	User      string // empty for local
	SSHKeyRef string // reference/identifier only; actual secret material lives in the credential vault (§7)
	// WorkspaceRoot is the directory every dynamic workspace on this
	// target is provisioned under (LOOM-90): an absolute, clean path.
	// Empty means $HOME/loomux-workspaces on the target. Provisioning
	// refuses any workspace whose resolved directory — symlinks followed —
	// isn't inside it.
	WorkspaceRoot string
	// PermissionMode is how much the agents launched on this target may
	// do without asking (one of the PermissionMode* values); empty means
	// each agent-type's own default. Per-target, so policy (LOOM-89) can
	// tighten it on, say, a work-only host.
	PermissionMode string
	// Policy is what Loomux may do on this target (LOOM-89). The zero
	// value allows everything, as before policies existed.
	Policy TargetPolicy
	// SSHPort, when non-zero, overrides the port the SSH config gives
	// for this target (LOOM-114: per-target options override the
	// mounted config, never replace it).
	SSHPort int
	// HostKeys are the known_hosts lines pinned for this target through
	// the API (LOOM-114), or empty. A pinned target is checked against
	// these alone; others against the mounted known_hosts. Written only
	// through SetTargetHostKeys, never by UpdateTarget.
	HostKeys  string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// TargetPolicy is what Loomux may do on a target (LOOM-89), enforced by
// the router after the routing decision, before anything runs — never
// left to the routing model. Its zero value allows everything.
type TargetPolicy struct {
	// Purpose is TargetPurposePersonal or TargetPurposeWork; empty is
	// personal. A work machine runs agents under the user's work logins.
	Purpose string
	// AllowedAgentTypes, if non-empty, are the only agent types that may
	// run there.
	AllowedAgentTypes []string
	// NoProvision forbids creating workspaces there; NoShell forbids
	// running plain shell commands.
	NoProvision bool
	NoShell     bool
	// RequireConfirmation makes new work there — a new workspace, a
	// command, an agent started in a workspace — wait for the user's
	// "yes" in chat to the plan.
	RequireConfirmation bool
	// Relay is how much of this target's work Loomux's router models (a
	// third-party API) may see: RelayFull, RelayLastMessage or RelayNone.
	// Empty means the purpose's default (EffectiveRelay).
	Relay string
}

// What the router models may see of a target's work (TargetPolicy.Relay,
// user decision 2026-10-07).
const (
	// RelayFull: the turn's output and context go to the relay model,
	// and its conversations' history to the routing model.
	RelayFull = "full"
	// RelayLastMessage: only the agent's own final message for a turn
	// (its visible screen, for an agent with no final-message hook); the
	// routing model sees only those replies of its conversations.
	RelayLastMessage = "last_message"
	// RelayNone: nothing from the target reaches either model. The
	// agent's final message is the chat reply as it is, and the routing
	// model sees no message, reply or summary from its conversations.
	RelayNone = "none"
)

// EffectiveRelay is p.Relay, or the default for p's purpose: none for a
// work machine, full otherwise.
func (p TargetPolicy) EffectiveRelay() string {
	switch {
	case p.Relay != "":
		return p.Relay
	case p.Purpose == TargetPurposeWork:
		return RelayNone
	default:
		return RelayFull
	}
}

// AllowsAgent reports whether agentType may run under p.
func (p TargetPolicy) AllowsAgent(agentType string) bool {
	if len(p.AllowedAgentTypes) == 0 {
		return true
	}
	for _, a := range p.AllowedAgentTypes {
		if a == agentType {
			return true
		}
	}
	return false
}

// Target purposes (TargetPolicy.Purpose).
const (
	TargetPurposePersonal = "personal"
	TargetPurposeWork     = "work"
)

// Validate enforces the invariants the execution layer assumes but
// cannot itself check at registration time, so a target that could never
// be dispatched to is rejected at whichever entry point registers it
// (the HTTP API today; an admin CLI would share this — LOOM-65) rather
// than stored and discovered broken later:
//
//   - Name must be non-blank.
//   - Kind must be one of the two targets.NewExecutor knows; anything
//     else yields "unknown target kind" at dispatch.
//   - a remote needs both Host and User, because
//     RemoteExecutor.destination() builds user+"@"+host and ssh rejects
//     a bare "@host".
//   - WorkspaceRoot, if set, must be an absolute, clean path other than
//     "/" — it bounds where an agent can be given write access.
//   - a local must carry neither. Clearing them silently would hide a
//     caller's misunderstanding until an attach-info response came back
//     missing the fields they thought they had set.
//
// Every non-nil error is a validation failure whose text is safe to show
// to the caller as-is.
// validSSHUser is a POSIX-style login name, as useradd takes it.
var validSSHUser = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,31}$`)

func (t *Target) Validate() error {
	if strings.TrimSpace(t.Name) == "" {
		return errors.New("name is required")
	}
	switch t.Kind {
	case TargetKindLocal:
		if t.Host != "" || t.User != "" {
			return errors.New("host and user must be empty for a local target")
		}
	case TargetKindRemote:
		if t.Host == "" || t.User == "" {
			return errors.New("host and user are required for a remote target")
		}
		// Both end up in ssh's argv: nothing ssh could read as an option
		// (a leading "-", as in -oProxyCommand=…), and nothing that splits
		// or re-targets user@host.
		if !validSSHUser.MatchString(t.User) {
			return errors.New("user must be a login name: a letter or _, then letters, digits, _ . or -, at most 32")
		}
		if strings.HasPrefix(t.Host, "-") || strings.ContainsAny(t.Host, "@/\\ \t\r\n\x00") {
			return errors.New("host must be a host name or address (no leading -, whitespace, @ or /)")
		}
	default:
		return fmt.Errorf("kind must be %q or %q", TargetKindLocal, TargetKindRemote)
	}
	if t.SSHPort < 0 || t.SSHPort > 65535 {
		return errors.New("ssh_port must be 0 (the SSH config's) or a port number")
	}
	if t.Kind == TargetKindLocal && t.SSHPort != 0 {
		return errors.New("ssh_port must be 0 for a local target")
	}
	switch t.PermissionMode {
	case "", PermissionModeAuto, PermissionModeAcceptEdits, PermissionModeManual:
	default:
		return fmt.Errorf("permission_mode must be empty, %q, %q or %q", PermissionModeAuto, PermissionModeAcceptEdits, PermissionModeManual)
	}
	switch t.Policy.Relay {
	case "", RelayFull, RelayLastMessage, RelayNone:
	default:
		return fmt.Errorf("relay must be empty (the purpose's default), %q, %q or %q", RelayFull, RelayLastMessage, RelayNone)
	}
	switch t.Policy.Purpose {
	case "", TargetPurposePersonal, TargetPurposeWork:
	default:
		return fmt.Errorf("purpose must be empty, %q or %q", TargetPurposePersonal, TargetPurposeWork)
	}
	for _, a := range t.Policy.AllowedAgentTypes {
		if strings.TrimSpace(a) == "" || a != strings.TrimSpace(a) {
			return errors.New("allowed_agent_types must not contain blank or padded names")
		}
	}
	if t.WorkspaceRoot != "" {
		switch {
		case !path.IsAbs(t.WorkspaceRoot):
			return errors.New("workspace_root must be an absolute path")
		case path.Clean(t.WorkspaceRoot) != t.WorkspaceRoot:
			return errors.New("workspace_root must be a clean path (no ., .. or trailing /)")
		case t.WorkspaceRoot == "/":
			return errors.New("workspace_root must not be /")
		}
	}
	return nil
}

// Target permission modes: how much an agent may do without a human
// approving it. Each agent-type maps them to its own CLI flags.
const (
	// PermissionModeAuto: the agent's own classifier-gated automatic
	// mode (claude --permission-mode auto) — never bypass.
	PermissionModeAuto = "auto"
	// PermissionModeAcceptEdits: file edits go ahead; commands ask.
	PermissionModeAcceptEdits = "accept-edits"
	// PermissionModeManual: everything asks.
	PermissionModeManual = "manual"
)

// WorkspaceStatus tracks a workspace's current lifecycle state.
type WorkspaceStatus string

const (
	WorkspaceStatusIdle         WorkspaceStatus = "idle"
	WorkspaceStatusActive       WorkspaceStatus = "active"
	WorkspaceStatusProvisioning WorkspaceStatus = "provisioning"
	WorkspaceStatusArchived     WorkspaceStatus = "archived"
	// WorkspaceStatusFailed marks a workspace whose provisioning failed
	// (LOOM-71). The row is kept for inspection rather than deleted, but
	// it is never offered to the routing model again — dispatching into a
	// workspace whose setup never finished would only fail later and
	// less legibly.
	WorkspaceStatusFailed WorkspaceStatus = "failed"
)

// Workspace is a directory (with an optional git remote) bound to exactly
// one target.
type Workspace struct {
	ID          string
	Name        string
	Path        string
	TargetID    string
	GitRemote   string // empty if none
	Tags        []string
	Description string
	// Capabilities lists the MCPs/tools available in this workspace.
	Capabilities []string
	Status       WorkspaceStatus
	IsDynamic    bool
	LastUsedAt   *time.Time
	// RollingSummary is replaced (never appended to) on each update — see
	// SetWorkspaceRollingSummary.
	RollingSummary string
	// StatusReason says why the workspace is in its Status when that
	// isn't self-explanatory — e.g. what made it failed (LOOM-77). Empty
	// otherwise.
	StatusReason string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// TaskKind identifies what a tmux pane is running.
type TaskKind string

const (
	TaskKindAgent TaskKind = "agent"
	TaskKindShell TaskKind = "shell"
	// TaskKindCommand is a one-shot command run as the pane's own process
	// — an agent CLI install (LOOM-71), a direct shell command (LOOM-72).
	// Its completion is the process exiting (completion.TierExit), never
	// an idle heuristic, and the exit code is recorded on the task.
	TaskKindCommand TaskKind = "command"
)

// TaskStatus tracks a task's current lifecycle state.
type TaskStatus string

const (
	TaskStatusRunning       TaskStatus = "running"
	TaskStatusAwaitingInput TaskStatus = "awaiting-input"
	TaskStatusHumanTakeover TaskStatus = "human-takeover"
	// TaskStatusNeedsAttention: the agent is stopped at a prompt only a
	// human can answer — an approval its permission mode still asks for,
	// a question, a trust dialog (LOOM-97). Task.Attention says what it
	// shows; the conversation's next message answers it.
	TaskStatusNeedsAttention TaskStatus = "needs-attention"
	TaskStatusCompleted      TaskStatus = "completed"
	TaskStatusFailed         TaskStatus = "failed"
)

// Task is a single tmux pane's lifecycle record, scoped to a workspace.
type Task struct {
	ID          string
	WorkspaceID string
	Kind        TaskKind
	AgentType   string // empty for shell panes
	TmuxSession string
	Status      TaskStatus
	// ConversationID identifies the chat conversation this task belongs to.
	ConversationID string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	StartedAt      *time.Time
	CompletedAt    *time.Time
	// ReapedAt is set when the idle reaper (design spec's continuation
	// model, LOOM-16) tears down this task's session for inactivity.
	// Purely informational — it does not change Status or otherwise gate
	// resumability: a follow-up message still finds this task via its
	// (unchanged) Status, discovers the session is gone, and the router
	// falls back to a fresh task (a new row, ReapedAt naturally nil)
	// rather than clearing this one's marker in place.
	ReapedAt *time.Time
	// Command is what a TaskKindCommand task ran, verbatim. Empty for
	// every other kind: an agent's launch command carries injected
	// credentials (credentials.ShellEnvPrefix) and is never stored.
	Command string
	// ExitCode is a TaskKindCommand task's exit status once its process
	// has exited; nil while it is still running or for any other kind.
	ExitCode *int
	// FailureReason, ErrorClass and OutputTail say why a
	// TaskStatusFailed task failed (LOOM-77): a human-readable reason, a
	// stable machine-readable class, and the last of the pane's output
	// when there was any (bounded, credentials redacted). Empty for a task
	// that hasn't failed.
	FailureReason string
	ErrorClass    ErrorClass
	OutputTail    string
	// Attention is the prompt a TaskStatusNeedsAttention task is stopped
	// at (LOOM-97); nil otherwise.
	Attention *Attention
}

// AttentionKind says what an agent's prompt asks a human for (LOOM-97).
type AttentionKind string

const (
	// AttentionPermission: approve or deny something the agent wants to
	// do (run a command, edit a file).
	AttentionPermission AttentionKind = "permission"
	// AttentionQuestion: the agent asks the user to pick an answer or
	// type one.
	AttentionQuestion AttentionKind = "question"
	// AttentionTrust: the agent asks whether to trust the workspace's
	// folder.
	AttentionTrust AttentionKind = "trust"
	// AttentionLogin: the agent CLI isn't signed in. Loomux never answers
	// this one: the task fails with ErrorClassLoginRequired.
	AttentionLogin AttentionKind = "login"
	// AttentionUsageLimit: the agent's account hit its usage limit
	// (LOOM-109). Nothing to answer: the task fails with
	// ErrorClassAgentRateLimited, saying when the limit resets.
	AttentionUsageLimit AttentionKind = "usage_limit"
)

// Attention is a prompt read off an agent's pane (LOOM-97): what a chat
// client shows so the user can answer it without attaching.
type Attention struct {
	Kind AttentionKind `json:"kind"`
	// Title is the prompt's heading ("Bash command"), Detail what it is
	// about (the command, the file), Question what it asks ("Do you want
	// to proceed?"). Any may be empty.
	Title    string `json:"title,omitempty"`
	Detail   string `json:"detail,omitempty"`
	Question string `json:"question,omitempty"`
	// Options are the choices the prompt lists, in order.
	Options []AttentionOption `json:"options,omitempty"`
	// Selected is the index of the option the prompt's cursor is on.
	Selected int `json:"selected"`
	// ResetsAt is, for AttentionUsageLimit, when the limit resets as the
	// agent put it ("5pm (Europe/Berlin)", "in 2 hours"); may be empty.
	ResetsAt string `json:"resets_at,omitempty"`
}

// AttentionOption is one choice an Attention offers.
type AttentionOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// ErrorClass is a stable, machine-readable category for a task failure
// (LOOM-77) — what a client switches on to render a failure, where
// FailureReason is the text it shows.
type ErrorClass string

const (
	// ErrorClassLaunchFailed: the task's tmux session could not be
	// started.
	ErrorClassLaunchFailed ErrorClass = "launch_failed"
	// ErrorClassTargetUnreachable: the target couldn't be reached.
	ErrorClassTargetUnreachable ErrorClass = "target_unreachable"
	// ErrorClassSendFailed: the turn's message could not be delivered.
	ErrorClassSendFailed ErrorClass = "send_failed"
	// ErrorClassAgentExited: the agent CLI's process exited mid-turn.
	ErrorClassAgentExited ErrorClass = "agent_exited"
	// ErrorClassProvisionFailed: a provisioning script exited non-zero.
	ErrorClassProvisionFailed ErrorClass = "provision_failed"
	// ErrorClassWaitFailed: waiting for the turn to complete failed or
	// was abandoned (e.g. the request was cancelled).
	ErrorClassWaitFailed ErrorClass = "wait_failed"
	// ErrorClassRelayFailed: the finished turn's output could not be
	// captured or relayed.
	ErrorClassRelayFailed ErrorClass = "relay_failed"
	// ErrorClassTimeout: the turn hit one of its bounds — its maximum
	// duration, or no progress while waiting (LOOM-76). The pane is left
	// running.
	ErrorClassTimeout ErrorClass = "timeout"
	// ErrorClassSessionLost: the task's session was gone (reaped,
	// crashed, killed) when the next message arrived.
	ErrorClassSessionLost ErrorClass = "session_lost"
	// ErrorClassLoginRequired: the agent CLI asked to be signed in
	// (LOOM-97). Its pane is left for a human to complete the login.
	ErrorClassLoginRequired ErrorClass = "login_required"
	// ErrorClassAgentRateLimited: the agent's account hit its usage
	// limit (LOOM-109); its reason says when it resets.
	ErrorClassAgentRateLimited ErrorClass = "agent_rate_limited"
	// ErrorClassCompactionLoop: the agent kept compacting its context
	// within one turn without finishing it (LOOM-109); Loomux
	// interrupted it.
	ErrorClassCompactionLoop ErrorClass = "compaction_loop"
	// ErrorClassTargetUnhealthy: the target failed its health probe for a
	// reason other than being unreachable — no tmux, its disk nearly full
	// (LOOM-86).
	ErrorClassTargetUnhealthy ErrorClass = "target_unhealthy"
	// ErrorClassMessageTooLarge: the message, with the context in front
	// of it, is over what Loomux pastes into an agent's pane
	// (targets.MaxPasteBytes, LOOM-111). Nothing was sent; the agent's
	// task is left as it was.
	ErrorClassMessageTooLarge ErrorClass = "message_too_large"
	// ErrorClassCancelled: the user cancelled the turn (LOOM-99).
	ErrorClassCancelled ErrorClass = "cancelled"
	// ErrorClassInternal: anything else.
	ErrorClassInternal ErrorClass = "internal"
)

// TaskTurn is what one turn of a task produced (LOOM-91), kept after the
// task ends so a person can see what actually happened, not only the
// relay's summary of it. Credential values are redacted before storing.
type TaskTurn struct {
	ID     string
	TaskID string
	// UserMessage is the message the turn sent the agent.
	UserMessage string
	// AgentMessage is the agent's own final message for the turn, from
	// its completion hook; empty when it saved none.
	AgentMessage string
	// Pane is the pane's scrollback when the turn ended, bounded.
	Pane      string
	CreatedAt time.Time
}

// TaskFailure is what a failing task records (see Task.FailureReason).
type TaskFailure struct {
	Class      ErrorClass
	Reason     string
	OutputTail string
}

// TargetAgent records whether an agent-type's CLI was found on a target
// the last time Loomux probed for it (LOOM-71) — a `command -v` of the
// CLI through the target's executor. Rows exist only for agent-types that
// have been probed; no row means "unknown", not "absent". Refreshed on
// every pre-launch probe and on demand.
type TargetAgent struct {
	TargetID  string
	AgentType string
	Available bool
	// Path is the absolute path the CLI resolved to on the target, and
	// Version the first line its --version printed (LOOM-79). Launches
	// use Path, so the version recorded is the version that runs. Empty
	// when unavailable.
	Path    string
	Version string
	// AuthStatus is whether the CLI reported itself signed in when probed
	// (LOOM-86): one of the AgentAuth* values, empty when not checked.
	AuthStatus string
	CheckedAt  time.Time
}

// TargetAgent.AuthStatus values (LOOM-86).
const (
	AgentAuthLoggedIn  = "logged_in"
	AgentAuthLoggedOut = "logged_out"
	// AgentAuthUnknown: the CLI was asked but its answer wasn't
	// recognised.
	AgentAuthUnknown = "unknown"
)

// TargetHealth is a target's latest health probe (LOOM-86), refreshed
// periodically and on demand. No record means never probed.
type TargetHealth struct {
	TargetID string
	// Reachable is whether the probe could run a command on the target
	// at all; Error says why not, or what else is wrong with it.
	Reachable bool
	Error     string
	// Latency is how long the probe command took, end to end.
	Latency     time.Duration
	TmuxVersion string
	// DiskFreeBytes is the space free on the filesystem holding the
	// target's workspace root; -1 when unknown.
	DiskFreeBytes int64
	ProbedAt      time.Time
}
