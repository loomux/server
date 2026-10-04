-- LOOM-89: what Loomux may do on a target. purpose is personal or work
-- ('' = personal); allowed_agent_types is a JSON array, empty meaning
-- every agent type; no_provision / no_shell forbid new workspaces and
-- plain shell commands; require_confirmation makes new work there wait
-- for a "yes" in chat.

-- +goose Up
ALTER TABLE targets ADD COLUMN purpose TEXT NOT NULL DEFAULT '';
ALTER TABLE targets ADD COLUMN allowed_agent_types TEXT NOT NULL DEFAULT '[]';
ALTER TABLE targets ADD COLUMN no_provision INTEGER NOT NULL DEFAULT 0;
ALTER TABLE targets ADD COLUMN no_shell INTEGER NOT NULL DEFAULT 0;
ALTER TABLE targets ADD COLUMN require_confirmation INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE targets DROP COLUMN require_confirmation;
ALTER TABLE targets DROP COLUMN no_shell;
ALTER TABLE targets DROP COLUMN no_provision;
ALTER TABLE targets DROP COLUMN allowed_agent_types;
ALTER TABLE targets DROP COLUMN purpose;
