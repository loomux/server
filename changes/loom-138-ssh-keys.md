### Added

- Loomux-managed SSH keys (LOOM-138, first step of in-app target
  onboarding): `GET/POST /api/v1/ssh-keys` and `DELETE
  /api/v1/ssh-keys/{id}` generate ed25519 keys and show their public
  parts, the `authorized_keys` line among them. Private keys are encrypted
  with `LOOMUX_MASTER_KEY` and never returned. Targets don't connect with
  them yet.

### Changed

- A target's stored `ssh_key_ref`, unused and gone from the API since the
  v1 freeze, is cleared by migration 23; it now names a managed key.
