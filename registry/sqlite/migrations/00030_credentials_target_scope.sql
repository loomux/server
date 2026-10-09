-- LOOM-178: a credential may be scoped to one target (a machine a plugin
-- made, whose agents all need the same token). One name per scope, the
-- target now part of it.

-- +goose Up
ALTER TABLE credentials ADD COLUMN target_id TEXT REFERENCES targets (id) ON DELETE RESTRICT;
DROP INDEX idx_credentials_name_scope;
CREATE UNIQUE INDEX idx_credentials_name_scope ON credentials (name, COALESCE(workspace_id, ''), COALESCE(target_id, ''), agent_type);
CREATE INDEX idx_credentials_target_id ON credentials (target_id);

-- +goose Down
DROP INDEX idx_credentials_target_id;
DROP INDEX idx_credentials_name_scope;
CREATE UNIQUE INDEX idx_credentials_name_scope ON credentials (name, COALESCE(workspace_id, ''), agent_type);
ALTER TABLE credentials DROP COLUMN target_id;
