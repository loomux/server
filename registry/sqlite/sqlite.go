// Package sqlite is a registry.Store implementation backed by SQLite via
// the pure-Go modernc.org/sqlite driver (no cgo), so the server can still
// build as a single static binary.
package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"

	"github.com/Loomux/server/registry"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Store is a registry.Store backed by a SQLite database.
type Store struct {
	db        *sql.DB
	masterKey []byte // nil unless WithMasterKey was passed to Open
}

// Open opens (creating if necessary) a SQLite database at path and applies
// any pending migrations. The design spec calls for migrations tracked in
// a "schema_migrations" table; goose's own default table name doesn't
// match, so it's overridden here to keep that contract regardless of
// which migration tool runs it.
//
// Credential operations (§7's vault) need an AES-256 key, supplied via
// WithMasterKey — most callers never touch credentials, so this is an
// option, not a required parameter; a credential operation attempted
// without one fails fast with a clear error instead.

// sqliteBusyTimeout is the per-connection busy timeout applied to every
// pooled SQLite connection. 5s is long enough to absorb the short races
// between the background reaper and foreground Dispatch paths (LOOM-34)
// while staying short enough to fail fast if a real deadlock occurs.
const sqliteBusyTimeout = 5000

// dsn builds a driver DSN that applies foreign-key enforcement and a busy
// timeout on every connection the pool opens. Using DSN parameters instead
// of db.Exec("PRAGMA ...") is required because PRAGMAs set via Exec only
// affect the single connection the statement happens to run on (LOOM-67).
func dsn(path string) string {
	if path == "" {
		return path
	}
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return fmt.Sprintf("%s%s_foreign_keys=1&_busy_timeout=%d", path, sep, sqliteBusyTimeout)
}

func Open(path string, opts ...Option) (*Store, error) {
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("sqlite: open %s: %w", path, err)
	}

	goose.SetTableName("schema_migrations")
	goose.SetBaseFS(migrationsFS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite: set migration dialect: %w", err)
	}
	if err := goose.Up(db, "migrations"); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite: run migrations: %w", err)
	}

	s := &Store{db: db}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

// Close closes the underlying database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) CreateTarget(ctx context.Context, t *registry.Target) error {
	now := time.Now().UTC()
	t.CreatedAt = now
	t.UpdatedAt = now
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO targets (id, name, kind, host, user, ssh_key_ref, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.Name, string(t.Kind), t.Host, t.User, t.SSHKeyRef, t.CreatedAt, t.UpdatedAt,
	)
	if isUniqueConstraintErr(err) {
		return fmt.Errorf("%w: target name %q already exists", registry.ErrConflict, t.Name)
	}
	if err != nil {
		return fmt.Errorf("sqlite: create target: %w", err)
	}
	return nil
}

func (s *Store) GetTarget(ctx context.Context, id string) (*registry.Target, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, name, kind, host, user, ssh_key_ref, created_at, updated_at
		FROM targets WHERE id = ?`, id)
	t, err := scanTarget(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: target %q", registry.ErrNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: get target: %w", err)
	}
	return t, nil
}

func (s *Store) ListTargets(ctx context.Context) ([]*registry.Target, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, kind, host, user, ssh_key_ref, created_at, updated_at
		FROM targets ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list targets: %w", err)
	}
	defer rows.Close()

	var out []*registry.Target
	for rows.Next() {
		t, err := scanTarget(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite: list targets: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: list targets: %w", err)
	}
	return out, nil
}

func (s *Store) UpdateTarget(ctx context.Context, t *registry.Target) error {
	t.UpdatedAt = time.Now().UTC()
	res, err := s.db.ExecContext(ctx, `
		UPDATE targets SET name = ?, kind = ?, host = ?, user = ?, ssh_key_ref = ?, updated_at = ?
		WHERE id = ?`,
		t.Name, string(t.Kind), t.Host, t.User, t.SSHKeyRef, t.UpdatedAt, t.ID,
	)
	if isUniqueConstraintErr(err) {
		return fmt.Errorf("%w: target name %q already exists", registry.ErrConflict, t.Name)
	}
	if err != nil {
		return fmt.Errorf("sqlite: update target: %w", err)
	}
	return requireRowAffected(res, "target", t.ID)
}

func (s *Store) DeleteTarget(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM targets WHERE id = ?`, id)
	if isForeignKeyConstraintErr(err) {
		return fmt.Errorf("%w: target %q still has workspaces referencing it", registry.ErrConflict, id)
	}
	if err != nil {
		return fmt.Errorf("sqlite: delete target: %w", err)
	}
	return requireRowAffected(res, "target", id)
}

func (s *Store) CreateWorkspace(ctx context.Context, w *registry.Workspace) error {
	now := time.Now().UTC()
	w.CreatedAt = now
	w.UpdatedAt = now
	tags, err := json.Marshal(w.Tags)
	if err != nil {
		return fmt.Errorf("sqlite: marshal tags: %w", err)
	}
	caps, err := json.Marshal(w.Capabilities)
	if err != nil {
		return fmt.Errorf("sqlite: marshal capabilities: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO workspaces (
			id, name, path, target_id, git_remote, tags, description, capabilities,
			status, is_dynamic, last_used_at, rolling_summary, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		w.ID, w.Name, w.Path, w.TargetID, w.GitRemote, string(tags), w.Description, string(caps),
		string(w.Status), w.IsDynamic, w.LastUsedAt, w.RollingSummary, w.CreatedAt, w.UpdatedAt,
	)
	if isUniqueConstraintErr(err) {
		return fmt.Errorf("%w: workspace name %q already exists", registry.ErrConflict, w.Name)
	}
	if isForeignKeyConstraintErr(err) {
		return fmt.Errorf("%w: target %q does not exist", registry.ErrConflict, w.TargetID)
	}
	if err != nil {
		return fmt.Errorf("sqlite: create workspace: %w", err)
	}
	return nil
}

const workspaceColumns = `
	id, name, path, target_id, git_remote, tags, description, capabilities,
	status, is_dynamic, last_used_at, rolling_summary, created_at, updated_at`

func (s *Store) GetWorkspace(ctx context.Context, id string) (*registry.Workspace, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+workspaceColumns+` FROM workspaces WHERE id = ?`, id)
	w, err := scanWorkspace(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: workspace %q", registry.ErrNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: get workspace: %w", err)
	}
	return w, nil
}

func (s *Store) GetWorkspaceByName(ctx context.Context, name string) (*registry.Workspace, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+workspaceColumns+` FROM workspaces WHERE name = ?`, name)
	w, err := scanWorkspace(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: workspace %q", registry.ErrNotFound, name)
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: get workspace by name: %w", err)
	}
	return w, nil
}

func (s *Store) ListWorkspaces(ctx context.Context) ([]*registry.Workspace, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+workspaceColumns+` FROM workspaces ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list workspaces: %w", err)
	}
	defer rows.Close()

	var out []*registry.Workspace
	for rows.Next() {
		w, err := scanWorkspace(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite: list workspaces: %w", err)
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: list workspaces: %w", err)
	}
	return out, nil
}

func (s *Store) UpdateWorkspace(ctx context.Context, w *registry.Workspace) error {
	w.UpdatedAt = time.Now().UTC()
	tags, err := json.Marshal(w.Tags)
	if err != nil {
		return fmt.Errorf("sqlite: marshal tags: %w", err)
	}
	caps, err := json.Marshal(w.Capabilities)
	if err != nil {
		return fmt.Errorf("sqlite: marshal capabilities: %w", err)
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE workspaces SET
			name = ?, path = ?, target_id = ?, git_remote = ?, tags = ?, description = ?,
			capabilities = ?, status = ?, is_dynamic = ?, last_used_at = ?, updated_at = ?
		WHERE id = ?`,
		w.Name, w.Path, w.TargetID, w.GitRemote, string(tags), w.Description,
		string(caps), string(w.Status), w.IsDynamic, w.LastUsedAt, w.UpdatedAt, w.ID,
	)
	if isUniqueConstraintErr(err) {
		return fmt.Errorf("%w: workspace name %q already exists", registry.ErrConflict, w.Name)
	}
	if isForeignKeyConstraintErr(err) {
		return fmt.Errorf("%w: target %q does not exist", registry.ErrConflict, w.TargetID)
	}
	if err != nil {
		return fmt.Errorf("sqlite: update workspace: %w", err)
	}
	return requireRowAffected(res, "workspace", w.ID)
}

// SetWorkspaceRollingSummary is the only way to change rolling_summary: a
// direct SET, never an append, so "replace not accumulate" (design spec §2)
// can't be violated by a caller building its own UPDATE.
func (s *Store) SetWorkspaceRollingSummary(ctx context.Context, id, summary string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE workspaces SET rolling_summary = ?, updated_at = ? WHERE id = ?`,
		summary, time.Now().UTC(), id,
	)
	if err != nil {
		return fmt.Errorf("sqlite: set workspace rolling summary: %w", err)
	}
	return requireRowAffected(res, "workspace", id)
}

func (s *Store) DeleteWorkspace(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM workspaces WHERE id = ?`, id)
	if isForeignKeyConstraintErr(err) {
		return fmt.Errorf("%w: workspace %q still has tasks referencing it", registry.ErrConflict, id)
	}
	if err != nil {
		return fmt.Errorf("sqlite: delete workspace: %w", err)
	}
	return requireRowAffected(res, "workspace", id)
}

func scanWorkspace(row rowScanner) (*registry.Workspace, error) {
	var w registry.Workspace
	var status, tags, caps string
	if err := row.Scan(
		&w.ID, &w.Name, &w.Path, &w.TargetID, &w.GitRemote, &tags, &w.Description, &caps,
		&status, &w.IsDynamic, &w.LastUsedAt, &w.RollingSummary, &w.CreatedAt, &w.UpdatedAt,
	); err != nil {
		return nil, err
	}
	w.Status = registry.WorkspaceStatus(status)
	if err := json.Unmarshal([]byte(tags), &w.Tags); err != nil {
		return nil, fmt.Errorf("unmarshal tags: %w", err)
	}
	if err := json.Unmarshal([]byte(caps), &w.Capabilities); err != nil {
		return nil, fmt.Errorf("unmarshal capabilities: %w", err)
	}
	return &w, nil
}

func (s *Store) CreateTask(ctx context.Context, t *registry.Task) error {
	now := time.Now().UTC()
	t.CreatedAt = now
	t.UpdatedAt = now
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO tasks (
			id, workspace_id, kind, agent_type, tmux_session, status, conversation_id,
			created_at, updated_at, started_at, completed_at, reaped_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.WorkspaceID, string(t.Kind), t.AgentType, t.TmuxSession, string(t.Status), t.ConversationID,
		t.CreatedAt, t.UpdatedAt, t.StartedAt, t.CompletedAt, t.ReapedAt,
	)
	if isForeignKeyConstraintErr(err) {
		return fmt.Errorf("%w: workspace %q does not exist", registry.ErrConflict, t.WorkspaceID)
	}
	if err != nil {
		return fmt.Errorf("sqlite: create task: %w", err)
	}
	return nil
}

const taskColumns = `
	id, workspace_id, kind, agent_type, tmux_session, status, conversation_id,
	created_at, updated_at, started_at, completed_at, reaped_at`

func (s *Store) GetTask(ctx context.Context, id string) (*registry.Task, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id = ?`, id)
	task, err := scanTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: task %q", registry.ErrNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: get task: %w", err)
	}
	return task, nil
}

func (s *Store) ListTasksByWorkspace(ctx context.Context, workspaceID string) ([]*registry.Task, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+taskColumns+` FROM tasks WHERE workspace_id = ? ORDER BY created_at`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list tasks by workspace: %w", err)
	}
	defer rows.Close()

	var out []*registry.Task
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite: list tasks by workspace: %w", err)
		}
		out = append(out, task)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: list tasks by workspace: %w", err)
	}
	return out, nil
}

func (s *Store) UpdateTask(ctx context.Context, t *registry.Task) error {
	t.UpdatedAt = time.Now().UTC()
	res, err := s.db.ExecContext(ctx, `
		UPDATE tasks SET
			kind = ?, agent_type = ?, tmux_session = ?, status = ?, conversation_id = ?,
			updated_at = ?, started_at = ?, completed_at = ?, reaped_at = ?
		WHERE id = ?`,
		string(t.Kind), t.AgentType, t.TmuxSession, string(t.Status), t.ConversationID,
		t.UpdatedAt, t.StartedAt, t.CompletedAt, t.ReapedAt, t.ID,
	)
	if err != nil {
		return fmt.Errorf("sqlite: update task: %w", err)
	}
	return requireRowAffected(res, "task", t.ID)
}

func (s *Store) DeleteTask(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM tasks WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("sqlite: delete task: %w", err)
	}
	return requireRowAffected(res, "task", id)
}

func (s *Store) ListTasks(ctx context.Context) ([]*registry.Task, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+taskColumns+` FROM tasks ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list tasks: %w", err)
	}
	defer rows.Close()

	var out []*registry.Task
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite: list tasks: %w", err)
		}
		out = append(out, task)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: list tasks: %w", err)
	}
	return out, nil
}

func scanTask(row rowScanner) (*registry.Task, error) {
	var t registry.Task
	var kind, status string
	if err := row.Scan(
		&t.ID, &t.WorkspaceID, &kind, &t.AgentType, &t.TmuxSession, &status, &t.ConversationID,
		&t.CreatedAt, &t.UpdatedAt, &t.StartedAt, &t.CompletedAt, &t.ReapedAt,
	); err != nil {
		return nil, err
	}
	t.Kind = registry.TaskKind(kind)
	t.Status = registry.TaskStatus(status)
	return &t, nil
}

func (s *Store) CreateMessage(ctx context.Context, m *registry.Message) error {
	m.CreatedAt = time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO messages (id, conversation_id, task_id, role, content, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		m.ID, m.ConversationID, nullIfEmpty(m.TaskID), string(m.Role), m.Content, m.CreatedAt,
	)
	if isForeignKeyConstraintErr(err) {
		return fmt.Errorf("%w: task %q does not exist", registry.ErrConflict, m.TaskID)
	}
	if err != nil {
		return fmt.Errorf("sqlite: create message: %w", err)
	}
	return nil
}

const messageColumns = `id, conversation_id, task_id, role, content, created_at`

// ListMessagesByConversation orders by created_at then the table's
// implicit rowid — the rowid tiebreak guarantees insertion order even
// when two messages in the same turn (a user message immediately
// followed by its assistant reply) land on a created_at value with
// insufficient resolution to distinguish them on its own.
func (s *Store) ListMessagesByConversation(ctx context.Context, conversationID string) ([]*registry.Message, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+messageColumns+` FROM messages
		WHERE conversation_id = ?
		ORDER BY created_at, rowid`, conversationID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list messages by conversation: %w", err)
	}
	defer rows.Close()

	out := make([]*registry.Message, 0)
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite: list messages by conversation: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: list messages by conversation: %w", err)
	}
	return out, nil
}

func scanMessage(row rowScanner) (*registry.Message, error) {
	var m registry.Message
	var taskID sql.NullString
	var role string
	if err := row.Scan(&m.ID, &m.ConversationID, &taskID, &role, &m.Content, &m.CreatedAt); err != nil {
		return nil, err
	}
	m.TaskID = taskID.String
	m.Role = registry.MessageRole(role)
	return &m, nil
}

func (s *Store) CreateCredential(ctx context.Context, c *registry.Credential) error {
	if s.masterKey == nil {
		return fmt.Errorf("sqlite: no master key configured (see WithMasterKey); cannot create credential")
	}
	now := time.Now().UTC()
	c.CreatedAt = now
	c.UpdatedAt = now

	ciphertext, err := encrypt(s.masterKey, c.Value)
	if err != nil {
		return fmt.Errorf("sqlite: encrypt credential: %w", err)
	}

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO credentials (id, name, workspace_id, agent_type, ciphertext, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.Name, nullIfEmpty(c.WorkspaceID), c.AgentType, ciphertext, c.CreatedAt, c.UpdatedAt,
	)
	if isUniqueConstraintErr(err) {
		return fmt.Errorf("%w: credential %q already exists at this scope", registry.ErrConflict, c.Name)
	}
	if isForeignKeyConstraintErr(err) {
		return fmt.Errorf("%w: workspace %q does not exist", registry.ErrConflict, c.WorkspaceID)
	}
	if err != nil {
		return fmt.Errorf("sqlite: create credential: %w", err)
	}
	return nil
}

const credentialColumns = `
	id, name, workspace_id, agent_type, ciphertext, created_at, updated_at`

func (s *Store) GetCredential(ctx context.Context, id string) (*registry.Credential, error) {
	if s.masterKey == nil {
		return nil, fmt.Errorf("sqlite: no master key configured (see WithMasterKey); cannot read credential")
	}
	row := s.db.QueryRowContext(ctx, `SELECT `+credentialColumns+` FROM credentials WHERE id = ?`, id)
	c, err := scanCredential(row, s.masterKey)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: credential %q", registry.ErrNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: get credential: %w", err)
	}
	return c, nil
}

func (s *Store) ListCredentials(ctx context.Context) ([]*registry.Credential, error) {
	if s.masterKey == nil {
		return nil, fmt.Errorf("sqlite: no master key configured (see WithMasterKey); cannot list credentials")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+credentialColumns+` FROM credentials ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list credentials: %w", err)
	}
	defer rows.Close()

	var out []*registry.Credential
	for rows.Next() {
		c, err := scanCredential(rows, s.masterKey)
		if err != nil {
			return nil, fmt.Errorf("sqlite: list credentials: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: list credentials: %w", err)
	}
	return out, nil
}

func (s *Store) DeleteCredential(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM credentials WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("sqlite: delete credential: %w", err)
	}
	return requireRowAffected(res, "credential", id)
}

func scanCredential(row rowScanner, key []byte) (*registry.Credential, error) {
	var c registry.Credential
	var workspaceID sql.NullString
	var ciphertext []byte
	if err := row.Scan(&c.ID, &c.Name, &workspaceID, &c.AgentType, &ciphertext, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, err
	}
	c.WorkspaceID = workspaceID.String
	plaintext, err := decrypt(key, ciphertext)
	if err != nil {
		return nil, fmt.Errorf("decrypt credential %q: %w", c.ID, err)
	}
	c.Value = plaintext
	return &c, nil
}

func (s *Store) CreateSession(ctx context.Context, sess *registry.Session) error {
	now := time.Now().UTC()
	sess.CreatedAt = now
	if sess.LastUsedAt.IsZero() {
		sess.LastUsedAt = now
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sessions (id, token_hash, created_at, last_used_at)
		VALUES (?, ?, ?, ?)`,
		sess.ID, sess.TokenHash, sess.CreatedAt, sess.LastUsedAt,
	)
	if isUniqueConstraintErr(err) {
		return fmt.Errorf("%w: session with this token already exists", registry.ErrConflict)
	}
	if err != nil {
		return fmt.Errorf("sqlite: create session: %w", err)
	}
	return nil
}

const sessionColumns = `id, token_hash, created_at, last_used_at`

func (s *Store) GetSessionByTokenHash(ctx context.Context, tokenHash string) (*registry.Session, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+sessionColumns+` FROM sessions WHERE token_hash = ?`, tokenHash)
	sess, err := scanSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: session", registry.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: get session: %w", err)
	}
	return sess, nil
}

func (s *Store) ListSessions(ctx context.Context) ([]*registry.Session, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+sessionColumns+` FROM sessions ORDER BY last_used_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list sessions: %w", err)
	}
	defer rows.Close()

	var out []*registry.Session
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite: list sessions: %w", err)
		}
		out = append(out, sess)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: list sessions: %w", err)
	}
	return out, nil
}

func (s *Store) TouchSession(ctx context.Context, id string, lastUsedAt time.Time) error {
	res, err := s.db.ExecContext(ctx, `UPDATE sessions SET last_used_at = ? WHERE id = ?`, lastUsedAt, id)
	if err != nil {
		return fmt.Errorf("sqlite: touch session: %w", err)
	}
	return requireRowAffected(res, "session", id)
}

func (s *Store) DeleteSession(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("sqlite: delete session: %w", err)
	}
	return requireRowAffected(res, "session", id)
}

func scanSession(row rowScanner) (*registry.Session, error) {
	var sess registry.Session
	if err := row.Scan(&sess.ID, &sess.TokenHash, &sess.CreatedAt, &sess.LastUsedAt); err != nil {
		return nil, err
	}
	return &sess, nil
}

// nullIfEmpty maps Go's "" (this codebase's usual empty-means-absent
// convention) to a genuine SQL NULL for workspace_id specifically, so
// FOREIGN KEY ... ON DELETE RESTRICT applies correctly (SQLite exempts
// NULL from FK checks) and an unscoped credential never blocks a
// workspace's deletion.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanTarget(row rowScanner) (*registry.Target, error) {
	var t registry.Target
	var kind string
	if err := row.Scan(&t.ID, &t.Name, &kind, &t.Host, &t.User, &t.SSHKeyRef, &t.CreatedAt, &t.UpdatedAt); err != nil {
		return nil, err
	}
	t.Kind = registry.TargetKind(kind)
	return &t, nil
}

func requireRowAffected(res sql.Result, entity, id string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlite: rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %s %q", registry.ErrNotFound, entity, id)
	}
	return nil
}
