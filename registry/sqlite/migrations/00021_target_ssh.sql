-- LOOM-114: target onboarding through the API. host_keys are known_hosts
-- lines pinned for the target (scan, then pin by fingerprint); a pinned
-- target is checked against them alone. ssh_port overrides the port the
-- mounted SSH config gives.

-- +goose Up
ALTER TABLE targets ADD COLUMN ssh_port INTEGER NOT NULL DEFAULT 0;
ALTER TABLE targets ADD COLUMN host_keys TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE targets DROP COLUMN host_keys;
ALTER TABLE targets DROP COLUMN ssh_port;
