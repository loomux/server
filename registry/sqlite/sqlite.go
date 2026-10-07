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
	"slices"
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

// Ping verifies the database connection is alive. It satisfies both
// registry.Store's implicit health contract and api/health's DB probe.
func (s *Store) Ping(ctx context.Context) error {
	if err := s.db.PingContext(ctx); err != nil {
		return fmt.Errorf("sqlite: ping: %w", err)
	}
	return nil
}

func (s *Store) CreateTarget(ctx context.Context, t *registry.Target) error {
	allowed, err := marshalAllowedAgents(t.Policy.AllowedAgentTypes)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	t.CreatedAt = now
	t.UpdatedAt = now
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO targets (id, name, kind, host, user, ssh_key_ref, workspace_root, permission_mode,
			purpose, allowed_agent_types, no_provision, no_shell, require_confirmation, relay, ssh_port, host_keys,
			created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.Name, string(t.Kind), t.Host, t.User, t.SSHKeyRef, t.WorkspaceRoot, t.PermissionMode,
		t.Policy.Purpose, allowed, t.Policy.NoProvision, t.Policy.NoShell, t.Policy.RequireConfirmation, t.Policy.Relay,
		t.SSHPort, t.HostKeys,
		t.CreatedAt, t.UpdatedAt,
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
		SELECT `+targetColumns+`
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
		SELECT `+targetColumns+`
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
	allowed, err := marshalAllowedAgents(t.Policy.AllowedAgentTypes)
	if err != nil {
		return err
	}
	t.UpdatedAt = time.Now().UTC()
	res, err := s.db.ExecContext(ctx, `
		UPDATE targets SET name = ?, kind = ?, host = ?, user = ?, ssh_key_ref = ?, workspace_root = ?, permission_mode = ?,
			purpose = ?, allowed_agent_types = ?, no_provision = ?, no_shell = ?, require_confirmation = ?, relay = ?,
			ssh_port = ?, updated_at = ?
		WHERE id = ?`,
		t.Name, string(t.Kind), t.Host, t.User, t.SSHKeyRef, t.WorkspaceRoot, t.PermissionMode,
		t.Policy.Purpose, allowed, t.Policy.NoProvision, t.Policy.NoShell, t.Policy.RequireConfirmation, t.Policy.Relay, t.SSHPort,
		t.UpdatedAt, t.ID,
	)
	if isUniqueConstraintErr(err) {
		return fmt.Errorf("%w: target name %q already exists", registry.ErrConflict, t.Name)
	}
	if err != nil {
		return fmt.Errorf("sqlite: update target: %w", err)
	}
	return requireRowAffected(res, "target", t.ID)
}

func (s *Store) SetTargetHostKeys(ctx context.Context, id, hostKeys string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE targets SET host_keys = ?, updated_at = ? WHERE id = ?`,
		hostKeys, time.Now().UTC(), id)
	if err != nil {
		return fmt.Errorf("sqlite: set target host keys: %w", err)
	}
	return requireRowAffected(res, "target", id)
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

// SetTargetAgent upserts one agent CLI probe result (LOOM-71).
func (s *Store) SetTargetAgent(ctx context.Context, a *registry.TargetAgent) error {
	if a.CheckedAt.IsZero() {
		a.CheckedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO target_agents (target_id, agent_type, available, path, version, auth_status, checked_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (target_id, agent_type) DO UPDATE SET
			available = excluded.available, path = excluded.path, version = excluded.version,
			auth_status = excluded.auth_status, checked_at = excluded.checked_at`,
		a.TargetID, a.AgentType, a.Available, a.Path, a.Version, a.AuthStatus, a.CheckedAt,
	)
	if isForeignKeyConstraintErr(err) {
		return fmt.Errorf("%w: target %q does not exist", registry.ErrConflict, a.TargetID)
	}
	if err != nil {
		return fmt.Errorf("sqlite: set target agent: %w", err)
	}
	return nil
}

func (s *Store) ListTargetAgents(ctx context.Context, targetID string) ([]*registry.TargetAgent, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT target_id, agent_type, available, path, version, auth_status, checked_at FROM target_agents
		WHERE target_id = ? ORDER BY agent_type`, targetID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list target agents: %w", err)
	}
	defer rows.Close()

	out := []*registry.TargetAgent{}
	for rows.Next() {
		var a registry.TargetAgent
		if err := rows.Scan(&a.TargetID, &a.AgentType, &a.Available, &a.Path, &a.Version, &a.AuthStatus, &a.CheckedAt); err != nil {
			return nil, fmt.Errorf("sqlite: list target agents: %w", err)
		}
		out = append(out, &a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: list target agents: %w", err)
	}
	return out, nil
}

// SetTargetHealth upserts a target's latest health probe (LOOM-86).
func (s *Store) SetTargetHealth(ctx context.Context, h *registry.TargetHealth) error {
	if h.ProbedAt.IsZero() {
		h.ProbedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO target_health (target_id, reachable, error, latency_ms, tmux_version, disk_free_bytes, probed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (target_id) DO UPDATE SET
			reachable = excluded.reachable, error = excluded.error, latency_ms = excluded.latency_ms,
			tmux_version = excluded.tmux_version, disk_free_bytes = excluded.disk_free_bytes,
			probed_at = excluded.probed_at`,
		h.TargetID, h.Reachable, h.Error, h.Latency.Milliseconds(), h.TmuxVersion, h.DiskFreeBytes, h.ProbedAt,
	)
	if isForeignKeyConstraintErr(err) {
		return fmt.Errorf("%w: target %q does not exist", registry.ErrConflict, h.TargetID)
	}
	if err != nil {
		return fmt.Errorf("sqlite: set target health: %w", err)
	}
	return nil
}

const targetHealthColumns = `target_id, reachable, error, latency_ms, tmux_version, disk_free_bytes, probed_at`

func (s *Store) GetTargetHealth(ctx context.Context, targetID string) (*registry.TargetHealth, error) {
	h, err := scanTargetHealth(s.db.QueryRowContext(ctx,
		`SELECT `+targetHealthColumns+` FROM target_health WHERE target_id = ?`, targetID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: health of target %q", registry.ErrNotFound, targetID)
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: get target health: %w", err)
	}
	return h, nil
}

func (s *Store) ListTargetHealth(ctx context.Context) ([]*registry.TargetHealth, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+targetHealthColumns+` FROM target_health`)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list target health: %w", err)
	}
	defer rows.Close()
	out := []*registry.TargetHealth{}
	for rows.Next() {
		h, err := scanTargetHealth(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite: list target health: %w", err)
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: list target health: %w", err)
	}
	return out, nil
}

func scanTargetHealth(row rowScanner) (*registry.TargetHealth, error) {
	var h registry.TargetHealth
	var latencyMS int64
	if err := row.Scan(&h.TargetID, &h.Reachable, &h.Error, &latencyMS, &h.TmuxVersion, &h.DiskFreeBytes, &h.ProbedAt); err != nil {
		return nil, err
	}
	h.Latency = time.Duration(latencyMS) * time.Millisecond
	return &h, nil
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
			status, is_dynamic, last_used_at, rolling_summary, status_reason, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		w.ID, w.Name, w.Path, w.TargetID, w.GitRemote, string(tags), w.Description, string(caps),
		string(w.Status), w.IsDynamic, w.LastUsedAt, w.RollingSummary, w.StatusReason, w.CreatedAt, w.UpdatedAt,
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
	status, is_dynamic, last_used_at, rolling_summary, status_reason, created_at, updated_at`

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
			capabilities = ?, status = ?, is_dynamic = ?, last_used_at = ?, status_reason = ?, updated_at = ?
		WHERE id = ?`,
		w.Name, w.Path, w.TargetID, w.GitRemote, string(tags), w.Description,
		string(caps), string(w.Status), w.IsDynamic, w.LastUsedAt, w.StatusReason, w.UpdatedAt, w.ID,
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

func (s *Store) DeleteWorkspaceAndTasks(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: delete workspace and tasks: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM tasks WHERE workspace_id = ?`, id); err != nil {
		return fmt.Errorf("sqlite: delete workspace tasks: %w", err)
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM workspaces WHERE id = ?`, id)
	if isForeignKeyConstraintErr(err) {
		return fmt.Errorf("%w: workspace %q still has credentials scoped to it", registry.ErrConflict, id)
	}
	if err != nil {
		return fmt.Errorf("sqlite: delete workspace: %w", err)
	}
	if err := requireRowAffected(res, "workspace", id); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: delete workspace and tasks: %w", err)
	}
	return nil
}

func scanWorkspace(row rowScanner) (*registry.Workspace, error) {
	var w registry.Workspace
	var status, tags, caps string
	if err := row.Scan(
		&w.ID, &w.Name, &w.Path, &w.TargetID, &w.GitRemote, &tags, &w.Description, &caps,
		&status, &w.IsDynamic, &w.LastUsedAt, &w.RollingSummary, &w.StatusReason, &w.CreatedAt, &w.UpdatedAt,
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
			created_at, updated_at, started_at, completed_at, reaped_at, command, exit_code,
			failure_reason, error_class, output_tail, attention
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.WorkspaceID, string(t.Kind), t.AgentType, t.TmuxSession, string(t.Status), t.ConversationID,
		t.CreatedAt, t.UpdatedAt, t.StartedAt, t.CompletedAt, t.ReapedAt, t.Command, t.ExitCode,
		t.FailureReason, string(t.ErrorClass), t.OutputTail, attentionJSON(t.Attention),
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
	created_at, updated_at, started_at, completed_at, reaped_at, command, exit_code,
	failure_reason, error_class, output_tail, attention`

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

func (s *Store) SetTaskReapedAt(ctx context.Context, id string, at time.Time) error {
	res, err := s.db.ExecContext(ctx, `UPDATE tasks SET reaped_at = ?, updated_at = ? WHERE id = ?`, at, time.Now().UTC(), id)
	if err != nil {
		return fmt.Errorf("sqlite: set task reaped_at: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("sqlite: set task reaped_at: %w", err)
	} else if n == 0 {
		return fmt.Errorf("%w: task %q", registry.ErrNotFound, id)
	}
	return nil
}

func (s *Store) UpdateTask(ctx context.Context, t *registry.Task) error {
	t.UpdatedAt = time.Now().UTC()
	res, err := s.db.ExecContext(ctx, `
		UPDATE tasks SET
			kind = ?, agent_type = ?, tmux_session = ?, status = ?, conversation_id = ?,
			updated_at = ?, started_at = ?, completed_at = ?, reaped_at = ?, command = ?, exit_code = ?,
			failure_reason = ?, error_class = ?, output_tail = ?, attention = ?
		WHERE id = ?`,
		string(t.Kind), t.AgentType, t.TmuxSession, string(t.Status), t.ConversationID,
		t.UpdatedAt, t.StartedAt, t.CompletedAt, t.ReapedAt, t.Command, t.ExitCode,
		t.FailureReason, string(t.ErrorClass), t.OutputTail, attentionJSON(t.Attention), t.ID,
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

// CreateTaskTurn records one turn of a task (LOOM-91).
func (s *Store) CreateTaskTurn(ctx context.Context, t *registry.TaskTurn) error {
	t.CreatedAt = time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO task_turns (id, task_id, user_message, agent_message, pane, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		t.ID, t.TaskID, t.UserMessage, t.AgentMessage, t.Pane, t.CreatedAt,
	)
	if isForeignKeyConstraintErr(err) {
		return fmt.Errorf("%w: task %q does not exist", registry.ErrConflict, t.TaskID)
	}
	if err != nil {
		return fmt.Errorf("sqlite: create task turn: %w", err)
	}
	return nil
}

func (s *Store) ListTaskTurns(ctx context.Context, taskID string) ([]*registry.TaskTurn, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, task_id, user_message, agent_message, pane, created_at FROM task_turns
		WHERE task_id = ? ORDER BY created_at, rowid`, taskID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list task turns: %w", err)
	}
	defer rows.Close()
	out := []*registry.TaskTurn{}
	for rows.Next() {
		var t registry.TaskTurn
		if err := rows.Scan(&t.ID, &t.TaskID, &t.UserMessage, &t.AgentMessage, &t.Pane, &t.CreatedAt); err != nil {
			return nil, fmt.Errorf("sqlite: list task turns: %w", err)
		}
		out = append(out, &t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: list task turns: %w", err)
	}
	return out, nil
}

func (s *Store) ListTaskTurnsPage(ctx context.Context, taskID, beforeID string, limit int) ([]*registry.TaskTurn, bool, error) {
	query := `SELECT id, task_id, user_message, agent_message, pane, created_at FROM task_turns WHERE task_id = ?`
	args := []any{taskID}
	if beforeID != "" {
		var at time.Time
		var rowid int64
		err := s.db.QueryRowContext(ctx, `SELECT created_at, rowid FROM task_turns WHERE id = ? AND task_id = ?`,
			beforeID, taskID).Scan(&at, &rowid)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, fmt.Errorf("%w: turn %q of task %q", registry.ErrNotFound, beforeID, taskID)
		}
		if err != nil {
			return nil, false, fmt.Errorf("sqlite: list task turns page: %w", err)
		}
		query += ` AND (created_at, rowid) < (?, ?)`
		args = append(args, at, rowid)
	}
	// One more than asked says whether there are more.
	query += ` ORDER BY created_at DESC, rowid DESC LIMIT ?`
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, fmt.Errorf("sqlite: list task turns page: %w", err)
	}
	defer rows.Close()
	out := []*registry.TaskTurn{}
	for rows.Next() {
		var t registry.TaskTurn
		if err := rows.Scan(&t.ID, &t.TaskID, &t.UserMessage, &t.AgentMessage, &t.Pane, &t.CreatedAt); err != nil {
			return nil, false, fmt.Errorf("sqlite: list task turns page: %w", err)
		}
		out = append(out, &t)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("sqlite: list task turns page: %w", err)
	}
	more := len(out) > limit
	if more {
		out = out[:limit]
	}
	slices.Reverse(out)
	return out, more, nil
}

func (s *Store) DeleteTaskTurnsBefore(ctx context.Context, cutoff time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM task_turns WHERE created_at < ?`, cutoff.UTC())
	if err != nil {
		return 0, fmt.Errorf("sqlite: delete task turns: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("sqlite: delete task turns: %w", err)
	}
	return int(n), nil
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
	var kind, status, errorClass, attention string
	if err := row.Scan(
		&t.ID, &t.WorkspaceID, &kind, &t.AgentType, &t.TmuxSession, &status, &t.ConversationID,
		&t.CreatedAt, &t.UpdatedAt, &t.StartedAt, &t.CompletedAt, &t.ReapedAt, &t.Command, &t.ExitCode,
		&t.FailureReason, &errorClass, &t.OutputTail, &attention,
	); err != nil {
		return nil, err
	}
	if attention != "" {
		t.Attention = new(registry.Attention)
		if err := json.Unmarshal([]byte(attention), t.Attention); err != nil {
			return nil, fmt.Errorf("task %q attention: %w", t.ID, err)
		}
	}
	t.ErrorClass = registry.ErrorClass(errorClass)
	t.Kind = registry.TaskKind(kind)
	t.Status = registry.TaskStatus(status)
	return &t, nil
}

// attentionJSON is how a task's Attention is stored: JSON, or empty for
// none.
func attentionJSON(a *registry.Attention) string {
	if a == nil {
		return ""
	}
	b, err := json.Marshal(a)
	if err != nil {
		// Only plain strings and ints: Marshal can't fail.
		panic(err)
	}
	return string(b)
}

func (s *Store) CreateMessage(ctx context.Context, m *registry.Message) error {
	m.CreatedAt = time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO messages (id, conversation_id, task_id, dispatch_id, origin, origin_target_id, role, content, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.ConversationID, nullIfEmpty(m.TaskID), nullIfEmpty(m.DispatchID), m.Origin, m.OriginTargetID, string(m.Role),
		m.Content, m.CreatedAt,
	)
	if isForeignKeyConstraintErr(err) {
		return fmt.Errorf("%w: task %q or dispatch %q does not exist", registry.ErrConflict, m.TaskID, m.DispatchID)
	}
	if err != nil {
		return fmt.Errorf("sqlite: create message: %w", err)
	}
	return nil
}

const messageColumns = `id, conversation_id, task_id, dispatch_id, origin, origin_target_id, role, content, created_at`

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

// ListConversationActivity takes each conversation's most recently
// inserted message by rowid rather than MAX(created_at): created_at is
// stored as driver-formatted text, where a lexical MAX can misorder
// timestamps with differently trimmed fractional seconds. Messages are
// append-only and stamped with the current time on insert, so the
// highest rowid is the latest message.
func (s *Store) ListConversationActivity(ctx context.Context) ([]*registry.ConversationActivity, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT conversation_id, created_at FROM messages
		WHERE rowid IN (SELECT MAX(rowid) FROM messages GROUP BY conversation_id)`)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list conversation activity: %w", err)
	}
	defer rows.Close()

	out := make([]*registry.ConversationActivity, 0)
	for rows.Next() {
		var a registry.ConversationActivity
		if err := rows.Scan(&a.ConversationID, &a.LastMessageAt); err != nil {
			return nil, fmt.Errorf("sqlite: list conversation activity: %w", err)
		}
		out = append(out, &a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: list conversation activity: %w", err)
	}
	return out, nil
}

func scanMessage(row rowScanner) (*registry.Message, error) {
	var m registry.Message
	var taskID, dispatchID sql.NullString
	var role string
	if err := row.Scan(&m.ID, &m.ConversationID, &taskID, &dispatchID, &m.Origin, &m.OriginTargetID, &role, &m.Content, &m.CreatedAt); err != nil {
		return nil, err
	}
	m.TaskID = taskID.String
	m.DispatchID = dispatchID.String
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

func (s *Store) ListCredentialInfo(ctx context.Context) ([]*registry.Credential, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, workspace_id, agent_type, created_at, updated_at FROM credentials ORDER BY name, id`)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list credential info: %w", err)
	}
	defer rows.Close()
	out := []*registry.Credential{}
	for rows.Next() {
		var c registry.Credential
		var workspaceID sql.NullString
		if err := rows.Scan(&c.ID, &c.Name, &workspaceID, &c.AgentType, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, fmt.Errorf("sqlite: list credential info: %w", err)
		}
		c.WorkspaceID = workspaceID.String
		out = append(out, &c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: list credential info: %w", err)
	}
	return out, nil
}

func (s *Store) SetCredentialValue(ctx context.Context, id, value string) error {
	if s.masterKey == nil {
		return fmt.Errorf("sqlite: no master key configured (see WithMasterKey); cannot set credential")
	}
	ciphertext, err := encrypt(s.masterKey, value)
	if err != nil {
		return fmt.Errorf("sqlite: encrypt credential: %w", err)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE credentials SET ciphertext = ?, updated_at = ? WHERE id = ?`,
		ciphertext, time.Now().UTC(), id)
	if err != nil {
		return fmt.Errorf("sqlite: set credential value: %w", err)
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

// targetColumns is what scanTarget reads, in order.
const targetColumns = `id, name, kind, host, user, ssh_key_ref, workspace_root, permission_mode,
	purpose, allowed_agent_types, no_provision, no_shell, require_confirmation, relay, ssh_port, host_keys, created_at,
	updated_at`

func scanTarget(row rowScanner) (*registry.Target, error) {
	var t registry.Target
	var kind, allowed string
	if err := row.Scan(&t.ID, &t.Name, &kind, &t.Host, &t.User, &t.SSHKeyRef, &t.WorkspaceRoot, &t.PermissionMode,
		&t.Policy.Purpose, &allowed, &t.Policy.NoProvision, &t.Policy.NoShell, &t.Policy.RequireConfirmation,
		&t.Policy.Relay, &t.SSHPort, &t.HostKeys, &t.CreatedAt, &t.UpdatedAt); err != nil {
		return nil, err
	}
	t.Kind = registry.TargetKind(kind)
	if err := json.Unmarshal([]byte(allowed), &t.Policy.AllowedAgentTypes); err != nil {
		return nil, fmt.Errorf("unmarshal allowed agent types: %w", err)
	}
	if len(t.Policy.AllowedAgentTypes) == 0 {
		t.Policy.AllowedAgentTypes = nil
	}
	return &t, nil
}

// marshalAllowedAgents stores a policy's agent-type list (LOOM-89).
func marshalAllowedAgents(agents []string) (string, error) {
	if agents == nil {
		agents = []string{}
	}
	b, err := json.Marshal(agents)
	if err != nil {
		return "", fmt.Errorf("sqlite: marshal allowed agent types: %w", err)
	}
	return string(b), nil
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
