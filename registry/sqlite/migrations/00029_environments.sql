-- LOOM-178: machines a plugin made (docs/design/target-providers.md
-- §1.7): one per target, with the plugin that owns it (NULL after an
-- uninstall that kept the machines), the host key the host generated for
-- it (the pin's source; the private half AES-256-GCM under the master
-- key with "environment:<id>" as additional data, so it can be given to
-- a recreated machine), and what it was created with.

-- +goose Up
CREATE TABLE environments (
    id               TEXT PRIMARY KEY,
    target_id        TEXT NOT NULL UNIQUE REFERENCES targets (id) ON DELETE RESTRICT,
    plugin_id        TEXT REFERENCES plugins (id) ON DELETE RESTRICT,
    plugin_name      TEXT NOT NULL,
    plugin_version   TEXT NOT NULL,
    status           TEXT NOT NULL CHECK (status IN ('creating', 'starting', 'running', 'stopped',
                                                     'recreating', 'destroying', 'lost', 'error', 'detached')),
    status_reason    TEXT NOT NULL DEFAULT '',
    size             TEXT NOT NULL,
    persistent       INTEGER NOT NULL,
    egress           TEXT NOT NULL,
    image            TEXT NOT NULL,
    image_digest     TEXT NOT NULL DEFAULT '',
    host_key         TEXT NOT NULL,
    host_private_key BLOB NOT NULL,
    created_at       TIMESTAMP NOT NULL,
    updated_at       TIMESTAMP NOT NULL
);

CREATE INDEX idx_environments_plugin_id ON environments (plugin_id);

-- +goose Down
DROP INDEX idx_environments_plugin_id;
DROP TABLE environments;
