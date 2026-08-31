-- +goose Up
ALTER TABLE tasks ADD COLUMN reaped_at TIMESTAMP;

-- +goose Down
ALTER TABLE tasks DROP COLUMN reaped_at;
