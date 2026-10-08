### Security

- A vault credential's ciphertext is bound to its row (LOOM-175), as SSH keys' are since LOOM-138: the AES-GCM additional data is the row id, so a ciphertext copied onto another credential's row (by someone with write access to the database but not the key) no longer decrypts there. Migration 00026 marks which rows are bound; the first start with `LOOMUX_MASTER_KEY` re-seals the existing ones in one transaction. A release from before this can't read re-sealed rows: roll back with the pre-upgrade snapshot.
