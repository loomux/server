-- LOOM-138: whether a managed target (one with a Loomux SSH key) is
-- reached through the server's LOOMUX_SSH_PROXY ('') or directly ('none').

-- +goose Up
ALTER TABLE targets ADD COLUMN ssh_proxy TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE targets DROP COLUMN ssh_proxy;
