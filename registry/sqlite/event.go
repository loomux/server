package sqlite

import (
	"context"
	"fmt"
	"time"

	"github.com/Loomux/server/registry"
)

const eventColumns = `id, conversation_id, dispatch_id, created_at, kind, model, tier, target_id, workspace_id,
	task_id, command, outcome, error_class, duration_ms, detail`

func (s *Store) CreateDispatchEvent(ctx context.Context, e *registry.DispatchEvent) error {
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO dispatch_events (`+eventColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.ConversationID, e.DispatchID, e.CreatedAt.UTC(), e.Kind, e.Model, e.Tier, e.TargetID, e.WorkspaceID,
		e.TaskID, e.Command, e.Outcome, string(e.ErrorClass), e.Duration.Milliseconds(), e.Detail)
	if isUniqueConstraintErr(err) {
		return fmt.Errorf("%w: dispatch event %q exists", registry.ErrConflict, e.ID)
	}
	if err != nil {
		return fmt.Errorf("sqlite: create dispatch event: %w", err)
	}
	return nil
}

func (s *Store) ListDispatchEventsByConversation(ctx context.Context, conversationID string) ([]*registry.DispatchEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+eventColumns+` FROM dispatch_events
		WHERE conversation_id = ? ORDER BY created_at, rowid`, conversationID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list dispatch events: %w", err)
	}
	defer rows.Close()
	out := make([]*registry.DispatchEvent, 0)
	for rows.Next() {
		var e registry.DispatchEvent
		var errorClass string
		var durationMS int64
		if err := rows.Scan(&e.ID, &e.ConversationID, &e.DispatchID, &e.CreatedAt, &e.Kind, &e.Model, &e.Tier,
			&e.TargetID, &e.WorkspaceID, &e.TaskID, &e.Command, &e.Outcome, &errorClass, &durationMS, &e.Detail); err != nil {
			return nil, fmt.Errorf("sqlite: list dispatch events: %w", err)
		}
		e.ErrorClass = registry.ErrorClass(errorClass)
		e.Duration = time.Duration(durationMS) * time.Millisecond
		out = append(out, &e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: list dispatch events: %w", err)
	}
	return out, nil
}

func (s *Store) DeleteDispatchEventsBefore(ctx context.Context, cutoff time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM dispatch_events WHERE created_at < ?`, cutoff.UTC())
	if err != nil {
		return 0, fmt.Errorf("sqlite: delete dispatch events: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("sqlite: delete dispatch events: %w", err)
	}
	return int(n), nil
}
