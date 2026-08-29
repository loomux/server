-- +goose Up
CREATE TABLE credentials (
    id           TEXT PRIMARY KEY,
    name         TEXT NOT NULL,
    workspace_id TEXT REFERENCES workspaces (id) ON DELETE RESTRICT,
    agent_type   TEXT NOT NULL DEFAULT '',
    ciphertext   BLOB NOT NULL,
    created_at   TIMESTAMP NOT NULL,
    updated_at   TIMESTAMP NOT NULL,
    UNIQUE (name, workspace_id, agent_type)
);

CREATE INDEX idx_credentials_workspace_id ON credentials (workspace_id);

-- +goose Down
DROP TABLE credentials;
