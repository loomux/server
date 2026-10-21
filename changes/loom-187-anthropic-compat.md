### Changed

- An `openai` router tier on Anthropic's API host must name its
  OpenAI-compatible endpoint, `https://api.anthropic.com/v1`: any other
  base URL there fails every call. The router settings API now refuses
  one, and a `LOOMUX_ROUTER_*` environment tier with one stops start-up
  with the reason instead of failing on the first call. Such a tier also
  sends `max_completion_tokens`, which that endpoint needs (LOOM-187).
