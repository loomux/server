### Added

- Managed targets (LOOM-138): a target registered with `generate_ssh_key`
  or an `ssh_key_id` is reached with that Loomux SSH key and no
  ssh_config. Its host key must be pinned before Loomux connects, and
  changing its host or port drops the pin. The key is served from an
  in-process agent. Connections go through `LOOMUX_SSH_PROXY`
  (`socks5://host:port`), relayed by loomuxd itself, unless the target
  sets `ssh_proxy: none`. Targets show `ssh_mode`, `ssh_key` (public
  parts), `ssh_proxy`, `ready` and `next_step`.
- `POST /targets/{id}/test` returns `steps`: which of connect, host key,
  authentication and tmux failed, and why.

### Fixed

- Creating or updating a target that names a missing SSH key says so
  (`400`), rather than reporting a duplicate name.
