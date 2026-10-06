### Added

- An agent at its usage limit fails the turn with class
  `agent_rate_limited` and says when the limit resets ("agent
  "claude-code" on sc1 hit its usage limit, resets 5pm
  (Europe/Istanbul)") instead of the error being relayed as an answer
  (LOOM-109). Detected for Claude Code and Codex while the turn runs and
  on its final screen.

### Fixed

- A dispatch job whose agent isn't signed in now records
  `login_required` (it recorded `internal`), so the web client shows its
  sign-in hint.

