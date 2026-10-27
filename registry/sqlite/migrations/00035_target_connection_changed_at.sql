-- LOOM-256: when how Loomux reaches a target last changed, so renaming it
-- or changing its permission mode or policy leaves its last probe, and
-- so its readiness, standing.

-- +goose Up
ALTER TABLE targets ADD COLUMN connection_changed_at TIMESTAMP;
UPDATE targets SET connection_changed_at = updated_at;

-- +goose Down
ALTER TABLE targets DROP COLUMN connection_changed_at;
