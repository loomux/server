-- LOOM-263: holds, a lease on a task by an API session (the terminal
-- client attached to its pane). One active drive hold per task, which the
-- partial unique index enforces; any number of watch holds. Ended rows are
-- kept as the audit trail until retention deletes them. session_id is not
-- a foreign key: a session is deleted at logout while its ended holds stay.
-- ttl_ms is the lease's length, which each renewal pushes expires_at out by.

-- +goose Up
CREATE TABLE task_holds (
    id           TEXT PRIMARY KEY,
    task_id      TEXT NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
    session_id   TEXT NOT NULL,
    client_kind  TEXT NOT NULL DEFAULT '',
    client_label TEXT NOT NULL DEFAULT '',
    mode         TEXT NOT NULL CHECK (mode IN ('drive', 'watch')),
    prev_status  TEXT NOT NULL DEFAULT '',
    acquired_at  TIMESTAMP NOT NULL,
    renewed_at   TIMESTAMP NOT NULL,
    ttl_ms       INTEGER NOT NULL CHECK (ttl_ms > 0),
    expires_at   TIMESTAMP NOT NULL,
    ended_at     TIMESTAMP,
    ended_reason TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX idx_task_holds_one_active_drive ON task_holds (task_id)
    WHERE ended_at IS NULL AND mode = 'drive';
CREATE INDEX idx_task_holds_task_id ON task_holds (task_id);
CREATE INDEX idx_task_holds_active ON task_holds (expires_at) WHERE ended_at IS NULL;

-- +goose Down
DROP TABLE task_holds;
