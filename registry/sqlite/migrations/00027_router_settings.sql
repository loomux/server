-- LOOM-185: router model tiers set through Settings, overriding the
-- environment's (docs/design/router-settings.md). api_key is AES-256-GCM
-- under the master key with "router_tier:<tier>" as additional data, as
-- ssh_keys does with its id. The audit trail names the fields a change
-- touched, never their values.

-- +goose Up
CREATE TABLE router_settings (
    tier            TEXT PRIMARY KEY CHECK (tier IN ('primary', 'escalation')),
    provider        TEXT NOT NULL,
    base_url        TEXT NOT NULL,
    model           TEXT NOT NULL,
    api_key         BLOB NOT NULL,
    key_fingerprint TEXT NOT NULL,
    key_last4       TEXT NOT NULL,
    set_at          TIMESTAMP NOT NULL
);

CREATE TABLE router_settings_audit (
    id         TEXT PRIMARY KEY,
    tier       TEXT NOT NULL,
    action     TEXT NOT NULL,
    fields     TEXT NOT NULL,
    actor      TEXT NOT NULL,
    created_at TIMESTAMP NOT NULL
);

CREATE INDEX idx_router_settings_audit_created_at ON router_settings_audit (created_at);

-- +goose Down
DROP INDEX idx_router_settings_audit_created_at;
DROP TABLE router_settings_audit;
DROP TABLE router_settings;
