-- +goose Up
CREATE TABLE tasks (
    id              TEXT PRIMARY KEY,
    workspace_id    TEXT NOT NULL REFERENCES workspaces (id) ON DELETE RESTRICT,
    kind            TEXT NOT NULL CHECK (kind IN ('agent', 'shell')),
    agent_type      TEXT NOT NULL DEFAULT '',
    tmux_session    TEXT NOT NULL,
    status          TEXT NOT NULL CHECK (status IN ('running', 'awaiting-input', 'human-takeover', 'completed', 'failed')),
    conversation_id TEXT NOT NULL,
    created_at      TIMESTAMP NOT NULL,
    updated_at      TIMESTAMP NOT NULL,
    started_at      TIMESTAMP,
    completed_at    TIMESTAMP
);

CREATE INDEX idx_tasks_workspace_id ON tasks (workspace_id);

-- +goose Down
DROP TABLE tasks;
