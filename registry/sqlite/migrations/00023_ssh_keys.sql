-- LOOM-138: SSH keys Loomux manages (docs/design/target-onboarding.md).
-- private_key is AES-256-GCM under the master key, with the row's id as
-- additional data. targets.ssh_key_ref names one; it predates this table
-- and nothing ever read it, so whatever an early client stored there is
-- cleared. SQLite can't add a foreign key to an existing column, so the
-- triggers enforce one.

-- +goose Up
CREATE TABLE ssh_keys (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,
    type        TEXT NOT NULL,
    public_key  TEXT NOT NULL,
    fingerprint TEXT NOT NULL,
    origin      TEXT NOT NULL,
    private_key BLOB NOT NULL,
    created_at  TIMESTAMP NOT NULL
);

UPDATE targets SET ssh_key_ref = '' WHERE ssh_key_ref <> '';

-- +goose StatementBegin
CREATE TRIGGER ssh_keys_in_use BEFORE DELETE ON ssh_keys
WHEN EXISTS (SELECT 1 FROM targets WHERE ssh_key_ref = OLD.id)
BEGIN
    SELECT RAISE(ABORT, 'loomux: ssh key in use');
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER targets_ssh_key_ref_insert BEFORE INSERT ON targets
WHEN NEW.ssh_key_ref <> '' AND NOT EXISTS (SELECT 1 FROM ssh_keys WHERE id = NEW.ssh_key_ref)
BEGIN
    SELECT RAISE(ABORT, 'loomux: no such ssh key');
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER targets_ssh_key_ref_update BEFORE UPDATE OF ssh_key_ref ON targets
WHEN NEW.ssh_key_ref <> '' AND NOT EXISTS (SELECT 1 FROM ssh_keys WHERE id = NEW.ssh_key_ref)
BEGIN
    SELECT RAISE(ABORT, 'loomux: no such ssh key');
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER targets_ssh_key_ref_update;
DROP TRIGGER targets_ssh_key_ref_insert;
DROP TRIGGER ssh_keys_in_use;
DROP TABLE ssh_keys;
