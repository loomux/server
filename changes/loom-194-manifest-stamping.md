### Changed

- The bundled and sidecar plugin manifests carry the server release they were built with (LOOM-194): `version` is the release, `min_host_version` its `X.Y.0`, and the default `agent_image` is the agent image that release pins, as `tag@digest`. In the source tree the manifests say `0.0.0-dev`; a build stamps them (`plugins/manifeststamp`), so `GET /api/v1/plugins/available` on a release shows its version for the Kubernetes and Docker plugins instead of `0.1.0`.
- A sidecar plugin whose container was bumped is adopted at startup like a bundled one (same capabilities and schema shape), and on adoption or upgrade a setting still at the plugin's previous default follows its new default (the release's `agent_image` pin reaches installs that never changed it); a value you set stays.
