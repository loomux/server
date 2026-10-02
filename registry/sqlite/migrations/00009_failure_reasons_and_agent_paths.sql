-- LOOM-77: why a task failed (and how to classify it, plus the last of
-- its output), and why a workspace is in its status. LOOM-79: the
-- absolute path an agent CLI resolved to on a target, and the version
-- that path reports.

-- +goose Up
ALTER TABLE tasks ADD COLUMN failure_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN error_class TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN output_tail TEXT NOT NULL DEFAULT '';
ALTER TABLE workspaces ADD COLUMN status_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE target_agents ADD COLUMN path TEXT NOT NULL DEFAULT '';
ALTER TABLE target_agents ADD COLUMN version TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE target_agents DROP COLUMN version;
ALTER TABLE target_agents DROP COLUMN path;
ALTER TABLE workspaces DROP COLUMN status_reason;
ALTER TABLE tasks DROP COLUMN output_tail;
ALTER TABLE tasks DROP COLUMN error_class;
ALTER TABLE tasks DROP COLUMN failure_reason;
