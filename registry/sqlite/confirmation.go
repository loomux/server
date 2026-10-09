package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/Loomux/server/registry"
)

const confirmationColumns = `id, conversation_id, dispatch_id, kind, target_id, target_name, agent_type, command,
	workspace, git_remote, status, created_at, expires_at, resolved_at`

func (s *Store) CreateConfirmation(ctx context.Context, c *registry.Confirmation) error {
	if c.CreatedAt.IsZero() {
		c.CreatedAt = time.Now().UTC()
	}
	if c.Status == "" {
		c.Status = registry.ConfirmationPending
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO confirmations (`+confirmationColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.ConversationID, nullIfEmpty(c.DispatchID), c.Kind, c.TargetID, c.TargetName, c.AgentType, c.Command,
		c.Workspace, c.GitRemote, string(c.Status), c.CreatedAt, c.ExpiresAt, c.ResolvedAt)
	if isUniqueConstraintErr(err) {
		return fmt.Errorf("%w: confirmation %q exists", registry.ErrConflict, c.ID)
	}
	if err != nil {
		return fmt.Errorf("sqlite: create confirmation: %w", err)
	}
	return nil
}

func (s *Store) ResolveConfirmation(ctx context.Context, id string, status registry.ConfirmationStatus) error {
	res, err := s.db.ExecContext(ctx, `UPDATE confirmations SET status = ?, resolved_at = ?
		WHERE id = ? AND status = 'pending'`, string(status), time.Now().UTC(), id)
	if err != nil {
		return fmt.Errorf("sqlite: resolve confirmation: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return nil
	}
	var cur string
	err = s.db.QueryRowContext(ctx, `SELECT status FROM confirmations WHERE id = ?`, id).Scan(&cur)
	if err == sql.ErrNoRows {
		return fmt.Errorf("%w: confirmation %q", registry.ErrNotFound, id)
	}
	if err != nil {
		return fmt.Errorf("sqlite: resolve confirmation: %w", err)
	}
	return fmt.Errorf("%w: confirmation %q is %s", registry.ErrConflict, id, cur)
}

func (s *Store) ListConfirmationsByConversation(ctx context.Context, conversationID string) ([]*registry.Confirmation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+confirmationColumns+` FROM confirmations
		WHERE conversation_id = ? ORDER BY created_at, rowid`, conversationID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list confirmations: %w", err)
	}
	defer rows.Close()
	out := make([]*registry.Confirmation, 0)
	for rows.Next() {
		var c registry.Confirmation
		var dispatchID sql.NullString
		var status string
		if err := rows.Scan(&c.ID, &c.ConversationID, &dispatchID, &c.Kind, &c.TargetID, &c.TargetName, &c.AgentType,
			&c.Command, &c.Workspace, &c.GitRemote, &status, &c.CreatedAt, &c.ExpiresAt, &c.ResolvedAt); err != nil {
			return nil, fmt.Errorf("sqlite: list confirmations: %w", err)
		}
		c.DispatchID = dispatchID.String
		c.Status = registry.ConfirmationStatus(status)
		out = append(out, &c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: list confirmations: %w", err)
	}
	return out, nil
}

func (s *Store) ExpirePendingConfirmations(ctx context.Context) (int, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE confirmations SET status = 'expired', resolved_at = ?
		WHERE status = 'pending'`, time.Now().UTC())
	if err != nil {
		return 0, fmt.Errorf("sqlite: expire confirmations: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func (s *Store) DeleteConfirmationsResolvedBefore(ctx context.Context, cutoff time.Time) (int, error) {
	n, err := s.deleteInBatches(ctx, "confirmations",
		`status != 'pending' AND COALESCE(resolved_at, expires_at) < ?`, cutoff.UTC())
	if err != nil {
		return n, fmt.Errorf("sqlite: delete confirmations: %w", err)
	}
	return n, nil
}
