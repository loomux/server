-- LOOM-80: dispatch jobs, so a chat turn outlives the HTTP request that
-- submitted it. See docs/design/async-dispatch-design.md.

-- +goose Up
CREATE TABLE dispatches (
    id              TEXT PRIMARY KEY,
    conversation_id TEXT NOT NULL,
    message         TEXT NOT NULL,
    workspace_hint  TEXT NOT NULL DEFAULT '',
    idempotency_key TEXT,
    request_hash    TEXT NOT NULL,
    status          TEXT NOT NULL CHECK (status IN ('queued', 'running', 'succeeded', 'failed', 'interrupted')),
    reply           TEXT NOT NULL DEFAULT '',
    error           TEXT NOT NULL DEFAULT '',
    error_class     TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMP NOT NULL,
    updated_at      TIMESTAMP NOT NULL,
    started_at      TIMESTAMP,
    finished_at     TIMESTAMP
);

CREATE INDEX idx_dispatches_conversation_id ON dispatches (conversation_id);
CREATE INDEX idx_dispatches_status ON dispatches (status);
CREATE UNIQUE INDEX idx_dispatches_idempotency_key ON dispatches (idempotency_key)
    WHERE idempotency_key IS NOT NULL;
-- At most one dispatch in flight per conversation; the index is what
-- makes two racing submits safe.
CREATE UNIQUE INDEX idx_dispatches_one_active ON dispatches (conversation_id)
    WHERE status IN ('queued', 'running');

ALTER TABLE messages ADD COLUMN dispatch_id TEXT REFERENCES dispatches (id) ON DELETE SET NULL;

-- +goose Down
ALTER TABLE messages DROP COLUMN dispatch_id;
DROP TABLE dispatches;
