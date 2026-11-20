-- LOOM-307: a target pins each agent type's model and effort, as JSON
-- keyed by agent type ({"claude-code":{"model":"opus","effort":"high"}});
-- existing targets pin nothing. A task records what it was launched
-- with, so attach-info reports the running pane even after the pin
-- changes; existing tasks launched unpinned.
-- (00039, not 00038: draft PR #558 adds 00038. Whichever merges second
-- must carry the higher number, or goose refuses it on a database that
-- already ran the other.)

-- +goose Up
ALTER TABLE targets ADD COLUMN agent_args TEXT NOT NULL DEFAULT '{}';
ALTER TABLE tasks ADD COLUMN agent_model TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN agent_effort TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE tasks DROP COLUMN agent_effort;
ALTER TABLE tasks DROP COLUMN agent_model;
ALTER TABLE targets DROP COLUMN agent_args;
