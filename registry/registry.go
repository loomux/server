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
}
