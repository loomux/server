-- A conversation's event stream reads its tasks by conversation_id on
-- every poll (LOOM-145), instead of the whole table.

-- +goose Up
CREATE INDEX idx_tasks_conversation_id ON tasks (conversation_id);

-- +goose Down
DROP INDEX idx_tasks_conversation_id;
