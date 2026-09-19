package registry

import (
	"context"
	"time"
)

// Store is the backend-agnostic interface every storage implementation
// satisfies. It intentionally has no SQL- or driver-specific types in its
// signature so a backend can be swapped without changing callers.
type Store interface {
	CreateTarget(ctx context.Context, t *Target) error
	GetTarget(ctx context.Context, id string) (*Target, error)
	ListTargets(ctx context.Context) ([]*Target, error)
	UpdateTarget(ctx context.Context, t *Target) error
	DeleteTarget(ctx context.Context, id string) error

	CreateWorkspace(ctx context.Context, w *Workspace) error
	GetWorkspace(ctx context.Context, id string) (*Workspace, error)
	GetWorkspaceByName(ctx context.Context, name string) (*Workspace, error)
	ListWorkspaces(ctx context.Context) ([]*Workspace, error)
	UpdateWorkspace(ctx context.Context, w *Workspace) error
	// SetWorkspaceRollingSummary replaces (never appends to) a workspace's
	// rolling summary field.
	SetWorkspaceRollingSummary(ctx context.Context, id, summary string) error
	DeleteWorkspace(ctx context.Context, id string) error

	CreateTask(ctx context.Context, t *Task) error
	GetTask(ctx context.Context, id string) (*Task, error)
	ListTasksByWorkspace(ctx context.Context, workspaceID string) ([]*Task, error)
	// ListTasks returns every task across every workspace — a
	// conversation isn't pinned to one workspace (the router can route
	// the same conversation_id to a different workspace on a later
	// message), so listing/grouping by conversation needs the
	// unfiltered view rather than ListTasksByWorkspace.
	ListTasks(ctx context.Context) ([]*Task, error)
	UpdateTask(ctx context.Context, t *Task) error
	DeleteTask(ctx context.Context, id string) error

	// CreateMessage and ListMessagesByConversation store the per-turn
	// chat transcript (design spec docs/design/message-logging-design.md).
	// Messages are append-only — there is no update/delete here by
	// design (see registry.Message's doc comment).
	CreateMessage(ctx context.Context, m *Message) error
	// ListMessagesByConversation returns every message for
	// conversationID, oldest first (insertion order). An unknown
	// conversationID returns an empty slice, not an error — mirrors
	// ListTasks's own "conversation isn't a stored entity" stance;
	// there's nothing to 404 on at this layer.
	ListMessagesByConversation(ctx context.Context, conversationID string) ([]*Message, error)

	// CreateCredential, GetCredential, ListCredentials, and
	// DeleteCredential are the vault (design spec §7's second half).
	// Credential.Value is plaintext at this interface's boundary; a
	// backend implementation is responsible for encrypting it at rest
	// and decrypting on read.
	CreateCredential(ctx context.Context, c *Credential) error
	GetCredential(ctx context.Context, id string) (*Credential, error)
	ListCredentials(ctx context.Context) ([]*Credential, error)
	DeleteCredential(ctx context.Context, id string) error

	// CreateSession, GetSessionByTokenHash, ListSessions, TouchSession,
	// and DeleteSession back client auth (design spec §9). TouchSession
	// updates LastUsedAt (sliding expiration); a caller with an unknown
	// id gets ErrNotFound, matching every other Update-shaped method
	// here. ListSessions (LOOM-47) backs a "which devices are logged in"
	// view and revoke-by-id, alongside the existing self-revoke-only
	// DeleteSession (already used by /logout for the presented token).
	CreateSession(ctx context.Context, s *Session) error
	GetSessionByTokenHash(ctx context.Context, tokenHash string) (*Session, error)
	// ListSessions returns every session, ordered most-recently-used
	// first — the order a "revoke a device" UI would want a session
	// list rendered in.
	ListSessions(ctx context.Context) ([]*Session, error)
	TouchSession(ctx context.Context, id string, lastUsedAt time.Time) error
	DeleteSession(ctx context.Context, id string) error

	Close() error
}
