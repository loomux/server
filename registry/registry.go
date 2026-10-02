// Package registry defines the workspace registry's domain types and the
// Store interface that backend implementations satisfy. See design spec
// §2, §8.
package registry

import (
	"errors"
	"fmt"
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
	CreatedAt time.Time
	UpdatedAt time.Time
}

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
//   - a local must carry neither. Clearing them silently would hide a
//     caller's misunderstanding until an attach-info response came back
//     missing the fields they thought they had set.
//
// Every non-nil error is a validation failure whose text is safe to show
// to the caller as-is.
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
	default:
		return fmt.Errorf("kind must be %q or %q", TargetKindLocal, TargetKindRemote)
	}
	return nil
}

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
	CreatedAt      time.Time
	UpdatedAt      time.Time
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
	TaskStatusCompleted     TaskStatus = "completed"
	TaskStatusFailed        TaskStatus = "failed"
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
	CheckedAt time.Time
}
