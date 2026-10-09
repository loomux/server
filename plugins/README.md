# plugins

The host side of Loomux's plugin system (LOOM-178,
`docs/design/target-providers.md` §1). A plugin is a separate program
loomuxd starts or connects to, described by a manifest, speaking
`loomux-plugin/1`: JSON-RPC 2.0 in `Content-Length` frames over the
subprocess's stdio or a unix socket. The host drives it; a plugin never
calls into loomuxd. In this first step the only method group every
plugin serves is `plugin.*` (describe, configure, check, shutdown); the
target-provider group (`targets.*`, machines on demand) is the next PR.

## Layout

- `manifest.go` — `Manifest` (`plugin.json`: name, version, protocol,
  capabilities, permissions, `config_schema`) and its validation; the
  JSON Schema subset the configuration schema may use, with
  `Validate`, `ApplyDefaults`, `SecretFields` and `Split`.
- `protocol/` — the protocol's vocabulary: method names and the
  parameter and result types of `plugin.*`. Imports nothing of Loomux,
  so the host, the SDK and any plugin share it.
- `rpc/` — the wire layer: frames of at most 1 MiB, `Conn` (the host's
  side: calls with deadlines, notifications) and `Serve` (a plugin's).
- `sdk/` — what a plugin's `main` calls: implement `Plugin`, hand it to
  `Main`; stdio with no arguments, a socket with `--listen` (the
  sidecar form).
- `fake/` + `cmd/loomux-plugin-fake` — the test plugin: scripted
  behaviour (crash, hang, exit after configure) and a check that reports
  any `LOOMUX_*` variable it can see. Not shipped.
- `catalog.go` — `Catalog`: plugins found in the bundle directory
  (`bundled`, trusted as the image is), the operator's plugin directory
  (`unsigned`, until a `Verifier` says otherwise) and the socket
  directory (sidecars, `isolation: container`).
- `instance.go` — `Instance`: one running plugin. A subprocess gets a
  clean environment (never `LOOMUX_*`), its stderr goes to the log, it
  is restarted with backoff and failed after three exits in five
  minutes; a socket is redialled while it is away. The handshake
  refuses a plugin whose describe doesn't match the manifest on disk.
- `manager.go` — `Manager`: install (schema-validated configuration,
  secrets encrypted at rest, the installed manifest kept on the row),
  get/list (secrets shown as set or not), reconfigure in place, enable,
  disable, upgrade, check, uninstall with an explicit choice for the
  plugin's machines, `StartAll` at boot, `Health`, two metrics.
- `plugintest/` — the conformance suite any plugin must pass.

Run `go test ./...` from the repo root. The package's own tests build
`cmd/loomux-plugin-fake` once and drive it as a real subprocess and over
a socket.
