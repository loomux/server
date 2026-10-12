### Changed

- A router tier's saved API key is bound to its provider and base URL as well as its tier (LOOM-189): a row whose provider or base URL is changed in the database no longer decrypts. The first start with the master key re-seals tiers saved before this; a release from before can't read them (save them again in Settings after rolling back).
- `/api/v1/health` checks the router configuration in force, the one Settings may have replaced since startup, judged as the router does: an Anthropic tier without a base URL is healthy.

### Fixed

- The router's primary circuit breaker drops a call's outcome reported after the primary was replaced, under the breaker's own lock; before, one such failure could still land on the new primary's count.
