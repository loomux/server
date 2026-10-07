### Added

- A per-target relay policy (`relay` on a target: `full`, `last_message`
  or `none`; empty means the purpose's default). It decides what the
  router models, third-party APIs, see of the target's work: the relay
  model gets everything, only the agent's final message, or nothing (the
  agent's own message is then the reply, and its session stays open);
  the routing model sees the target's conversation history, only its
  replies, or nothing, and no workspace summary under `none`. Under
  `none` a notification's body carries none of the target's work either:
  only its kind and workspace, and "Open Loomux to see it." Responses
  show `relay_effective`.

### Changed

- **Work machines (`purpose: work`) default to `none`**: nothing from
  them reaches the router models unless their `relay` is set otherwise.
  Personal machines keep `full`. On the test instance this includes
  sc1: its replies now arrive as the agent's raw final message (redacted,
  last 4000 characters) instead of a condensed summary, and its sessions
  stay open until the idle reaper closes them.
