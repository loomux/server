### Added

- The router can run on Claude through Anthropic's native Messages API
  (LOOM-186): set `LOOMUX_ROUTER_PRIMARY_PROVIDER=anthropic` (or
  `LOOMUX_ROUTER_ESCALATION_PROVIDER`), an API key and a model, e.g.
  `claude-haiku-4-5` for the primary and `claude-sonnet-5-5` for
  escalation; the base URL defaults to Anthropic's API. The provider
  defaults to `openai`, so existing configurations are unchanged. It works
  with current models that reject a forced tool choice: the tool is
  strict, the model is told to call it, and a reply without the call is
  retried once before it fails. The system prompt and tool are cached,
  routing runs at low effort where the model supports it, and a refusal
  escalates.

### Changed

- A relay answer that can't be used (no tool call, unparseable, empty
  reply) is now retried once on the same tier, told what was wrong,
  before escalating, as routing decisions already were (LOOM-186).
