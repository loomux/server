-- LOOM-71: a failed provision leaves its workspace 'failed' (never
-- 'active'); one-shot 'command' tasks (an agent install, LOOM-72's direct
-- shell commands) record the command they ran and its exit code; and the
-- per-target agent CLI availability the router is told about.
--
-- SQLite can't ALTER a CHECK constraint, so workspaces and tasks are
-- rebuilt (https://sqlite.org/lang_altertable.html#otheralter). That needs
-- foreign_keys OFF, which is a no-op inside a transaction and per
-- connection — hence NO TRANSACTION, and the whole rebuild as a single
-- statement block so it all runs on one pooled connection, inside its own
-- BEGIN/COMMIT, with foreign_keys switched back on afterwards.

-- +goose NO TRANSACTION
-- +goose Up
-- +goose StatementBegin
PRAGMA foreign_keys = OFF;
BEGIN;

CREATE TABLE workspaces_new (
    id              TEXT PRIMARY KEY,
    name            TEXT NOT NULL UNIQUE,
    path            TEXT NOT NULL,
    target_id       TEXT NOT NULL REFERENCES targets (id) ON DELETE RESTRICT,
    git_remote      TEXT NOT NULL DEFAULT '',
    tags            TEXT NOT NULL DEFAULT '[]',
    description     TEXT NOT NULL DEFAULT '',
    capabilities    TEXT NOT NULL DEFAULT '[]',
    status          TEXT NOT NULL CHECK (status IN ('idle', 'active', 'provisioning', 'archived', 'failed')),
    is_dynamic      INTEGER NOT NULL DEFAULT 0,
    last_used_at    TIMESTAMP,
    rolling_summary TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMP NOT NULL,
    updated_at      TIMESTAMP NOT NULL
);
INSERT INTO workspaces_new
    SELECT id, name, path, target_id, git_remote, tags, description, capabilities, status,
           is_dynamic, last_used_at, rolling_summary, created_at, updated_at
    FROM workspaces;
DROP TABLE workspaces;
ALTER TABLE workspaces_new RENAME TO workspaces;
CREATE INDEX idx_workspaces_target_id ON workspaces (target_id);

CREATE TABLE tasks_new (
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
    exit_code       INTEGER
);
INSERT INTO tasks_new (id, workspace_id, kind, agent_type, tmux_session, status, conversation_id,
                       created_at, updated_at, started_at, completed_at, reaped_at)
    SELECT id, workspace_id, kind, agent_type, tmux_session, status, conversation_id,
           created_at, updated_at, started_at, completed_at, reaped_at
    FROM tasks;
DROP TABLE tasks;
ALTER TABLE tasks_new RENAME TO tasks;
CREATE INDEX idx_tasks_workspace_id ON tasks (workspace_id);

CREATE TABLE target_agents (
    target_id  TEXT NOT NULL REFERENCES targets (id) ON DELETE CASCADE,
    agent_type TEXT NOT NULL,
    available  INTEGER NOT NULL,
    checked_at TIMESTAMP NOT NULL,
    PRIMARY KEY (target_id, agent_type)
);

COMMIT;
PRAGMA foreign_keys = ON;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
PRAGMA foreign_keys = OFF;
BEGIN;

DROP TABLE target_agents;

CREATE TABLE tasks_old (
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
    completed_at    TIMESTAMP,
    reaped_at       TIMESTAMP
);
INSERT INTO tasks_old
    SELECT id, workspace_id, CASE kind WHEN 'command' THEN 'shell' ELSE kind END, agent_type, tmux_session,
           status, conversation_id, created_at, updated_at, started_at, completed_at, reaped_at
    FROM tasks;
DROP TABLE tasks;
ALTER TABLE tasks_old RENAME TO tasks;
CREATE INDEX idx_tasks_workspace_id ON tasks (workspace_id);

CREATE TABLE workspaces_old (
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
INSERT INTO workspaces_old
    SELECT id, name, path, target_id, git_remote, tags, description, capabilities,
           CASE status WHEN 'failed' THEN 'idle' ELSE status END,
           is_dynamic, last_used_at, rolling_summary, created_at, updated_at
    FROM workspaces;
DROP TABLE workspaces;
ALTER TABLE workspaces_old RENAME TO workspaces;
CREATE INDEX idx_workspaces_target_id ON workspaces (target_id);

COMMIT;
PRAGMA foreign_keys = ON;
-- +goose StatementEnd
