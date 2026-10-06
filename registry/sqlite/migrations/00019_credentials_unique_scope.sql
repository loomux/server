-- LOOM-134: one credential per name and scope. The table's UNIQUE (name,
-- workspace_id, agent_type) never caught two unscoped ones, because SQL
-- treats NULL workspace_ids as distinct. Nothing wrote the vault before
-- this, so no existing rows can collide.
-- +goose Up
CREATE UNIQUE INDEX idx_credentials_name_scope ON credentials (name, COALESCE(workspace_id, ''), agent_type);

-- +goose Down
DROP INDEX idx_credentials_name_scope;
