### Added

- A per-target relay policy (`relay` on a target: `full`, `last_message`
  or `none`; empty means the purpose's default). It decides what the
  router models, third-party APIs, see of the target's work: the relay
  model gets everything, only the agent's final message, or nothing (the
  agent's own message is then the reply, and its session stays open);
  the routing model sees the target's conversation history, only its
  replies, or nothing, and no workspace summary under `none`. Responses
  show `relay_effective`.

### Changed

- **Work machines (`purpose: work`) default to `none`**: nothing from
  them reaches the router models unless their `relay` is set otherwise.
  Personal machines keep `full`.
