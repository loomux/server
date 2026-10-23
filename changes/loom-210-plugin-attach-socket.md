### Fixed

- A Docker or Kubernetes machine's attach command (`docker exec … tmux` / `kubectl exec … tmux` in attach-info) names the instance's tmux socket (`LOOMUX_TMUX_SOCKET`), where its sessions run, instead of always `loomux` (LOOM-210). On an instance with its own socket, the command used to reach an empty tmux server. The host now passes its socket in `targets.attach_commands`; plugins built before this keep naming `loomux`.
- Tests run tmux on sockets of their own and remove them afterwards, never on an instance's `loomux` socket (LOOM-210).
