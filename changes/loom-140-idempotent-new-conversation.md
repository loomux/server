### Fixed

- Retrying a new conversation's first message with the same `Idempotency-Key` (LOOM-140): a client that sent no `conversation_id` and retried after a dropped connection got `422 idempotency_conflict` while the original turn ran on in a conversation it never learned of. The retry now returns the original dispatch and its `conversation_id`.
