-- LOOM-216: why a yes was refused afterwards (the offer changed, the
-- name was taken, the quota was reached), for the offer's card.

-- +goose Up
ALTER TABLE confirmations ADD COLUMN status_reason TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE confirmations DROP COLUMN status_reason;
