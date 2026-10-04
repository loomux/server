-- LOOM-97: a task can be 'needs-attention' — its agent stopped at a
-- prompt only a human can answer — and records that prompt (JSON:
-- registry.Attention; empty when it isn't stopped at one).
--
-- SQLite can't ALTER a CHECK constraint, so tasks is rebuilt, as in
-- 00008 (see there for why NO TRANSACTION and one statement block).

-- +goose NO TRANSACTION
-- +goose Up
-- +goose StatementBegin
PRAGMA foreign_keys = OFF;
BEGIN;

CREATE TABLE tasks_new (
    id              TEXT PRIMARY KEY,
    workspace_id    TEXT NOT NULL REFERENCES workspaces (id) ON DELETE RESTRICT,
    kind            TEXT NOT NULL CHECK (kind IN ('agent', 'shell', 'command')),
    agent_type      TEXT NOT NULL DEFAULT '',
    tmux_session    TEXT NOT NULL,
    status          TEXT NOT NULL CHECK (status IN ('running', 'awaiting-input', 'human-takeover', 'needs-attention', 'completed', 'failed')),
    conversation_id TEXT NOT NULL,
    created_at      TIMESTAMP NOT NULL,
    updated_at      TIMESTAMP NOT NULL,
    started_at      TIMESTAMP,
    completed_at    TIMESTAMP,
    reaped_at       TIMESTAMP,
    command         TEXT NOT NULL DEFAULT '',
    exit_code       INTEGER,
    failure_reason  TEXT NOT NULL DEFAULT '',
    error_class     TEXT NOT NULL DEFAULT '',
    output_tail     TEXT NOT NULL DEFAULT '',
    attention       TEXT NOT NULL DEFAULT ''
);
INSERT INTO tasks_new (id, workspace_id, kind, agent_type, tmux_session, status, conversation_id,
                       created_at, updated_at, started_at, completed_at, reaped_at, command, exit_code,
                       failure_reason, error_class, output_tail)
    SELECT id, workspace_id, kind, agent_type, tmux_session, status, conversation_id,
           created_at, updated_at, started_at, completed_at, reaped_at, command, exit_code,
           failure_reason, error_class, output_tail
    FROM tasks;
DROP TABLE tasks;
ALTER TABLE tasks_new RENAME TO tasks;
CREATE INDEX idx_tasks_workspace_id ON tasks (workspace_id);

COMMIT;
PRAGMA foreign_keys = ON;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
PRAGMA foreign_keys = OFF;
BEGIN;

CREATE TABLE tasks_old (
    id              TEXT PRIMARY KEY,
    workspace_id    TEXT NOT NULL REFERENCES workspaces (id) ON DELETE RESTRICT,
    kind            TEXT NOT NULL CHECK (kind IN ('agent', 'shell', 'command')),
    agent_type      TEXT NOT NULL DEFAULT '',
    tmux_session    TEXT NOT NULL,
    status          TEXT NOT NULL CHECK (status IN ('running', 'awaiting-input', 'human-takeover', 'completed', 'failed')),
    conversation_id TEXT NOT NULL,
    created_at      TIMESTAMP NOT NULL,
    updated_at      TIMESTAMP NOT NULL,
    started_at      TIMESTAMP,
    completed_at    TIMESTAMP,
    reaped_at       TIMESTAMP,
    command         TEXT NOT NULL DEFAULT '',
    exit_code       INTEGER,
    failure_reason  TEXT NOT NULL DEFAULT '',
    error_class     TEXT NOT NULL DEFAULT '',
    output_tail     TEXT NOT NULL DEFAULT ''
);
INSERT INTO tasks_old
    SELECT id, workspace_id, kind, agent_type, tmux_session,
           CASE status WHEN 'needs-attention' THEN 'running' ELSE status END, conversation_id,
           created_at, updated_at, started_at, completed_at, reaped_at, command, exit_code,
           failure_reason, error_class, output_tail
    FROM tasks;
DROP TABLE tasks;
ALTER TABLE tasks_old RENAME TO tasks;
CREATE INDEX idx_tasks_workspace_id ON tasks (workspace_id);

COMMIT;
PRAGMA foreign_keys = ON;
-- +goose StatementEnd
