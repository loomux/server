-- +goose Up
CREATE TABLE messages (
    id              TEXT PRIMARY KEY,
    conversation_id TEXT NOT NULL,
    task_id         TEXT REFERENCES tasks (id) ON DELETE SET NULL,
    role            TEXT NOT NULL CHECK (role IN ('user', 'assistant')),
    content         TEXT NOT NULL,
    created_at      TIMESTAMP NOT NULL
);

CREATE INDEX idx_messages_conversation_id ON messages (conversation_id);
CREATE INDEX idx_messages_task_id ON messages (task_id);

-- +goose Down
DROP TABLE messages;
