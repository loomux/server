### Added

- Plugin instances can be declared in the deployment (`LOOMUX_PLUGINS_SEED`, LOOM-219), as JSON or as the path of a JSON file. At startup, loomuxd logs the plan and then:
  - installs the instances the seed lists and nothing has;
  - reconfigures the ones the seed installed when the seed changes;
  - never touches an instance installed through the UI or API.
  Secrets are references to an environment variable or a file, never values. An instance the seed no longer lists is kept unless the seed says `"prune": true`, and is never pruned while it owns machines. A malformed seed stops startup; an entry that fails to apply is logged, and retried for a while when its plugin isn't there yet. `LOOMUX_PLUGINS_SEED_DRY_RUN=true` only logs the plan.
