-- +goose Up
CREATE TABLE sessions (
    id           TEXT PRIMARY KEY,
    token_hash   TEXT NOT NULL UNIQUE,
    created_at   TIMESTAMP NOT NULL,
    last_used_at TIMESTAMP NOT NULL
);

CREATE INDEX idx_sessions_token_hash ON sessions (token_hash);

-- +goose Down
DROP TABLE sessions;
