-- LOOM-123: offers awaiting the user's yes, shown in the web UI as a
-- card with Approve and Deny, and how each was answered. A dispatch made
-- from such a card names the confirmation it answers.

-- +goose Up
CREATE TABLE confirmations (
    id              TEXT PRIMARY KEY,
    conversation_id TEXT NOT NULL,
    dispatch_id     TEXT,
    kind            TEXT NOT NULL,
    target_id       TEXT NOT NULL DEFAULT '',
    target_name     TEXT NOT NULL DEFAULT '',
    agent_type      TEXT NOT NULL DEFAULT '',
    command         TEXT NOT NULL DEFAULT '',
    workspace       TEXT NOT NULL DEFAULT '',
    git_remote      TEXT NOT NULL DEFAULT '',
    status          TEXT NOT NULL CHECK (status IN ('pending', 'approved', 'denied', 'expired')),
    created_at      TIMESTAMP NOT NULL,
    expires_at      TIMESTAMP NOT NULL,
    resolved_at     TIMESTAMP
);
CREATE INDEX idx_confirmations_conversation_id ON confirmations (conversation_id);

ALTER TABLE dispatches ADD COLUMN confirmation_id TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE dispatches DROP COLUMN confirmation_id;
DROP TABLE confirmations;
