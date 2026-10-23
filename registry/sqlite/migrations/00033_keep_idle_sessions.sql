-- LOOM-227: a target whose idle sessions the reaper leaves open (a
-- hub's own session waits for its next message for days).

-- +goose Up
ALTER TABLE targets ADD COLUMN keep_idle_sessions INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE targets DROP COLUMN keep_idle_sessions;
