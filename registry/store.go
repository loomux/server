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
	// ListTasksByConversation returns conversationID's tasks, oldest
	// first; unknown is empty (LOOM-145: a conversation's event stream
	// polls this, not every task).
	ListTasksByConversation(ctx context.Context, conversationID string) ([]*Task, error)
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
	// ListTaskTurnsPage returns up to limit of taskID's turns, the latest
	// ones recorded before the turn beforeID (any, when empty), oldest
	// first; more reports whether earlier ones remain (LOOM-122). An
	// unknown beforeID is ErrNotFound.
	ListTaskTurnsPage(ctx context.Context, taskID, beforeID string, limit int) (turns []*TaskTurn, more bool, err error)
	// DeleteTaskTurnsBefore deletes every task's turns recorded before
	// cutoff (retention, LOOM-122), returning how many it deleted.
	DeleteTaskTurnsBefore(ctx context.Context, cutoff time.Time) (int, error)

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
	// ListMessagesAfter returns conversationID's messages stored after
	// the message afterID, in storage order (LOOM-145: a stream reads
	// only what's new). An empty or unknown afterID returns them all.
	ListMessagesAfter(ctx context.Context, conversationID, afterID string) ([]*Message, error)
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
	// ListDispatchesAfter returns conversationID's dispatches created
	// after the dispatch afterID, plus those named in also, in creation
	// order (LOOM-145: a stream rereads only new jobs and the ones it
	// last saw unfinished). An empty or unknown afterID returns them all.
	ListDispatchesAfter(ctx context.Context, conversationID, afterID string, also []string) ([]*Dispatch, error)
	// ListDispatchesByStatus returns every dispatch in any of statuses.
	ListDispatchesByStatus(ctx context.Context, statuses ...DispatchStatus) ([]*Dispatch, error)
	// TransitionDispatch writes d's status, reply, error, error class and
	// started/finished times, but only if the stored status is still
	// from; otherwise ErrDispatchStateChanged (ErrNotFound if no such
	// dispatch). It stamps d.UpdatedAt.
	TransitionDispatch(ctx context.Context, d *Dispatch, from DispatchStatus) error

	// CreateConfirmation, ResolveConfirmation,
	// ListConfirmationsByConversation and ExpirePendingConfirmations
	// record offers awaiting the user's yes (LOOM-123).
	CreateConfirmation(ctx context.Context, c *Confirmation) error
	// ResolveConfirmation moves a pending confirmation to status;
	// ErrConflict if it isn't pending any more, ErrNotFound if unknown.
	ResolveConfirmation(ctx context.Context, id string, status ConfirmationStatus) error
	// ListConfirmationsByConversation returns oldest first.
	ListConfirmationsByConversation(ctx context.Context, conversationID string) ([]*Confirmation, error)
	// ExpirePendingConfirmations marks every pending confirmation
	// expired, returning how many: at startup, since a restart forgets
	// the offers themselves.
	ExpirePendingConfirmations(ctx context.Context) (int, error)

	// SetTargetHostKeys pins (or, with "", unpins) a target's host keys
	// (LOOM-114); ErrNotFound if there's no such target.
	SetTargetHostKeys(ctx context.Context, id, hostKeys string) error

	// CreateDispatchEvent, ListDispatchEventsByConversation and
	// DeleteDispatchEventsBefore keep the audit trail (LOOM-110).
	// CreateDispatchEvent stamps CreatedAt if zero.
	CreateDispatchEvent(ctx context.Context, e *DispatchEvent) error
	// ListDispatchEventsByConversation returns oldest first; unknown is
	// empty.
	ListDispatchEventsByConversation(ctx context.Context, conversationID string) ([]*DispatchEvent, error)
	// DeleteDispatchEventsBefore deletes events recorded before cutoff
	// (retention), returning how many it deleted.
	DeleteDispatchEventsBefore(ctx context.Context, cutoff time.Time) (int, error)

	// CreateCredential, GetCredential, ListCredentials, and
	// DeleteCredential are the vault (design spec §7's second half).
	// Credential.Value is plaintext at this interface's boundary; a
	// backend implementation is responsible for encrypting it at rest
	// and decrypting on read.
	CreateCredential(ctx context.Context, c *Credential) error
	GetCredential(ctx context.Context, id string) (*Credential, error)
	ListCredentials(ctx context.Context) ([]*Credential, error)
	DeleteCredential(ctx context.Context, id string) error
	// ListCredentialInfo lists every credential without its value, and
	// without decrypting anything (LOOM-134): it works with no or a wrong
	// master key, so rows that no longer decrypt can still be found and
	// deleted.
	ListCredentialInfo(ctx context.Context) ([]*Credential, error)
	// SetCredentialValue replaces a credential's value (LOOM-134), keeping
	// its name and scope.
	SetCredentialValue(ctx context.Context, id, value string) error

	// CreateSSHKey, GetSSHKey, ListSSHKeys and DeleteSSHKey keep the SSH
	// keys Loomux manages (LOOM-138). CreateSSHKey stamps CreatedAt and
	// encrypts PrivateKey at rest (ErrNoMasterKey without a master key);
	// a duplicate name is ErrConflict. GetSSHKey decrypts it.
	// ListSSHKeys returns every key without PrivateKey, ordered by name,
	// decrypting nothing. DeleteSSHKey is ErrConflict while a target's
	// SSHKeyRef names the key; a target can only name a key that exists
	// (ErrConflict on create or update otherwise).
	CreateSSHKey(ctx context.Context, k *SSHKey) error
	GetSSHKey(ctx context.Context, id string) (*SSHKey, error)
	ListSSHKeys(ctx context.Context) ([]*SSHKey, error)
	DeleteSSHKey(ctx context.Context, id string) error

	// SetRouterTier, ListRouterTiers, DeleteRouterTier and
	// ListRouterSettingsChanges keep the router model tiers set through
	// Settings and their audit trail (LOOM-185). SetRouterTier inserts or
	// replaces t's row and records change in the same transaction,
	// stamping both times; APIKey is encrypted at rest (ErrNoMasterKey
	// without a master key). ListRouterTiers decrypts every row, ordered
	// by tier; a row that doesn't decrypt (or any, without a master key)
	// is returned without APIKey and with the error joined into err (the
	// others still come back).
	// DeleteRouterTier deletes the row and records change, ErrNotFound if
	// there is none. ListRouterSettingsChanges returns up to limit
	// entries, newest first.
	SetRouterTier(ctx context.Context, t *RouterTier, change *RouterSettingsChange) error
	ListRouterTiers(ctx context.Context) ([]*RouterTier, error)
	DeleteRouterTier(ctx context.Context, tier string, change *RouterSettingsChange) error
	ListRouterSettingsChanges(ctx context.Context, limit int) ([]*RouterSettingsChange, error)

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

	// CreatePlugin, GetPlugin, ListPlugins, UpdatePlugin, SetPluginConfig
	// and DeletePlugin keep the installed plugins (LOOM-178). CreatePlugin
	// stamps InstalledAt/UpdatedAt and encrypts Secrets at rest
	// (ErrNoMasterKey if there are any and no master key); a duplicate
	// label is ErrConflict. GetPlugin decrypts Secrets (a row that doesn't
	// decrypt comes back with Secrets nil and ErrNoMasterKey, or the
	// decryption error, joined). ListPlugins returns every plugin ordered by
	// label, decrypting nothing. UpdatePlugin writes status, reason,
	// enabled, version, protocol, capabilities, path and trust, never the
	// configuration; SetPluginConfig replaces the configuration and the
	// secrets (nil or empty secrets store none). Unknown ids are
	// ErrNotFound.
	CreatePlugin(ctx context.Context, p *Plugin) error
	GetPlugin(ctx context.Context, id string) (*Plugin, error)
	ListPlugins(ctx context.Context) ([]*Plugin, error)
	UpdatePlugin(ctx context.Context, p *Plugin) error
	SetPluginConfig(ctx context.Context, id string, config map[string]any, secrets map[string]string) error
	DeletePlugin(ctx context.Context, id string) error

	// CreateEnvironment, GetEnvironment, GetEnvironmentByTarget,
	// ListEnvironments, ListEnvironmentsByPlugin, UpdateEnvironment and
	// DeleteEnvironment keep the machines plugins made (LOOM-178,
	// docs/design/target-providers.md §1.7). CreateEnvironment stamps
	// CreatedAt/UpdatedAt and encrypts HostPrivateKey at rest
	// (ErrNoMasterKey without a key); its target must exist and have no
	// environment yet (ErrConflict). GetEnvironment decrypts the key;
	// the list methods never do. UpdateEnvironment writes status, reason,
	// plugin id and version, image and digest. Unknown ids are
	// ErrNotFound; GetEnvironmentByTarget is ErrNotFound for a target
	// with none. A target with an environment can't be deleted
	// (ErrConflict), nor a plugin that environments still name.
	CreateEnvironment(ctx context.Context, e *Environment) error
	GetEnvironment(ctx context.Context, id string) (*Environment, error)
	GetEnvironmentByTarget(ctx context.Context, targetID string) (*Environment, error)
	ListEnvironments(ctx context.Context) ([]*Environment, error)
	ListEnvironmentsByPlugin(ctx context.Context, pluginID string) ([]*Environment, error)
	UpdateEnvironment(ctx context.Context, e *Environment) error
	DeleteEnvironment(ctx context.Context, id string) error

	// GetSetting and SetSetting keep server-wide settings by key
	// (LOOM-178: the instance id). GetSetting is ErrNotFound for a key
	// never set; SetSetting creates or overwrites.
	GetSetting(ctx context.Context, key string) (string, error)
	SetSetting(ctx context.Context, key, value string) error

	Close() error
}
