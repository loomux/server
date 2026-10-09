-- LOOM-178: a credential may be scoped to one target (a machine a plugin
-- made, whose agents all need the same token). One name per scope, the
-- target now part of it. A target's credentials go with it: a credential
-- scoped to a target that no longer exists is useless, and a delete
-- refused over one would read as a 409 about workspaces.

-- +goose Up
ALTER TABLE credentials ADD COLUMN target_id TEXT REFERENCES targets (id) ON DELETE CASCADE;
DROP INDEX idx_credentials_name_scope;
CREATE UNIQUE INDEX idx_credentials_name_scope ON credentials (name, COALESCE(workspace_id, ''), COALESCE(target_id, ''), agent_type);
CREATE INDEX idx_credentials_target_id ON credentials (target_id);

-- +goose Down
DROP INDEX idx_credentials_target_id;
DROP INDEX idx_credentials_name_scope;
CREATE UNIQUE INDEX idx_credentials_name_scope ON credentials (name, COALESCE(workspace_id, ''), agent_type);
ALTER TABLE credentials DROP COLUMN target_id;
