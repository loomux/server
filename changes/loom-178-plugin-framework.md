### Added

- The plugin framework (LOOM-178, first step of target providers,
  `docs/design/target-providers.md`): plugins are separate programs
  speaking `loomux-plugin/1` (JSON-RPC over stdio or a unix socket),
  found in the image's bundle directory, the operator's plugin
  directory (`LOOMUX_PLUGIN_DIR`, `plugins/` beside the database) or a
  sidecar's socket directory. `GET /api/v1/plugins/available` lists
  them with their permissions and trust; `POST /api/v1/plugins`
  installs one with a configuration validated against its manifest's
  schema (secret settings encrypted with `LOOMUX_MASTER_KEY`, never
  returned); `GET`, `PUT …/config`, `POST …/enable|disable|upgrade|check`
  and `DELETE /api/v1/plugins/{id}` manage it. Installed plugins start
  with the server; `GET /api/v1/health/deep` gains a `plugins`
  component, and `loomux_plugins` / `loomux_plugin_calls_total` are
  exported. No plugin ships yet: the Kubernetes and Docker plugins
  follow in their own releases. See `docs/deploy/plugins.md`.
