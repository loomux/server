-- What the router models may see of a target's work (user decision
-- 2026-10-07): targets.relay is full, last_message, none, or empty for
-- the purpose's default (work: none). A message records the target its
-- turn acted on, so a target's policy can be applied to its history.

-- +goose Up
ALTER TABLE targets ADD COLUMN relay TEXT NOT NULL DEFAULT '';
ALTER TABLE messages ADD COLUMN origin_target_id TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE messages DROP COLUMN origin_target_id;
ALTER TABLE targets DROP COLUMN relay;
