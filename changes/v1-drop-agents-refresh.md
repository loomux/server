### Removed

- `POST /api/v1/targets/{id}/agents/refresh` (API v1 freeze review):
  `POST /api/v1/targets/{id}/probe` re-probes the agent CLIs too, with
  the target's health, and returns both.
