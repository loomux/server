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
	// SetTargetAgent records an agent CLI probe result (LOOM-71),
	// replacing any earlier one for the same target + agent type. A target
	// that doesn't exist is ErrConflict; deleting a target deletes its
	// probe results with it.
	SetTargetAgent(ctx context.Context, a *TargetAgent) error
	// ListTargetAgents returns every recorded probe result for targetID,
	// ordered by agent type. A target never probed returns an empty slice.
	ListTargetAgents(ctx context.Context, targetID string) ([]*TargetAgent, error)
	// SetTargetHealth records a target's latest health probe (LOOM-86),
	// replacing the previous one. A target that doesn't exist is
	// ErrConflict; deleting a target deletes its record with it.
	SetTargetHealth(ctx context.Context, h *TargetHealth) error
	// GetTargetHealth returns targetID's latest health probe, or
	// ErrNotFound if it has never been probed.
	GetTargetHealth(ctx context.Context, targetID string) (*TargetHealth, error)
	// ListTargetHealth returns every recorded health probe, one per
	// probed target, in no particular order (LOOM-122: GET /targets in one
	// query rather than one per target).
	ListTargetHealth(ctx context.Context) ([]*TargetHealth, error)

	CreateWorkspace(ctx context.Context, w *Workspace) error
	GetWorkspace(ctx context.Context, id string) (*Workspace, error)
	GetWorkspaceByName(ctx context.Context, name string) (*Workspace, error)
	ListWorkspaces(ctx context.Context) ([]*Workspace, error)
	UpdateWorkspace(ctx context.Context, w *Workspace) error
	// SetWorkspaceRollingSummary replaces (never appends to) a workspace's
	// rolling summary field.
	SetWorkspaceRollingSummary(ctx context.Context, id, summary string) error
	DeleteWorkspace(ctx context.Context, id string) error
	// DeleteWorkspaceAndTasks deletes a workspace together with every
	// task in it (their turns with them; messages keep their text, losing
	// only the task link) in one transaction (LOOM-70). ErrNotFound for an
	// unknown id; ErrConflict, deleting nothing, while a credential is
	// still scoped to the workspace.
	DeleteWorkspaceAndTasks(ctx context.Context, id string) error

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
	// SetTaskReapedAt records that the idle reaper tore the task's
	// session down, touching nothing else (LOOM-120): a whole-row
	// UpdateTask from the reaper could revert a concurrent status change.
	// ErrNotFound for an unknown id.
	SetTaskReapedAt(ctx context.Context, id string, at time.Time) error
	DeleteTask(ctx context.Context, id string) error
	// CreateTaskTurn records one turn of a task (LOOM-91), stamping
	// CreatedAt; a task that doesn't exist is ErrConflict. Deleting a
	// task deletes its turns.
	CreateTaskTurn(ctx context.Context, t *TaskTurn) error
	// ListTaskTurns returns taskID's turns, oldest first; none (or an
	// unknown task) is an empty slice.
	ListTaskTurns(ctx context.Context, taskID string) ([]*TaskTurn, error)

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
	// ListConversationActivity returns one ConversationActivity per
	// distinct conversation_id in the message log, in no particular
	// order; an empty log returns an empty slice. It is how a
	// conversation with no tasks at all is discovered (LOOM-62).
	ListConversationActivity(ctx context.Context) ([]*ConversationActivity, error)

	// CreateDispatch, GetDispatch, GetDispatchByIdempotencyKey,
	// ListDispatchesByConversation, ListDispatchesByStatus and
	// TransitionDispatch back dispatch jobs (LOOM-80).
	//
	// CreateDispatch stores d (Status queued, or as given) and, when
	// userMessage is non-nil, that message with DispatchID = d.ID, in one
	// transaction: both or neither. It fails with ErrIdempotencyKeyExists
	// when d.IdempotencyKey is already held, and ErrConversationBusy when
	// d.ConversationID already has a queued or running dispatch.
	CreateDispatch(ctx context.Context, d *Dispatch, userMessage *Message) error
	GetDispatch(ctx context.Context, id string) (*Dispatch, error)
	GetDispatchByIdempotencyKey(ctx context.Context, key string) (*Dispatch, error)
	// ListDispatchesByConversation returns oldest first; unknown is empty.
	ListDispatchesByConversation(ctx context.Context, conversationID string) ([]*Dispatch, error)
	// ListDispatchesByStatus returns every dispatch in any of statuses.
	ListDispatchesByStatus(ctx context.Context, statuses ...DispatchStatus) ([]*Dispatch, error)
	// TransitionDispatch writes d's status, reply, error, error class and
	// started/finished times, but only if the stored status is still
	// from; otherwise ErrDispatchStateChanged (ErrNotFound if no such
	// dispatch). It stamps d.UpdatedAt.
	TransitionDispatch(ctx context.Context, d *Dispatch, from DispatchStatus) error

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
