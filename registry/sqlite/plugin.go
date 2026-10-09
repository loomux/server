package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Loomux/server/registry"
)

// Installed plugins and server settings (LOOM-178). A plugin's secret
// configuration fields are sealed as one JSON object under the master key
// with "plugin:<id>" as additional data, so a blob can't be moved to
// another plugin's row; the rest of the configuration is plain JSON.

func pluginAAD(id string) []byte { return []byte("plugin:" + id) }

// sealSecrets encrypts secrets, or returns nil for none.
func (s *Store) sealSecrets(id string, secrets map[string]string) ([]byte, error) {
	if len(secrets) == 0 {
		return nil, nil
	}
	if s.masterKey == nil {
		return nil, registry.ErrNoMasterKey
	}
	plain, err := json.Marshal(secrets)
	if err != nil {
		return nil, err
	}
	return seal(s.masterKey, plain, pluginAAD(id))
}

// openSecrets decrypts a secrets blob; nil for none.
func (s *Store) openSecrets(id string, sealed []byte) (map[string]string, error) {
	if len(sealed) == 0 {
		return nil, nil
	}
	if s.masterKey == nil {
		return nil, registry.ErrNoMasterKey
	}
	plain, err := open(s.masterKey, sealed, pluginAAD(id))
	if err != nil {
		return nil, err
	}
	var out map[string]string
	if err := json.Unmarshal(plain, &out); err != nil {
		return nil, fmt.Errorf("sqlite: plugin secrets: %w", err)
	}
	return out, nil
}

func marshalConfig(config map[string]any) (string, error) {
	if config == nil {
		config = map[string]any{}
	}
	b, err := json.Marshal(config)
	if err != nil {
		return "", fmt.Errorf("sqlite: plugin config: %w", err)
	}
	return string(b), nil
}

func (s *Store) CreatePlugin(ctx context.Context, p *registry.Plugin) error {
	sealed, err := s.sealSecrets(p.ID, p.Secrets)
	if err != nil {
		return fmt.Errorf("sqlite: create plugin: %w", err)
	}
	config, err := marshalConfig(p.Config)
	if err != nil {
		return err
	}
	caps, err := json.Marshal(nonNil(p.Capabilities))
	if err != nil {
		return fmt.Errorf("sqlite: create plugin: %w", err)
	}
	now := time.Now().UTC()
	p.InstalledAt = now
	p.UpdatedAt = now
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO plugins (id, name, label, version, protocol, source, path, trust, status, status_reason,
			enabled, capabilities, manifest, config, secrets, installed_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.Name, p.Label, p.Version, p.Protocol, string(p.Source), p.Path, p.Trust, string(p.Status), p.StatusReason,
		p.Enabled, string(caps), manifestJSON(p.Manifest), config, sealed, p.InstalledAt, p.UpdatedAt)
	if isUniqueConstraintErr(err) {
		return fmt.Errorf("%w: plugin label %q already exists", registry.ErrConflict, p.Label)
	}
	if err != nil {
		return fmt.Errorf("sqlite: create plugin: %w", err)
	}
	return nil
}

const pluginColumns = `id, name, label, version, protocol, source, path, trust, status, status_reason,
	enabled, capabilities, manifest, config, secrets, installed_at, updated_at`

// manifestJSON is the manifest column's value: "{}" for none.
func manifestJSON(m json.RawMessage) string {
	if len(m) == 0 {
		return "{}"
	}
	return string(m)
}

func scanPlugin(row interface{ Scan(...any) error }) (*registry.Plugin, []byte, error) {
	var p registry.Plugin
	var source, status, caps, manifest, config string
	var sealed []byte
	if err := row.Scan(&p.ID, &p.Name, &p.Label, &p.Version, &p.Protocol, &source, &p.Path, &p.Trust, &status, &p.StatusReason,
		&p.Enabled, &caps, &manifest, &config, &sealed, &p.InstalledAt, &p.UpdatedAt); err != nil {
		return nil, nil, err
	}
	p.Manifest = json.RawMessage(manifest)
	p.Source = registry.PluginSource(source)
	p.Status = registry.PluginStatus(status)
	if err := json.Unmarshal([]byte(caps), &p.Capabilities); err != nil {
		return nil, nil, fmt.Errorf("sqlite: plugin capabilities: %w", err)
	}
	p.Capabilities = nonNil(p.Capabilities)
	if err := json.Unmarshal([]byte(config), &p.Config); err != nil {
		return nil, nil, fmt.Errorf("sqlite: plugin config: %w", err)
	}
	if p.Config == nil {
		p.Config = map[string]any{}
	}
	return &p, sealed, nil
}

func (s *Store) GetPlugin(ctx context.Context, id string) (*registry.Plugin, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+pluginColumns+` FROM plugins WHERE id = ?`, id)
	p, sealed, err := scanPlugin(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: plugin %q", registry.ErrNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: get plugin: %w", err)
	}
	secrets, err := s.openSecrets(id, sealed)
	if err != nil {
		return p, fmt.Errorf("sqlite: plugin %q secrets: %w", id, err)
	}
	p.Secrets = secrets
	return p, nil
}

func (s *Store) ListPlugins(ctx context.Context) ([]*registry.Plugin, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+pluginColumns+` FROM plugins ORDER BY label`)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list plugins: %w", err)
	}
	defer rows.Close()
	out := []*registry.Plugin{}
	for rows.Next() {
		p, _, err := scanPlugin(rows)
		if err != nil {
			return nil, fmt.Errorf("sqlite: list plugins: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: list plugins: %w", err)
	}
	return out, nil
}

func (s *Store) UpdatePlugin(ctx context.Context, p *registry.Plugin) error {
	caps, err := json.Marshal(nonNil(p.Capabilities))
	if err != nil {
		return fmt.Errorf("sqlite: update plugin: %w", err)
	}
	p.UpdatedAt = time.Now().UTC()
	res, err := s.db.ExecContext(ctx, `
		UPDATE plugins SET version = ?, protocol = ?, path = ?, trust = ?, status = ?, status_reason = ?,
			enabled = ?, capabilities = ?, manifest = ?, updated_at = ?
		WHERE id = ?`,
		p.Version, p.Protocol, p.Path, p.Trust, string(p.Status), p.StatusReason, p.Enabled, string(caps), manifestJSON(p.Manifest), p.UpdatedAt, p.ID)
	if err != nil {
		return fmt.Errorf("sqlite: update plugin: %w", err)
	}
	return requireRowAffected(res, "plugin", p.ID)
}

func (s *Store) SetPluginConfig(ctx context.Context, id string, config map[string]any, secrets map[string]string) error {
	sealed, err := s.sealSecrets(id, secrets)
	if err != nil {
		return fmt.Errorf("sqlite: set plugin config: %w", err)
	}
	plain, err := marshalConfig(config)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE plugins SET config = ?, secrets = ?, updated_at = ? WHERE id = ?`,
		plain, sealed, time.Now().UTC(), id)
	if err != nil {
		return fmt.Errorf("sqlite: set plugin config: %w", err)
	}
	return requireRowAffected(res, "plugin", id)
}

func (s *Store) DeletePlugin(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM plugins WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("sqlite: delete plugin: %w", err)
	}
	return requireRowAffected(res, "plugin", id)
}

func (s *Store) GetSetting(ctx context.Context, key string) (string, error) {
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: setting %q", registry.ErrNotFound, key)
	}
	if err != nil {
		return "", fmt.Errorf("sqlite: get setting: %w", err)
	}
	return value, nil
}

func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	if err != nil {
		return fmt.Errorf("sqlite: set setting: %w", err)
	}
	return nil
}

// nonNil returns an empty slice for nil, so JSON and callers see [] not
// null.
func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
