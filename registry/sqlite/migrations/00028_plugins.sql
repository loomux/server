-- LOOM-178: installed plugins (docs/design/target-providers.md §1.7) and
-- server settings (the instance id, generated once). secrets is the
-- plugin's secret configuration fields as one JSON object, AES-256-GCM
-- under the master key with "plugin:<id>" as additional data, as
-- router_settings binds a key to its tier; config is the rest, plain.
-- manifest is the installed manifest, so the configuration form and an
-- upgrade's comparison don't need the plugin's files to be there.

-- +goose Up
CREATE TABLE plugins (
    id            TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    label         TEXT NOT NULL UNIQUE,
    version       TEXT NOT NULL,
    protocol      TEXT NOT NULL,
    source        TEXT NOT NULL CHECK (source IN ('bundled', 'dir', 'socket')),
    path          TEXT NOT NULL,
    trust         TEXT NOT NULL,
    status        TEXT NOT NULL CHECK (status IN ('installing', 'installed', 'disabled', 'error')),
    status_reason TEXT NOT NULL DEFAULT '',
    enabled       INTEGER NOT NULL DEFAULT 1,
    capabilities  TEXT NOT NULL DEFAULT '[]',
    manifest      TEXT NOT NULL DEFAULT '{}',
    config        TEXT NOT NULL DEFAULT '{}',
    secrets       BLOB,
    installed_at  TIMESTAMP NOT NULL,
    updated_at    TIMESTAMP NOT NULL
);

CREATE TABLE settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- +goose Down
DROP TABLE settings;
DROP TABLE plugins;
