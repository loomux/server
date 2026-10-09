-- LOOM-192: a credential's ciphertext is bound to its scope as well as
-- its row id: the AES-GCM additional data names the row's workspace_id,
-- target_id and agent_type, so a row moved to another scope doesn't
-- decrypt there.
-- sealed_to_id becomes seal_version, saying what a row is bound to:
-- 0 nothing (before LOOM-175), 1 its id (LOOM-175), 2 its id and scope.
-- Existing rows keep their version; Open re-seals every row below 2 once
-- a master key is configured (Store.resealCredentials) and then records
-- the re-seal as done in vault_reseal, so later starts skip the scan.
-- +goose Up
ALTER TABLE credentials RENAME COLUMN sealed_to_id TO seal_version;

CREATE TABLE vault_reseal (
    seal_version INTEGER PRIMARY KEY,
    done_at      TIMESTAMP NOT NULL
);

-- +goose Down
-- Code from before this migration can't read a row bound to its scope:
-- delete and re-add credentials after going back
-- (docs/deploy/operations.md). Such rows are marked bound to their id,
-- the most that code understands, so they fail as undecryptable rather
-- than as an unreadable column.
DROP TABLE vault_reseal;
ALTER TABLE credentials RENAME COLUMN seal_version TO sealed_to_id;
UPDATE credentials SET sealed_to_id = 1 WHERE sealed_to_id > 1;
