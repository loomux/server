### Added

- A plugin's `min_host_version` is enforced (LOOM-194): read as a caret range on the server's version (under 0.x the same `0.Y` line at or above it; from 1.0 the same major). `GET /api/v1/plugins/available` lists a plugin the server can't run with `incompatible` saying why; installing or upgrading to it answers `409`; an installed plugin whose minimum the server no longer meets stays stopped in status `error` with the reason. A server built without a release version runs anything.

### Changed

- A bundled plugin follows the server without an upgrade: at startup, a new bundled version that declares the same capabilities and configuration schema as the installed one is adopted (the row moves, the new binary runs). When either changed, the plugin stays stopped in status `error` with a reason naming the new version, and Upgrade moves it.

### Fixed

- `POST /api/v1/plugins/{id}/upgrade` no longer hangs when the new version's check reports a problem (the result was redacted under a lock that the redaction takes as well).
