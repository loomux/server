### Added

- `GET /api/v1/targets` answers `local_targets` (LOOM-183): whether this server allows targets of kind `local`. It is `false` under `LOOMUX_LOCAL_TARGETS=off` (the container image's default since LOOM-141), where registering a `local` target is a `400`, so a client can leave that choice out instead of offering it and failing. Additive; the targets list itself is unchanged.
