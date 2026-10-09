package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Loomux/server/registry"
)

// Environments (LOOM-178): the machine's host private key is sealed
// under the master key with "environment:<id>" as additional data.

func environmentAAD(id string) []byte { return []byte("environment:" + id) }

const environmentColumns = `id, target_id, plugin_id, plugin_name, plugin_version, status, status_reason,
	size, persistent, egress, image, image_digest, host_key, host_private_key, created_at, updated_at`

func (s *Store) CreateEnvironment(ctx context.Context, e *registry.Environment) error {
	if s.masterKey == nil {
		return fmt.Errorf("sqlite: create environment: %w", registry.ErrNoMasterKey)
	}
	sealed, err := seal(s.masterKey, e.HostPrivateKey, environmentAAD(e.ID))
	if err != nil {
		return fmt.Errorf("sqlite: encrypt host key: %w", err)
	}
	now := time.Now().UTC()
	e.CreatedAt, e.UpdatedAt = now, now
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO environments (`+environmentColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.TargetID, nullIfEmpty(e.PluginID), e.PluginName, e.PluginVersion, string(e.Status), e.StatusReason,
		e.Size, e.Persistent, e.Egress, e.Image, e.ImageDigest, e.HostKey, sealed, e.CreatedAt, e.UpdatedAt)
	if isUniqueConstraintErr(err) {
		return fmt.Errorf("%w: target %q already has an environment", registry.ErrConflict, e.TargetID)
	}
	if isForeignKeyConstraintErr(err) {
		return fmt.Errorf("%w: environment names a target or plugin that doesn't exist", registry.ErrConflict)
	}
	if err != nil {
		return fmt.Errorf("sqlite: create environment: %w", err)
	}
	return nil
}

func scanEnvironment(row interface{ Scan(...any) error }) (*registry.Environment, []byte, error) {
	var e registry.Environment
	var pluginID sql.NullString
	var status string
	var sealed []byte
	if err := row.Scan(&e.ID, &e.TargetID, &pluginID, &e.PluginName, &e.PluginVersion, &status, &e.StatusReason,
		&e.Size, &e.Persistent, &e.Egress, &e.Image, &e.ImageDigest, &e.HostKey, &sealed, &e.CreatedAt, &e.UpdatedAt); err != nil {
		return nil, nil, err
	}
	e.PluginID = pluginID.String
	e.Status = registry.EnvironmentStatus(status)
	return &e, sealed, nil
}

func (s *Store) getEnvironment(ctx context.Context, where string, arg any) (*registry.Environment, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+environmentColumns+` FROM environments WHERE `+where, arg)
	e, sealed, err := scanEnvironment(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: environment for %v", registry.ErrNotFound, arg)
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: get environment: %w", err)
	}
	if s.masterKey == nil {
		return e, fmt.Errorf("sqlite: environment %q host key: %w", e.ID, registry.ErrNoMasterKey)
	}
	key, err := open(s.masterKey, sealed, environmentAAD(e.ID))
	if err != nil {
		return e, fmt.Errorf("sqlite: environment %q host key: %w", e.ID, err)
	}
	e.HostPrivateKey = key
	return e, nil
}

func (s *Store) GetEnvironment(ctx context.Context, id string) (*registry.Environment, error) {
	return s.getEnvironment(ctx, "id = ?", id)
}

func (s *Store) GetEnvironmentByTarget(ctx context.Context, targetID string) (*registry.Environment, error) {
	return s.getEnvironment(ctx, "target_id = ?", targetID)
}

func (s *Store) listEnvironments(ctx context.Context, where string, args ...any) ([]*registry.Environment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+environmentColumns+` FROM environments `+where+` ORDER BY created_at, id`, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list environments: %w", err)
	}
	defer rows.Close()
	out := []*registry.Environment{}
	for rows.Next() {
		e, _, err := scanEnvironment(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite: list environments: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: list environments: %w", err)
	}
	return out, nil
}

func (s *Store) ListEnvironments(ctx context.Context) ([]*registry.Environment, error) {
	return s.listEnvironments(ctx, "")
}

func (s *Store) ListEnvironmentsByPlugin(ctx context.Context, pluginID string) ([]*registry.Environment, error) {
	return s.listEnvironments(ctx, "WHERE plugin_id = ?", pluginID)
}

func (s *Store) UpdateEnvironment(ctx context.Context, e *registry.Environment) error {
	e.UpdatedAt = time.Now().UTC()
	res, err := s.db.ExecContext(ctx, `
		UPDATE environments SET plugin_id = ?, plugin_version = ?, status = ?, status_reason = ?, size = ?,
			persistent = ?, egress = ?, image = ?, image_digest = ?, updated_at = ?
		WHERE id = ?`,
		nullIfEmpty(e.PluginID), e.PluginVersion, string(e.Status), e.StatusReason, e.Size,
		e.Persistent, e.Egress, e.Image, e.ImageDigest, e.UpdatedAt, e.ID)
	if isForeignKeyConstraintErr(err) {
		return fmt.Errorf("%w: environment names a plugin that doesn't exist", registry.ErrConflict)
	}
	if err != nil {
		return fmt.Errorf("sqlite: update environment: %w", err)
	}
	return requireRowAffected(res, "environment", e.ID)
}

func (s *Store) DeleteEnvironment(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM environments WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("sqlite: delete environment: %w", err)
	}
	return requireRowAffected(res, "environment", id)
}
