### Added

- A plugin's `min_host_version` is enforced (LOOM-194). The server refuses a plugin below that minimum or on another major (from 1.0): `GET /api/v1/plugins/available` lists it with `incompatible` saying why, installing or upgrading to it answers `409`, and an installed one stays stopped in status `error` with the reason. A plugin built for an older `0.Y` than the server's is listed with `incompatible` and a logged warning, but installs and runs. A server built without a release version runs anything.

### Changed

- A bundled plugin follows the server without an upgrade: at startup, a new bundled version that declares the same capabilities and configuration schema as the installed one is adopted (the row moves, the new binary runs). When either changed, the plugin stays stopped in status `error` with a reason naming the new version, and Upgrade moves it.

### Fixed

- `POST /api/v1/plugins/{id}/upgrade` no longer hangs when the new version's check reports a problem (the result was redacted under a lock that the redaction takes as well).
