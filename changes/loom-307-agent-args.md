### Added

- A target can pin the model and effort each agent type is launched with
  (LOOM-307): `agent_args` on the target API, e.g.
  `{"claude-code": {"model": "opus", "effort": "high"}}`. Claude Code gets
  `--model`/`--effort`, Codex `-m` and `-c model_reasoning_effort`. Values
  are checked strictly (a model name, an effort its CLI takes); the router
  and prompts can't change them, and a pin the CLI can't take fails the
  launch instead of running on the account's defaults. attach-info shows
  the `agent_args` a task was launched with. Migration 00039.
