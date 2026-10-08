### Fixed

- Shutdown now drains as intended (LOOM-147): loomuxd waits for the HTTP server's shutdown to finish before closing the database, so requests in flight when it stops (including a blocking dispatch released by the drain) get their full answer. Conversation streams are ended as shutdown begins instead of holding it open for its whole grace period.
