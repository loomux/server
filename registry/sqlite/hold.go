package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Loomux/server/registry"
)

const holdColumns = `id, task_id, session_id, client_kind, client_label, mode, prev_status,
	acquired_at, renewed_at, ttl_ms, expires_at, ended_at, ended_reason`

// querier is what a read or write runs on: the pool, or a transaction.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func (s *Store) CreateTaskHold(ctx context.Context, h *registry.TaskHold) error {
	if err := prepareHold(h); err != nil {
		return err
	}
	return insertHold(ctx, s.db, h)
}

// AcquireDriveHold runs in one transaction, which SQLite begins
// IMMEDIATE (see dsn): the status check, the end of a replaced hold, the
// insert and the status write can't be interleaved with another writer.
func (s *Store) AcquireDriveHold(ctx context.Context, h *registry.TaskHold, t *registry.Task, from registry.TaskStatus,
	replace registry.HoldEndReason) (*registry.TaskHold, bool, error) {
	if h.Mode != registry.HoldModeDrive {
		return nil, false, fmt.Errorf("sqlite: acquire drive hold: mode %q is not drive", h.Mode)
	}
	if h.TaskID != t.ID {
		return nil, false, fmt.Errorf("sqlite: acquire drive hold: hold is for task %q, not %q", h.TaskID, t.ID)
	}
	if replace != "" && !replace.Valid() {
		return nil, false, fmt.Errorf("sqlite: acquire drive hold: unknown end reason %q", replace)
	}
	if err := prepareHold(h); err != nil {
		return nil, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, fmt.Errorf("sqlite: acquire drive hold: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var status string
	err = tx.QueryRowContext(ctx, `SELECT status FROM tasks WHERE id = ?`, t.ID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, fmt.Errorf("%w: task %q", registry.ErrNotFound, t.ID)
	}
	if err != nil {
		return nil, false, fmt.Errorf("sqlite: acquire drive hold: %w", err)
	}
	if registry.TaskStatus(status) != from {
		return nil, false, nil
	}

	ended, err := activeDriveHold(ctx, tx, t.ID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		ended = nil
	case err != nil:
		return nil, false, fmt.Errorf("sqlite: acquire drive hold: %w", err)
	case replace == "":
		return nil, false, fmt.Errorf("%w: task %q", registry.ErrTaskHeld, t.ID)
	default:
		if _, err := endHold(ctx, tx, ended, replace, h.AcquiredAt); err != nil {
			return nil, false, err
		}
	}
	if err := insertHold(ctx, tx, h); err != nil {
		return nil, false, err
	}
	if t.Status != from {
		// Only the status: the rest of t was read before this
		// transaction, and may be older than the row.
		updatedAt := time.Now().UTC()
		if _, err := tx.ExecContext(ctx, `UPDATE tasks SET status = ?, updated_at = ? WHERE id = ? AND status = ?`,
			string(t.Status), updatedAt, t.ID, string(from)); err != nil {
			return nil, false, fmt.Errorf("sqlite: acquire drive hold: %w", err)
		}
		t.UpdatedAt = updatedAt
	}
	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("sqlite: acquire drive hold: %w", err)
	}
	return ended, true, nil
}

// prepareHold checks h and stamps what CreateTaskHold stamps.
func prepareHold(h *registry.TaskHold) error {
	if !h.Mode.Valid() {
		return fmt.Errorf("sqlite: create task hold: unknown mode %q", h.Mode)
	}
	if h.TTL.Milliseconds() <= 0 {
		return fmt.Errorf("sqlite: create task hold: ttl %s is not at least a millisecond", h.TTL)
	}
	if h.AcquiredAt.IsZero() {
		h.AcquiredAt = time.Now()
	}
	h.AcquiredAt = h.AcquiredAt.UTC()
	h.RenewedAt = h.AcquiredAt
	h.ExpiresAt = h.AcquiredAt.Add(h.TTL)
	h.EndedAt, h.EndedReason = nil, ""
	return nil
}

func insertHold(ctx context.Context, q querier, h *registry.TaskHold) error {
	_, err := q.ExecContext(ctx, `INSERT INTO task_holds (`+holdColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, '')`,
		h.ID, h.TaskID, h.SessionID, h.ClientKind, h.ClientLabel, string(h.Mode), string(h.PrevStatus),
		h.AcquiredAt, h.RenewedAt, h.TTL.Milliseconds(), h.ExpiresAt)
	switch {
	case isUniqueConstraintErr(err) && strings.Contains(err.Error(), "task_holds.task_id"):
		// The partial unique index: one active drive hold per task.
		return fmt.Errorf("%w: task %q", registry.ErrTaskHeld, h.TaskID)
	case isUniqueConstraintErr(err):
		return fmt.Errorf("%w: hold %q exists", registry.ErrConflict, h.ID)
	case isForeignKeyConstraintErr(err):
		return fmt.Errorf("%w: task %q does not exist", registry.ErrConflict, h.TaskID)
	case err != nil:
		return fmt.Errorf("sqlite: create task hold: %w", err)
	}
	return nil
}

func (s *Store) GetTaskHold(ctx context.Context, id string) (*registry.TaskHold, error) {
	return getHold(ctx, s.db, id)
}

func getHold(ctx context.Context, q querier, id string) (*registry.TaskHold, error) {
	h, err := scanHold(q.QueryRowContext(ctx, `SELECT `+holdColumns+` FROM task_holds WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: hold %q", registry.ErrNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: get task hold: %w", err)
	}
	return h, nil
}

// activeDriveHold is taskID's active drive hold, or sql.ErrNoRows.
func activeDriveHold(ctx context.Context, q querier, taskID string) (*registry.TaskHold, error) {
	return scanHold(q.QueryRowContext(ctx, `SELECT `+holdColumns+` FROM task_holds
		WHERE task_id = ? AND mode = 'drive' AND ended_at IS NULL`, taskID))
}

func (s *Store) GetActiveDriveHold(ctx context.Context, taskID string) (*registry.TaskHold, error) {
	h, err := activeDriveHold(ctx, s.db, taskID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: no active drive hold on task %q", registry.ErrNotFound, taskID)
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: get active drive hold: %w", err)
	}
	return h, nil
}

func (s *Store) ListActiveTaskHolds(ctx context.Context) ([]*registry.TaskHold, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+holdColumns+` FROM task_holds
		WHERE ended_at IS NULL ORDER BY acquired_at, rowid`)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list active task holds: %w", err)
	}
	defer rows.Close()
	out := make([]*registry.TaskHold, 0)
	for rows.Next() {
		h, err := scanHold(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite: list active task holds: %w", err)
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: list active task holds: %w", err)
	}
	return out, nil
}

// RenewTaskHold reads the hold's TTL and writes the renewal in one
// transaction; the UPDATE is guarded by ended_at, so a renewal can't
// bring back a hold that an expiry or a forced takeover just ended.
func (s *Store) RenewTaskHold(ctx context.Context, id string, at time.Time) (*registry.TaskHold, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("sqlite: renew task hold: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	h, err := getHold(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if !h.Active() {
		return h, fmt.Errorf("%w: hold %q (%s)", registry.ErrHoldEnded, id, h.EndedReason)
	}
	h.RenewedAt = at.UTC()
	// Never backwards: two renewals may commit out of order, and the
	// restart grace may already have moved the expiry further out.
	if expires := h.RenewedAt.Add(h.TTL); expires.After(h.ExpiresAt) {
		h.ExpiresAt = expires
	}
	if _, err := tx.ExecContext(ctx, `UPDATE task_holds SET renewed_at = ?, expires_at = ?
		WHERE id = ? AND ended_at IS NULL`, h.RenewedAt, h.ExpiresAt, id); err != nil {
		return nil, fmt.Errorf("sqlite: renew task hold: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("sqlite: renew task hold: %w", err)
	}
	return h, nil
}

// EndTaskHold is one UPDATE guarded by ended_at: of several enders, one
// ends the hold and its reason is the one kept.
func (s *Store) EndTaskHold(ctx context.Context, id string, reason registry.HoldEndReason, at time.Time) (*registry.TaskHold, bool, error) {
	if !reason.Valid() {
		return nil, false, fmt.Errorf("sqlite: end task hold: unknown end reason %q", reason)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE task_holds SET ended_at = ?, ended_reason = ?
		WHERE id = ? AND ended_at IS NULL`, at.UTC(), string(reason), id)
	if err != nil {
		return nil, false, fmt.Errorf("sqlite: end task hold: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, false, fmt.Errorf("sqlite: end task hold: %w", err)
	}
	h, err := getHold(ctx, s.db, id)
	if err != nil {
		return nil, false, err
	}
	return h, n == 1, nil
}

// endHold ends h, an active hold read on q, and updates h to match; it
// reports whether its UPDATE was the one that ended it.
func endHold(ctx context.Context, q querier, h *registry.TaskHold, reason registry.HoldEndReason, at time.Time) (bool, error) {
	at = at.UTC()
	res, err := q.ExecContext(ctx, `UPDATE task_holds SET ended_at = ?, ended_reason = ?
		WHERE id = ? AND ended_at IS NULL`, at, string(reason), h.ID)
	if err != nil {
		return false, fmt.Errorf("sqlite: end task hold: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("sqlite: end task hold: %w", err)
	}
	if n == 1 {
		h.EndedAt, h.EndedReason = &at, reason
	}
	return n == 1, nil
}

// ReleaseTaskHold runs in one transaction (IMMEDIATE, see dsn), so no
// takeover can land between the hold's end and the task's release: a task
// is never handed back to automation while a drive hold holds it.
func (s *Store) ReleaseTaskHold(ctx context.Context, taskID, holdID string, reason registry.HoldEndReason, at time.Time) (*registry.HoldRelease, error) {
	if !reason.Valid() {
		return nil, fmt.Errorf("sqlite: release task hold: unknown end reason %q", reason)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("sqlite: release task hold: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	task, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id = ?`, taskID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: task %q", registry.ErrNotFound, taskID)
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: release task hold: %w", err)
	}
	out := &registry.HoldRelease{Task: task}

	if holdID != "" {
		h, err := getHold(ctx, tx, holdID)
		if err != nil {
			return nil, err
		}
		if h.TaskID != taskID {
			return nil, fmt.Errorf("%w: hold %q on task %q", registry.ErrNotFound, holdID, taskID)
		}
		out.Hold = h
	} else {
		h, err := activeDriveHold(ctx, tx, taskID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("sqlite: release task hold: %w", err)
		}
		if err == nil {
			out.Hold = h
		}
	}
	if out.Hold != nil && out.Hold.Active() {
		if out.Ended, err = endHold(ctx, tx, out.Hold, reason, at); err != nil {
			return nil, err
		}
	}

	// A watch hold says nothing about who drives: its end leaves the task.
	if (out.Hold == nil || out.Hold.Mode == registry.HoldModeDrive) && task.Status == registry.TaskStatusHumanTakeover {
		_, err := activeDriveHold(ctx, tx, taskID)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			task.Status, task.Attention = registry.TaskStatusAwaitingInput, nil
			if out.TaskReleased, err = transitionTask(ctx, tx, task, registry.TaskStatusHumanTakeover); err != nil {
				return nil, err
			}
		case err != nil:
			return nil, fmt.Errorf("sqlite: release task hold: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("sqlite: release task hold: %w", err)
	}
	return out, nil
}

func (s *Store) ExtendActiveTaskHolds(ctx context.Context, from time.Time) (int, error) {
	from = from.UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("sqlite: extend task holds: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `SELECT id, ttl_ms, expires_at FROM task_holds WHERE ended_at IS NULL`)
	if err != nil {
		return 0, fmt.Errorf("sqlite: extend task holds: %w", err)
	}
	type extension struct {
		id    string
		until time.Time
	}
	var todo []extension
	for rows.Next() {
		var id string
		var ttlMS int64
		var expires time.Time
		if err := rows.Scan(&id, &ttlMS, &expires); err != nil {
			rows.Close()
			return 0, fmt.Errorf("sqlite: extend task holds: %w", err)
		}
		if until := from.Add(time.Duration(ttlMS) * time.Millisecond); expires.Before(until) {
			todo = append(todo, extension{id, until})
		}
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("sqlite: extend task holds: %w", err)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("sqlite: extend task holds: %w", err)
	}
	for _, e := range todo {
		if _, err := tx.ExecContext(ctx, `UPDATE task_holds SET expires_at = ? WHERE id = ? AND ended_at IS NULL`, e.until, e.id); err != nil {
			return 0, fmt.Errorf("sqlite: extend task holds: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("sqlite: extend task holds: %w", err)
	}
	return len(todo), nil
}

func (s *Store) DeleteTaskHoldsEndedBefore(ctx context.Context, cutoff time.Time) (int, error) {
	n, err := s.deleteInBatches(ctx, "task_holds", `ended_at IS NOT NULL AND ended_at < ?`, cutoff.UTC())
	if err != nil {
		return n, fmt.Errorf("sqlite: delete task holds: %w", err)
	}
	return n, nil
}

func scanHold(row rowScanner) (*registry.TaskHold, error) {
	var h registry.TaskHold
	var mode, prev, reason string
	var ttlMS int64
	if err := row.Scan(&h.ID, &h.TaskID, &h.SessionID, &h.ClientKind, &h.ClientLabel, &mode, &prev,
		&h.AcquiredAt, &h.RenewedAt, &ttlMS, &h.ExpiresAt, &h.EndedAt, &reason); err != nil {
		return nil, err
	}
	h.Mode = registry.HoldMode(mode)
	h.PrevStatus = registry.TaskStatus(prev)
	h.TTL = time.Duration(ttlMS) * time.Millisecond
	h.EndedReason = registry.HoldEndReason(reason)
	return &h, nil
}
