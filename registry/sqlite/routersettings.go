package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Loomux/server/registry"
)

// Router model tiers set through Settings (LOOM-185). The API key is
// sealed under the master key with "router_tier:<tier>" as additional
// data, as ssh_keys binds its private keys to their ids.

func routerTierAAD(tier string) []byte { return []byte("router_tier:" + tier) }

// recordRouterChange writes change in tx, stamping it.
func recordRouterChange(ctx context.Context, tx *sql.Tx, change *registry.RouterSettingsChange, now time.Time) error {
	if change == nil {
		return nil
	}
	if change.ID == "" {
		change.ID = uuid.NewString()
	}
	change.CreatedAt = now
	_, err := tx.ExecContext(ctx, `
		INSERT INTO router_settings_audit (id, tier, action, fields, actor, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		change.ID, change.Tier, change.Action, strings.Join(change.Fields, ","), change.Actor, change.CreatedAt)
	return err
}

func (s *Store) SetRouterTier(ctx context.Context, t *registry.RouterTier, change *registry.RouterSettingsChange) error {
	if s.masterKey == nil {
		return fmt.Errorf("sqlite: set router tier: %w", registry.ErrNoMasterKey)
	}
	now := time.Now().UTC()
	sealed, err := seal(s.masterKey, []byte(t.APIKey), routerTierAAD(t.Tier))
	if err != nil {
		return fmt.Errorf("sqlite: encrypt router key: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: set router tier: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	t.SetAt = now
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO router_settings (tier, provider, base_url, model, api_key, key_fingerprint, key_last4, set_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (tier) DO UPDATE SET provider = excluded.provider, base_url = excluded.base_url,
			model = excluded.model, api_key = excluded.api_key, key_fingerprint = excluded.key_fingerprint,
			key_last4 = excluded.key_last4, set_at = excluded.set_at`,
		t.Tier, t.Provider, t.BaseURL, t.Model, sealed, t.KeyFingerprint, t.KeyLast4, t.SetAt); err != nil {
		return fmt.Errorf("sqlite: set router tier: %w", err)
	}
	if err := recordRouterChange(ctx, tx, change, now); err != nil {
		return fmt.Errorf("sqlite: record router settings change: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: set router tier: %w", err)
	}
	return nil
}

func (s *Store) ListRouterTiers(ctx context.Context) ([]*registry.RouterTier, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT tier, provider, base_url, model, api_key, key_fingerprint, key_last4, set_at
		FROM router_settings ORDER BY tier`)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list router tiers: %w", err)
	}
	defer rows.Close()
	out := []*registry.RouterTier{}
	var errs []error
	for rows.Next() {
		var t registry.RouterTier
		var sealed []byte
		if err := rows.Scan(&t.Tier, &t.Provider, &t.BaseURL, &t.Model, &sealed, &t.KeyFingerprint, &t.KeyLast4, &t.SetAt); err != nil {
			return nil, fmt.Errorf("sqlite: list router tiers: %w", err)
		}
		if s.masterKey == nil {
			errs = append(errs, fmt.Errorf("sqlite: router tier %q: %w", t.Tier, registry.ErrNoMasterKey))
			out = append(out, &t)
			continue
		}
		key, err := open(s.masterKey, sealed, routerTierAAD(t.Tier))
		if err != nil {
			errs = append(errs, fmt.Errorf("sqlite: router tier %q: %w", t.Tier, err))
		} else {
			t.APIKey = string(key)
		}
		out = append(out, &t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: list router tiers: %w", err)
	}
	return out, errors.Join(errs...)
}

func (s *Store) DeleteRouterTier(ctx context.Context, tier string, change *registry.RouterSettingsChange) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: delete router tier: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `DELETE FROM router_settings WHERE tier = ?`, tier)
	if err != nil {
		return fmt.Errorf("sqlite: delete router tier: %w", err)
	}
	if err := requireRowAffected(res, "router tier", tier); err != nil {
		return err
	}
	if err := recordRouterChange(ctx, tx, change, time.Now().UTC()); err != nil {
		return fmt.Errorf("sqlite: record router settings change: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: delete router tier: %w", err)
	}
	return nil
}

func (s *Store) ListRouterSettingsChanges(ctx context.Context, limit int) ([]*registry.RouterSettingsChange, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, tier, action, fields, actor, created_at FROM router_settings_audit
		ORDER BY created_at DESC, rowid DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list router settings changes: %w", err)
	}
	defer rows.Close()
	out := []*registry.RouterSettingsChange{}
	for rows.Next() {
		var c registry.RouterSettingsChange
		var fields string
		if err := rows.Scan(&c.ID, &c.Tier, &c.Action, &fields, &c.Actor, &c.CreatedAt); err != nil {
			return nil, fmt.Errorf("sqlite: list router settings changes: %w", err)
		}
		c.Fields = []string{}
		if fields != "" {
			c.Fields = strings.Split(fields, ",")
		}
		out = append(out, &c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: list router settings changes: %w", err)
	}
	return out, nil
}
