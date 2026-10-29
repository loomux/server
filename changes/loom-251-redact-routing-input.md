### Security

- Vault values are redacted from what the routing model gets (the
  message and the conversation so far) and from the earlier-conversation
  note a newly started agent gets (LOOM-251). The agent still gets the
  message as typed. If the vault can't be read, the routing model gets
  no history, the agent no note, and the message isn't routed.

### Changed

- Without `LOOMUX_MASTER_KEY`, chat messages can no longer be routed:
  the vault can't be read to redact them.
