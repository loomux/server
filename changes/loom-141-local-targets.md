### Security

- Local targets are off in the container image (LOOM-141). An agent on a local target runs as loomuxd's own user, and so could read the database, the vault key and the SSH keys. With `LOOMUX_LOCAL_TARGETS=off` (the image's default; a bare binary defaults to `on`), registering a `local` target is refused with `400` and an existing one fails to run with a plain message. When they're on, local commands and agents get an allow-listed environment instead of loomuxd's (no `LOOMUX_*` secrets). See `docs/deploy/targets.md`, "Local targets".
