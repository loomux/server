-- +goose Up
CREATE TABLE targets (
    id            TEXT PRIMARY KEY,
    name          TEXT NOT NULL UNIQUE,
    kind          TEXT NOT NULL CHECK (kind IN ('local', 'remote')),
    host          TEXT NOT NULL DEFAULT '',
    user          TEXT NOT NULL DEFAULT '',
    ssh_key_ref   TEXT NOT NULL DEFAULT '',
    created_at    TIMESTAMP NOT NULL,
    updated_at    TIMESTAMP NOT NULL
);

-- +goose Down
DROP TABLE targets;
