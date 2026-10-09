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

const dispatchColumns = `id, conversation_id, message, workspace_hint, idempotency_key, request_hash, status,
	reply, error, error_class, created_at, updated_at, started_at, finished_at, confirmation_id`

// CreateDispatch inserts d and, if given, its user message in one
// transaction. The two partial unique indexes (idempotency key, one
// active dispatch per conversation) are what decide a race between two
// submits, so the error is mapped from whichever of them fired.
func (s *Store) CreateDispatch(ctx context.Context, d *registry.Dispatch, userMessage *registry.Message) error {
	now := time.Now().UTC()
	d.CreatedAt = now
	d.UpdatedAt = now
	if d.Status == "" {
		d.Status = registry.DispatchStatusQueued
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: create dispatch: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	_, err = tx.ExecContext(ctx, `INSERT INTO dispatches (`+dispatchColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.ID, d.ConversationID, d.Message, d.WorkspaceHint, nullIfEmpty(d.IdempotencyKey), d.RequestHash, string(d.Status),
		d.Reply, d.Error, string(d.ErrorClass), d.CreatedAt, d.UpdatedAt, d.StartedAt, d.FinishedAt, d.ConfirmationID,
	)
	switch {
	case isUniqueConstraintErr(err) && strings.Contains(err.Error(), "dispatches.idempotency_key"):
		return fmt.Errorf("%w: %q", registry.ErrIdempotencyKeyExists, d.IdempotencyKey)
	case isUniqueConstraintErr(err) && strings.Contains(err.Error(), "dispatches.conversation_id"):
		return fmt.Errorf("%w: %q", registry.ErrConversationBusy, d.ConversationID)
	case isUniqueConstraintErr(err):
		return fmt.Errorf("%w: dispatch %q already exists", registry.ErrConflict, d.ID)
	case err != nil:
		return fmt.Errorf("sqlite: create dispatch: %w", err)
	}

	if userMessage != nil {
		userMessage.DispatchID = d.ID
		userMessage.CreatedAt = now
		_, err = tx.ExecContext(ctx, `
			INSERT INTO messages (id, conversation_id, task_id, dispatch_id, role, content, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			userMessage.ID, userMessage.ConversationID, nullIfEmpty(userMessage.TaskID), d.ID,
			string(userMessage.Role), userMessage.Content, userMessage.CreatedAt,
		)
		if err != nil {
			return fmt.Errorf("sqlite: create dispatch: user message: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: create dispatch: %w", err)
	}
	return nil
}

func (s *Store) GetDispatch(ctx context.Context, id string) (*registry.Dispatch, error) {
	return s.getDispatchWhere(ctx, "id = ?", id)
}

func (s *Store) GetDispatchByIdempotencyKey(ctx context.Context, key string) (*registry.Dispatch, error) {
	return s.getDispatchWhere(ctx, "idempotency_key = ?", key)
}

func (s *Store) getDispatchWhere(ctx context.Context, where string, arg string) (*registry.Dispatch, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+dispatchColumns+` FROM dispatches WHERE `+where, arg)
	d, err := scanDispatch(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: dispatch %q", registry.ErrNotFound, arg)
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: get dispatch: %w", err)
	}
	return d, nil
}

// ListDispatchesByConversation orders by rowid, i.e. insertion order —
// the same reasoning as ListMessagesByConversation's tiebreak.
func (s *Store) ListDispatchesByConversation(ctx context.Context, conversationID string) ([]*registry.Dispatch, error) {
	return s.listDispatches(ctx, `SELECT `+dispatchColumns+` FROM dispatches WHERE conversation_id = ? ORDER BY rowid`, conversationID)
}

// ListDispatchesAfter, like ListMessagesAfter, uses the rowid as the
// creation cursor; the dispatches in also are reread whatever their age.
func (s *Store) ListDispatchesAfter(ctx context.Context, conversationID, afterID string, also []string) ([]*registry.Dispatch, error) {
	query := `SELECT ` + dispatchColumns + ` FROM dispatches WHERE conversation_id = ?
		AND (rowid > COALESCE((SELECT rowid FROM dispatches WHERE id = ? AND conversation_id = ?), 0)`
	args := []any{conversationID, afterID, conversationID}
	if len(also) > 0 {
		query += ` OR id IN (` + strings.TrimSuffix(strings.Repeat("?, ", len(also)), ", ") + `)`
		for _, id := range also {
			args = append(args, id)
		}
	}
	return s.listDispatches(ctx, query+`) ORDER BY rowid`, args...)
}

func (s *Store) ListDispatchesByStatus(ctx context.Context, statuses ...registry.DispatchStatus) ([]*registry.Dispatch, error) {
	if len(statuses) == 0 {
		return []*registry.Dispatch{}, nil
	}
	args := make([]any, len(statuses))
	for i, st := range statuses {
		args[i] = string(st)
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(statuses)), ", ")
	return s.listDispatches(ctx, `SELECT `+dispatchColumns+` FROM dispatches WHERE status IN (`+placeholders+`) ORDER BY rowid`, args...)
}

func (s *Store) listDispatches(ctx context.Context, query string, args ...any) ([]*registry.Dispatch, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list dispatches: %w", err)
	}
	defer rows.Close()
	out := make([]*registry.Dispatch, 0)
	for rows.Next() {
		d, err := scanDispatch(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite: list dispatches: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: list dispatches: %w", err)
	}
	return out, nil
}

func (s *Store) TransitionDispatch(ctx context.Context, d *registry.Dispatch, from registry.DispatchStatus) error {
	updatedAt := time.Now().UTC()
	res, err := s.db.ExecContext(ctx, `
		UPDATE dispatches SET status = ?, reply = ?, error = ?, error_class = ?, updated_at = ?, started_at = ?, finished_at = ?
		WHERE id = ? AND status = ?`,
		string(d.Status), d.Reply, d.Error, string(d.ErrorClass), updatedAt, d.StartedAt, d.FinishedAt, d.ID, string(from),
	)
	if err != nil {
		return fmt.Errorf("sqlite: transition dispatch: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlite: transition dispatch: %w", err)
	}
	if n == 0 {
		if _, err := s.GetDispatch(ctx, d.ID); err != nil {
			return err
		}
		return fmt.Errorf("%w: dispatch %q is no longer %s", registry.ErrDispatchStateChanged, d.ID, from)
	}
	d.UpdatedAt = updatedAt
	return nil
}

func scanDispatch(row rowScanner) (*registry.Dispatch, error) {
	var d registry.Dispatch
	var key sql.NullString
	var status, errorClass string
	if err := row.Scan(&d.ID, &d.ConversationID, &d.Message, &d.WorkspaceHint, &key, &d.RequestHash, &status,
		&d.Reply, &d.Error, &errorClass, &d.CreatedAt, &d.UpdatedAt, &d.StartedAt, &d.FinishedAt, &d.ConfirmationID); err != nil {
		return nil, err
	}
	d.IdempotencyKey = key.String
	d.Status = registry.DispatchStatus(status)
	d.ErrorClass = registry.ErrorClass(errorClass)
	return &d, nil
}

func (s *Store) DeleteDispatchesFinishedBefore(ctx context.Context, cutoff time.Time) (int, error) {
	// finished_at is set on every move to a finished status; updated_at
	// stands in for a row from before it was.
	n, err := s.deleteInBatches(ctx, "dispatches",
		`status IN ('succeeded', 'failed', 'interrupted') AND COALESCE(finished_at, updated_at) < ?`, cutoff.UTC())
	if err != nil {
		return n, fmt.Errorf("sqlite: delete dispatches: %w", err)
	}
	return n, nil
}
