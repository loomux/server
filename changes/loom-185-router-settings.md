### Added

- Router model settings (LOOM-185, `docs/design/router-settings.md`):
  `GET /api/v1/settings/router`, `PUT` and `DELETE
  /api/v1/settings/router/{tier}`, `POST .../{tier}/test` and `GET
  /api/v1/settings/router/audit` set, rotate, test and remove each
  tier's provider (`openai` or `anthropic`), base URL, model and API key
  without editing secrets or redeploying. A stored tier overrides `LOOMUX_ROUTER_<TIER>_*`, which
  stays the bootstrap and the fallback, and applies to the running
  router at once. Every change is audited (tier, fields changed, actor,
  time).

### Security

- Router API keys set through Settings are write-only (responses show a
  fingerprint and the last four characters only), encrypted at rest
  with `LOOMUX_MASTER_KEY` bound to their tier, and, like the
  environment's router keys, removed from agent output, relay input and
  every other scrubbed text (LOOM-185). A saved key is never sent to a
  new provider or base URL without being entered again, stored base
  URLs must be https (http only to loopback), and a test call reports
  only the provider's status and an error class, never its response.
