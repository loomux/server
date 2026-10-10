### Added

- The Docker target-provider plugin (LOOM-180, `plugins/docker`): a
  container per machine on one Docker host, reached over SSH at a port
  published on the host's address. The plugin reaches the engine over
  SSH with its own key and a pinned host key (the first check scans and
  reports the key to trust), through a SOCKS5 proxy when the host is
  only on the tailnet, or through the engine's local socket; it never
  holds loomuxd's keys. Machines run hardened (no capabilities, no
  privilege escalation, read-only root, pids and memory limits,
  `--init`) with sshd's files in a read-only volume and a persistent
  machine's data in another; the published port is fixed for the
  machine's life. Docker offers no `egress` choice: it publishes no
  port on an internal network, and the LAN stays reachable. Bundled in
  the server image and published as `ghcr.io/loomux/plugin-docker`
  (the sidecar form). CI runs it against the runner's own engine, over
  its socket and over SSH.
