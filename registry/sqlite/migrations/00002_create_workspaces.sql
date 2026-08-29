-- +goose Up
CREATE TABLE workspaces (
    id              TEXT PRIMARY KEY,
    name            TEXT NOT NULL UNIQUE,
    path            TEXT NOT NULL,
    target_id       TEXT NOT NULL REFERENCES targets (id) ON DELETE RESTRICT,
    git_remote      TEXT NOT NULL DEFAULT '',
    tags            TEXT NOT NULL DEFAULT '[]',
    description     TEXT NOT NULL DEFAULT '',
    capabilities    TEXT NOT NULL DEFAULT '[]',
    status          TEXT NOT NULL CHECK (status IN ('idle', 'active', 'provisioning', 'archived')),
    is_dynamic      INTEGER NOT NULL DEFAULT 0,
    last_used_at    TIMESTAMP,
    rolling_summary TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMP NOT NULL,
    updated_at      TIMESTAMP NOT NULL
);

CREATE INDEX idx_workspaces_target_id ON workspaces (target_id);

-- +goose Down
DROP TABLE workspaces;
