### Added

- A machine can be created from chat (LOOM-216). While a machine plugin is running, the routing model may propose one with `provision_target`: a plugin, a name, and a size, whether it keeps its data, and egress from that plugin's `targets.describe`. Nothing is created on the proposal. Loomux resolves it against the plugin as `POST /api/v1/targets` would, shows the machine it would create (size with any requests, data, network, the plugin's machine count), and creates it only if the conversation's next message confirms. The usual offer rules apply, and the web card's kind is `create_target`. A yes creates nothing if the plugin's offer has changed since; a yes refused like that (or by the quota, a name taken meanwhile, or a failed create) leaves the card `denied` with the reason in the confirmation's new `status_reason`, not `approved`. Egress `trusted` is offered only for a name the plugin allows it: `targets.describe` takes an optional `name`, and a plugin implementing `sdk.NamedDescriber` (the Kubernetes plugin, for the names in `trusted_machines`) answers for it with `for_name`. With no machine plugin, a request for a machine is answered with how to get one.

### Fixed

- `GET /api/v1/plugins/{id}/targets` passes on a size's `cpu_request` and `memory_request` (LOOM-241), which it dropped.
