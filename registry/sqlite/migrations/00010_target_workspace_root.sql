-- LOOM-90: the directory a target's dynamic workspaces are provisioned
-- under. Empty means the default, $HOME/loomux-workspaces on the target.

-- +goose Up
ALTER TABLE targets ADD COLUMN workspace_root TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE targets DROP COLUMN workspace_root;
