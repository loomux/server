### Changed

- Router resilience (LOOM-107): a routing answer that names an unknown
  workspace, target or agent (or isn't a usable tool call) gets one
  retry on the same model, told what was wrong, before escalating. When
  the primary model fails 3 times in a row (unreachable, HTTP error,
  timeout) it's skipped for 2 minutes and messages go straight to the
  escalation model, so an outage doesn't cost every message the
  primary's 15 s timeout. New metrics `loomux_router_retries_total` and
  `loomux_router_primary_breaker_open`.
