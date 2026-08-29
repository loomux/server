package registry

import "context"

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
	UpdateTask(ctx context.Context, t *Task) error
	DeleteTask(ctx context.Context, id string) error

	// CreateCredential, GetCredential, ListCredentials, and
	// DeleteCredential are the vault (design spec §7's second half).
	// Credential.Value is plaintext at this interface's boundary; a
	// backend implementation is responsible for encrypting it at rest
	// and decrypting on read.
	CreateCredential(ctx context.Context, c *Credential) error
	GetCredential(ctx context.Context, id string) (*Credential, error)
	ListCredentials(ctx context.Context) ([]*Credential, error)
	DeleteCredential(ctx context.Context, id string) error

	Close() error
}
