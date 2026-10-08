### Fixed

- A dispatch whose status couldn't be recorded no longer leaves its conversation busy until a restart (LOOM-146). A job's status writes are retried with backoff, and a sweep every minute marks interrupted any queued or running dispatch with no job behind it, so the next message to that conversation goes through instead of getting `409 conversation_busy`.
