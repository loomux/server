### Added

- A target can keep its idle sessions (LOOM-227): with `keep_idle_sessions: true` (API, default `false`), the idle reaper never closes that target's sessions however long they wait for a follow-up. This is for a hub such as a command-center machine, whose own conversation waits days for its next message. A machine in `LOOMUX_PLUGINS_SEED` takes it too.

### Changed

- `LOOMUX_REAP_IDLE_THRESHOLD=off` (or `0`) now turns idle reaping off; before, `0` meant the 24h default. A negative threshold, which would close every waiting session on the next sweep, is refused at startup.
