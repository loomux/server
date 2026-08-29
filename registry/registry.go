// Package registry defines the workspace registry's domain types and the
// Store interface that backend implementations satisfy. See design spec
// §2, §8.
package registry

import "time"

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
}
