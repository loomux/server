-- LOOM-91: what each turn of a task produced, kept after the task ends:
-- the message sent, the agent's own final message (when its completion
-- hook saved one) and the pane's scrollback at the turn's end, with
-- credential values redacted.

-- +goose Up
CREATE TABLE task_turns (
    id            TEXT PRIMARY KEY,
    task_id       TEXT NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
    user_message  TEXT NOT NULL DEFAULT '',
    agent_message TEXT NOT NULL DEFAULT '',
    pane          TEXT NOT NULL DEFAULT '',
    created_at    TIMESTAMP NOT NULL
);
CREATE INDEX idx_task_turns_task_id ON task_turns (task_id, created_at);

-- +goose Down
DROP TABLE task_turns;
