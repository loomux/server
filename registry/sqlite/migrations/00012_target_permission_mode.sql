-- How much agents launched on a target may do without asking: empty
-- (each agent-type's default), auto, accept-edits or manual.

-- +goose Up
ALTER TABLE targets ADD COLUMN permission_mode TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE targets DROP COLUMN permission_mode;
