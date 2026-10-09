### Added

- `POST /api/v1/settings/router/{tier}/models` lists the models a router tier's provider offers (LOOM-191), for the Settings model picker: through the provider's own model list (OpenAI-compatible or Anthropic), with the tier's saved key, or with a key being entered for another provider or base URL, which is used for that call only and never stored or returned. A failure is reported by its status and error class, like the test call, and a listing is cached for five minutes.
