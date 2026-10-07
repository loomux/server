### Added

- Target onboarding through the API (LOOM-114): `POST
  /api/v1/targets/{id}/scan-host-key` reads a target's host key without
  trusting it, `POST /api/v1/targets/{id}/pin {fingerprint}` pins a key
  from that scan (the target is then checked against its pin alone,
  ahead of the mounted `known_hosts`), `DELETE …/pin` unpins, and `POST
  …/test` checks SSH and tmux and flags a host key problem. Targets take
  an `ssh_port` override. The host-key-changed and unknown hints say how
  to re-pin.
