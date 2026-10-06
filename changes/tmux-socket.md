### Added

- `LOOMUX_TMUX_SOCKET` names the tmux socket an instance runs its sessions
  on (default `loomux`), so a test and a production instance can drive the
  same targets without seeing, or sweeping, each other's sessions.
