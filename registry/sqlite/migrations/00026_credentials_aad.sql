-- LOOM-175: a credential's ciphertext is bound to its row id (AES-GCM
-- additional data), as an SSH key's is (LOOM-138), so a ciphertext copied
-- onto another row doesn't decrypt there. sealed_to_id says a row's is;
-- rows from before carry none, and Open re-seals them once a master key
-- is configured (Store.resealCredentials).
-- +goose Up
ALTER TABLE credentials ADD COLUMN sealed_to_id INTEGER NOT NULL DEFAULT 0;

-- +goose Down
-- Code from before this migration can't read a re-sealed row: delete and
-- re-add credentials after going back (docs/deploy/operations.md).
ALTER TABLE credentials DROP COLUMN sealed_to_id;
