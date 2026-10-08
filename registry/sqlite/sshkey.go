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

// SSH keys Loomux manages (LOOM-138). The private key is sealed under the
// master key with "ssh_key:<id>" as additional data.

func sshKeyAAD(id string) []byte { return []byte("ssh_key:" + id) }

// isTriggerErr reports whether err is one of 00023's triggers refusing.
func isTriggerErr(err error, reason string) bool {
	return err != nil && strings.Contains(err.Error(), "loomux: "+reason)
}

func (s *Store) CreateSSHKey(ctx context.Context, k *registry.SSHKey) error {
	if s.masterKey == nil {
		return fmt.Errorf("sqlite: create ssh key: %w", registry.ErrNoMasterKey)
	}
	k.CreatedAt = time.Now().UTC()
	sealed, err := seal(s.masterKey, k.PrivateKey, sshKeyAAD(k.ID))
	if err != nil {
		return fmt.Errorf("sqlite: encrypt ssh key: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO ssh_keys (id, name, type, public_key, fingerprint, origin, private_key, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		k.ID, k.Name, k.Type, k.PublicKey, k.Fingerprint, k.Origin, sealed, k.CreatedAt,
	)
	if isUniqueConstraintErr(err) {
		return fmt.Errorf("%w: ssh key %q already exists", registry.ErrConflict, k.Name)
	}
	if err != nil {
		return fmt.Errorf("sqlite: create ssh key: %w", err)
	}
	return nil
}

const sshKeyColumns = `id, name, type, public_key, fingerprint, origin, created_at`

func (s *Store) GetSSHKey(ctx context.Context, id string) (*registry.SSHKey, error) {
	if s.masterKey == nil {
		return nil, fmt.Errorf("sqlite: get ssh key: %w", registry.ErrNoMasterKey)
	}
	var k registry.SSHKey
	var sealed []byte
	err := s.db.QueryRowContext(ctx, `SELECT `+sshKeyColumns+`, private_key FROM ssh_keys WHERE id = ?`, id).
		Scan(&k.ID, &k.Name, &k.Type, &k.PublicKey, &k.Fingerprint, &k.Origin, &k.CreatedAt, &sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: ssh key %q", registry.ErrNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: get ssh key: %w", err)
	}
	if k.PrivateKey, err = open(s.masterKey, sealed, sshKeyAAD(k.ID)); err != nil {
		return nil, fmt.Errorf("sqlite: ssh key %q: %w", id, err)
	}
	return &k, nil
}

func (s *Store) ListSSHKeys(ctx context.Context) ([]*registry.SSHKey, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+sshKeyColumns+` FROM ssh_keys ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list ssh keys: %w", err)
	}
	defer rows.Close()
	out := []*registry.SSHKey{}
	for rows.Next() {
		var k registry.SSHKey
		if err := rows.Scan(&k.ID, &k.Name, &k.Type, &k.PublicKey, &k.Fingerprint, &k.Origin, &k.CreatedAt); err != nil {
			return nil, fmt.Errorf("sqlite: list ssh keys: %w", err)
		}
		out = append(out, &k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: list ssh keys: %w", err)
	}
	return out, nil
}

func (s *Store) DeleteSSHKey(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM ssh_keys WHERE id = ?`, id)
	if isTriggerErr(err, "ssh key in use") {
		return &registry.ConflictError{Reason: "a target still uses this SSH key"}
	}
	if err != nil {
		return fmt.Errorf("sqlite: delete ssh key: %w", err)
	}
	return requireRowAffected(res, "ssh key", id)
}
