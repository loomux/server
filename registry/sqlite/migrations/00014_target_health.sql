-- LOOM-86: each target's latest health probe — reachability, latency, tmux
-- version, disk free on the workspace root — and, per agent CLI, whether
-- it is signed in.

-- +goose Up
CREATE TABLE target_health (
    target_id       TEXT PRIMARY KEY REFERENCES targets (id) ON DELETE CASCADE,
    reachable       INTEGER NOT NULL,
    error           TEXT NOT NULL DEFAULT '',
    latency_ms      INTEGER NOT NULL DEFAULT 0,
    tmux_version    TEXT NOT NULL DEFAULT '',
    disk_free_bytes INTEGER NOT NULL DEFAULT -1,
    probed_at       TIMESTAMP NOT NULL
);
ALTER TABLE target_agents ADD COLUMN auth_status TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE target_agents DROP COLUMN auth_status;
DROP TABLE target_health;
