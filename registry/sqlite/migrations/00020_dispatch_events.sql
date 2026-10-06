-- LOOM-110: the dispatch audit trail. What each turn decided (model and
-- tier), ran (command, redacted) and where, and how it ended, queryable
-- after pod logs are gone. Kept for the retention period
-- (LOOMUX_EVENT_RETENTION, 90 days by default).

-- +goose Up
CREATE TABLE dispatch_events (
    id              TEXT PRIMARY KEY,
    conversation_id TEXT NOT NULL,
    dispatch_id     TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMP NOT NULL,
    kind            TEXT NOT NULL,
    model           TEXT NOT NULL DEFAULT '',
    tier            TEXT NOT NULL DEFAULT '',
    target_id       TEXT NOT NULL DEFAULT '',
    workspace_id    TEXT NOT NULL DEFAULT '',
    task_id         TEXT NOT NULL DEFAULT '',
    command         TEXT NOT NULL DEFAULT '',
    outcome         TEXT NOT NULL DEFAULT '',
    error_class     TEXT NOT NULL DEFAULT '',
    duration_ms     INTEGER NOT NULL DEFAULT 0,
    detail          TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_dispatch_events_conversation_id ON dispatch_events (conversation_id, created_at);
CREATE INDEX idx_dispatch_events_created_at ON dispatch_events (created_at);

-- +goose Down
DROP TABLE dispatch_events;
