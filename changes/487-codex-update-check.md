### Fixed

- Codex tasks no longer stop at Codex's "Update available" startup menu, whose option runs `curl … | sh` on the target (LOOM-260, #487). Every launch passes `-c check_for_update_on_startup=false` for that process, so a pinned CLI updates only with its image or target provisioning; `~/.codex/config.toml` is untouched.
