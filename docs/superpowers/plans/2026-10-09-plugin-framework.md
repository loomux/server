# Plugin framework (LOOM-178 PR 1) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The host side of Loomux's plugin system: discover plugins (bundled, operator directory, sidecar
socket), install/configure/enable/disable/upgrade/check/uninstall them through `/api/v1/plugins`, run them as
supervised subprocesses or over a socket speaking `loomux-plugin/1` (JSON-RPC 2.0, Content-Length framing),
store their configuration with secrets encrypted, and give plugin authors an SDK, a fake plugin and a
conformance suite. No capability beyond `plugin.*` yet: `targets.*` is PR 2.

**Architecture:** `plugins/` is the host framework (manifest, catalog, trust, instance supervisor, manager);
`plugins/rpc` the wire codec both sides use; `plugins/sdk` what a plugin's `main` calls; `plugins/fake` +
`cmd/loomux-plugin-fake` the test plugin; `plugins/plugintest` the conformance suite. The registry gains
`plugins` and `settings` tables (migration `00028_plugins`). The API gains the `/plugins` routes of the
design's §5. `app` wires it; `GET /health/deep` gains a `plugins` component; two metrics.

**Tech Stack:** Go 1.27, stdlib only for the protocol (`encoding/json`, `os/exec`, `net`), `modernc.org/sqlite`
via the existing store, `goose` migrations, `prometheus/client_golang` via `internal/metrics`.

**Spec:** `docs/design/target-providers.md` (§1 whole, §5 "Plugins", §12 "Plugin conformance", "Host
framework").

## Global Constraints

- API v1 is additions-only: new routes are recorded with `go test ./api -run TestAPIv1Contract -update`; no
  existing line changes.
- Errors are `{error, code}` via `writeError`/`writeErrorCode`; ids are `id`, references `<thing>_id`; enums
  snake_case.
- Secrets never appear in any response, log line or error text (the LOOM-139 JSON-scan test pattern in
  `api/audit_loom139_test.go`).
- Encrypted at rest with the master key, `seal`/`open` in `registry/sqlite/crypto.go`, AAD bound to the row.
- Every subprocess gets a clean environment: never `LOOMUX_*`.
- Frame size limit 1 MiB; per-call deadline 30 s; `plugin.shutdown` then kill after 10 s; supervisor: three
  failures in five minutes → `error`.
- A changelog fragment `changes/loom-178-plugin-framework.md` under `### Added`.
- CI: `gofmt -l .` empty, `go vet ./...`, `go test -race -shuffle=on ./...`.

## Review Focus

1. A plugin that prints a non-JSON line to stdout before its first frame (a stray `fmt.Println`): the host
   must fail the handshake with a message naming the plugin, not hang. Test in Task 3 (`TestConnRejectsGarbage`).
2. A manifest on disk whose `name` differs from what `plugin.describe` returns: refused at install, the
   process stopped. Test in Task 6 (`TestInstanceRefusesManifestMismatch`).
3. `PUT /plugins/{id}/config` with a secret field omitted keeps the stored value; with `""` clears it; the
   response never carries it. Test in Task 8 (`TestSetPluginConfigKeepsOmittedSecret`).
4. Restart of loomuxd with an installed plugin whose binary is gone: the row goes to `error` with a reason,
   `GET /plugins` still lists it, nothing else fails. Test in Task 7 (`TestStartAllMissingBinary`).
5. A socket-source plugin that disappears while installed: calls fail `unavailable`, the instance reconnects
   when the socket is back. Test in Task 6 (`TestSocketInstanceReconnects`).

---

### Task 1: Registry: plugins and settings

**Files:**
- Create: `registry/sqlite/migrations/00028_plugins.sql`, `registry/plugin.go`, `registry/sqlite/plugin.go`
- Modify: `registry/store.go` (append methods), `registry/storetest/storetest.go` (add cases),
  `registry/sqlite/sqlite_test.go` (nothing: `storetest.Run` already runs)
- Test: `registry/storetest/plugin.go` (new file with `testPlugin*`)

**Interfaces:**
- Produces:

```go
// registry/plugin.go
type PluginStatus string
const (
    PluginStatusInstalling PluginStatus = "installing"
    PluginStatusInstalled  PluginStatus = "installed"
    PluginStatusDisabled   PluginStatus = "disabled"
    PluginStatusError      PluginStatus = "error"
)
type PluginSource string
const (
    PluginSourceBundled PluginSource = "bundled"
    PluginSourceDir     PluginSource = "dir"
    PluginSourceSocket  PluginSource = "socket"
)
type Plugin struct {
    ID, Name, Label, Version, Protocol string
    Source PluginSource
    Path   string
    Trust  string            // "bundled" | "unsigned" | "signed:<identity>"
    Status PluginStatus
    StatusReason string
    Enabled bool
    Capabilities []string
    // Config is the non-secret configuration; Secrets the secret fields, plaintext at this level,
    // encrypted at rest (AAD "plugin:<id>"). Only GetPlugin fills Secrets; ListPlugins leaves it nil.
    Config  map[string]any
    Secrets map[string]string
    InstalledAt, UpdatedAt time.Time
}
// Store additions
CreatePlugin(ctx, p *Plugin) error                 // ErrConflict on a duplicate label; ErrNoMasterKey if Secrets non-empty and no key
GetPlugin(ctx, id string) (*Plugin, error)         // decrypts Secrets; a row that doesn't decrypt returns the plugin with Secrets nil and ErrNoMasterKey joined
ListPlugins(ctx) ([]*Plugin, error)               // ordered by label; no decryption
UpdatePlugin(ctx, p *Plugin) error                 // status, reason, enabled, version, protocol, capabilities, path, trust; not config
SetPluginConfig(ctx, id string, config map[string]any, secrets map[string]string) error
DeletePlugin(ctx, id string) error
GetSetting(ctx, key string) (string, error)        // ErrNotFound
SetSetting(ctx, key, value string) error           // upsert
```

- [ ] **Step 1: Write the migration**

```sql
-- LOOM-178: installed plugins (docs/design/target-providers.md §1.7) and
-- server settings (the instance id). secrets is the plugin's secret
-- configuration fields as one JSON object, AES-256-GCM under the master
-- key with "plugin:<id>" as additional data; config is the rest, plain.
-- +goose Up
CREATE TABLE plugins (
    id            TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    label         TEXT NOT NULL UNIQUE,
    version       TEXT NOT NULL,
    protocol      TEXT NOT NULL,
    source        TEXT NOT NULL CHECK (source IN ('bundled','dir','socket')),
    path          TEXT NOT NULL,
    trust         TEXT NOT NULL,
    status        TEXT NOT NULL CHECK (status IN ('installing','installed','disabled','error')),
    status_reason TEXT NOT NULL DEFAULT '',
    enabled       INTEGER NOT NULL DEFAULT 1,
    capabilities  TEXT NOT NULL DEFAULT '[]',
    config        TEXT NOT NULL DEFAULT '{}',
    secrets       BLOB,
    installed_at  TIMESTAMP NOT NULL,
    updated_at    TIMESTAMP NOT NULL
);
CREATE TABLE settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
-- +goose Down
DROP TABLE settings;
DROP TABLE plugins;
```

- [ ] **Step 2: Write the failing storetest cases** in `registry/storetest/plugin.go`: `testPluginCRUD`
  (create with secrets → Get returns them, List doesn't; Update changes status; Delete → ErrNotFound),
  `testPluginDuplicateLabel` (ErrConflict), `testPluginConfigRoundTrip` (SetPluginConfig replaces both maps;
  nested values survive JSON), `testPluginSecretsNeedMasterKey` (a store opened without a key: Create with
  secrets → ErrNoMasterKey; without secrets ok), `testSettings` (Get → ErrNotFound; Set; Get; Set again
  overwrites). Register them in `Run`. The master-key case needs `newStore` variants: `storetest.Run` is
  called by `sqlite_test.go` with a keyed store; add `RunWithoutMasterKey(t, newStore)` exposing just the
  no-key case, called from `sqlite_test.go` with a key-less `Open`.
- [ ] **Step 3: Run** `go test ./registry/... -run Plugin -v` → FAIL (methods missing).
- [ ] **Step 4: Implement** `registry/plugin.go` (types above) and `registry/sqlite/plugin.go`: `pluginAAD(id)
  = []byte("plugin:"+id)`; `CreatePlugin` seals `json.Marshal(p.Secrets)` when non-empty (nil blob when
  empty), maps the UNIQUE violation on label to `ErrConflict` (see how `CreateTarget` does it);
  `GetPlugin` opens the blob, joins `ErrNoMasterKey`/decrypt errors like `ListRouterTiers`; `UpdatePlugin`
  updates the listed columns and `updated_at`, `requireRowAffected`; `SetPluginConfig` writes `config`,
  `secrets`, `updated_at`; `GetSetting`/`SetSetting` with `ON CONFLICT (key) DO UPDATE`.
- [ ] **Step 5: Run** `go test ./registry/...` → PASS. `gofmt -l .` empty.
- [ ] **Step 6: Commit** `LOOM-178: registry: plugins and settings tables`.

### Task 2: Manifest

**Files:**
- Create: `plugins/manifest.go`, `plugins/manifest_test.go`, `plugins/testdata/manifest-ok.json`

**Interfaces:**
- Produces:

```go
package plugins
const ProtocolV1 = "loomux-plugin/1"
type Permission struct { Scope string `json:"scope"`; Detail string `json:"detail"` }
type Manifest struct {
    Name, Title, Version, Protocol, Vendor, Homepage, Description, MinHostVersion string // json: snake_case
    Capabilities []string         `json:"capabilities"`
    Permissions  []Permission     `json:"permissions"`
    HostRequests []string         `json:"host_requests"`
    ConfigSchema json.RawMessage  `json:"config_schema"`
}
func ParseManifest(data []byte) (*Manifest, error)   // parse + Validate
func (m *Manifest) Validate() error                   // name ^[a-z][a-z0-9-]{0,31}$; version non-empty; protocol major "1"; host_requests empty; capabilities from KnownCapabilities; schema parses
func (m *Manifest) ProtocolMajor() (int, error)
var KnownCapabilities = []string{"targets.create", "targets.stop_start", "targets.recreate", "targets.persistent", "targets.ephemeral", "targets.egress_policy", "targets.attach_commands"}
type Schema struct { /* the subset: type object, properties (string|integer|boolean, enum, default, x-secret, x-format, description), required, one nested level */ }
func (m *Manifest) Schema() (*Schema, error)
func (s *Schema) Validate(config map[string]any) error       // required present, types match, enums, unknown keys refused; message names the field
func (s *Schema) SecretFields() []string                     // x-secret: true, sorted
func (s *Schema) Split(config map[string]any) (plain map[string]any, secrets map[string]string)
func (s *Schema) ApplyDefaults(config map[string]any) map[string]any
```

- [ ] **Step 1: Tests** (`manifest_test.go`): parse the ok fixture; each Validate failure (bad name, protocol
  `loomux-plugin/2`, unknown capability, non-empty host_requests, schema not an object); `Schema.Validate`:
  missing required, wrong type, enum, unknown key, nested object ok; `SecretFields` sorted; `Split` moves
  secrets out and stringifies them; `ApplyDefaults` fills absent keys only.
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.** **Step 4: Run** → PASS. **Step 5: Commit**
  `LOOM-178: plugins: manifest and configuration schema`.

### Task 3: Wire codec and JSON-RPC connection

**Files:**
- Create: `plugins/rpc/frame.go`, `plugins/rpc/conn.go`, `plugins/rpc/server.go`, `plugins/rpc/rpc_test.go`

**Interfaces:**
- Produces:

```go
package rpc
const MaxFrameBytes = 1 << 20
func WriteFrame(w io.Writer, payload []byte) error            // "Content-Length: N\r\n\r\n" + payload
func ReadFrame(r *bufio.Reader) ([]byte, error)               // ErrFrameTooLarge, ErrBadFrame (no header / junk before it)
type Error struct { Code string; Message string }             // Code from: invalid_config, unauthorized, not_found, quota, unavailable, internal, method_not_found, invalid_params
func (e *Error) Error() string
type Conn struct{ ... }                                       // client side
func NewConn(r io.Reader, w io.Writer, onNotify func(method string, params json.RawMessage)) *Conn
func (c *Conn) Call(ctx context.Context, method string, params, result any) error   // ctx deadline; *Error on a JSON-RPC error; ErrClosed after Close
func (c *Conn) Close() error
// server side (the SDK uses it)
type Handler func(ctx context.Context, method string, params json.RawMessage) (any, *Error)
func Serve(ctx context.Context, r io.Reader, w io.Writer, h Handler) error    // one goroutine per request; stops on EOF/ctx
```

JSON-RPC mapping: numeric string `code` isn't used; the error object is `{"code": -32000, "message": "...",
"data": {"code": "unavailable"}}`; method not found is `-32601` → `method_not_found`; invalid params
`-32602` → `invalid_params`. Requests carry `"jsonrpc":"2.0"`, integer ids; notifications have no id.

- [ ] **Step 1: Tests**: frame round trip; `ReadFrame` on oversize → `ErrFrameTooLarge`; on `hello\n` →
  `ErrBadFrame` (`TestConnRejectsGarbage` drives a `Conn` over a pipe whose peer writes junk: `Call` returns
  `ErrBadFrame` and the conn is closed); `Call` round trip through `Serve` over `io.Pipe`; two concurrent
  calls get their own results; a `Serve` handler returning `&Error{Code:"quota"}` surfaces as `*Error`;
  unknown method → `method_not_found`; ctx deadline → `context.DeadlineExceeded` and a later reply is
  dropped; notifications reach `onNotify`; `Close` fails pending calls with `ErrClosed`.
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement** (a reader goroutine demuxing by id into per-call channels;
  writes serialised by a mutex). **Step 4: Run** `-race` → PASS. **Step 5: Commit** `LOOM-178: plugins/rpc:
  Content-Length framing and JSON-RPC 2.0`.

### Task 4: SDK, the fake plugin, its binary

**Files:**
- Create: `plugins/sdk/sdk.go`, `plugins/sdk/sdk_test.go`, `plugins/fake/fake.go`, `plugins/fake/fake_test.go`,
  `cmd/loomux-plugin-fake/main.go`, `plugins/fake/plugin.json`

**Interfaces:**
- Import rule (no cycles): `plugins/protocol` is a leaf package holding the method names and their
  param/result types; `plugins/rpc` the codec; `plugins` (host) imports `protocol` and `rpc`, never `sdk`;
  `sdk` imports `plugins` (for `Manifest`), `protocol` and `rpc`. Host tests that need the fake served
  in-process are external tests (`package plugins_test`).
- Produces:

```go
package protocol
const (MethodDescribe = "plugin.describe"; MethodConfigure = "plugin.configure"; MethodCheck = "plugin.check"; MethodShutdown = "plugin.shutdown")
type ConfigureParams struct { Config map[string]any `json:"config"`; Host HostInfo `json:"host"` }
type HostInfo struct { Version, InstanceID, DataDir string }     // json snake_case
type Problem struct { Code, Message, Severity string }           // severity "warning" | "error"
type CheckResult struct { OK bool `json:"ok"`; Problems []Problem `json:"problems"` }

package sdk
type Plugin interface {
    Describe(ctx) (*plugins.Manifest, error)
    Configure(ctx, ConfigureParams) error
    Check(ctx) (CheckResult, error)
    Shutdown(ctx) error
}
func Serve(ctx context.Context, p Plugin, r io.Reader, w io.Writer) error         // dispatches plugin.* ; other methods → method_not_found
func Main(p Plugin)   // flags: --listen <socket path>; no flag = stdio; SIGTERM → Shutdown; exit codes 0 / 2 on a bad flag
```

`fake.New()` returns a `Plugin` with manifest name `fake`, capabilities none, config schema
`{"mode": enum [ok, crash_on_check, hang_on_check, exit_after_configure], "token": {x-secret}, "greeting":
{default "hi"}}`; `Check` returns `ok` plus a problem `env_leak` listing every env var whose name starts with
`LOOMUX_` (severity error), and a problem `token_set` when the token is non-empty (so tests see secrets
arrived without printing them); `mode` controls crash (`os.Exit(3)`), hang (block until ctx done), exit after
configure.

- [ ] **Step 1: Tests**: `sdk.Serve` over pipes answers `plugin.describe` with the manifest, routes
  `configure`/`check`/`shutdown`, unknown → `method_not_found`; `fake` behaviours (hang respects ctx; check
  lists `LOOMUX_X` when set in the test's own env via `t.Setenv`).
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.** **Step 4: Run** → PASS; `go build ./cmd/loomux-plugin-fake`.
  **Step 5: Commit** `LOOM-178: plugins/sdk and the fake plugin`.

### Task 5: Catalog and trust

**Files:**
- Create: `plugins/catalog.go`, `plugins/trust.go`, `plugins/catalog_test.go`

**Interfaces:**
- Produces:

```go
type Available struct {
    Manifest  Manifest
    Source    registry.PluginSource
    Path      string   // the plugin's directory, or the socket path
    Trust     string   // TrustBundled | TrustUnsigned (signed:<identity> arrives with PR 3's verifier)
    Isolation string   // "none" for a subprocess, "container" for a socket
}
const TrustBundled, TrustUnsigned = "bundled", "unsigned"
type Catalog struct { BundleDir, PluginDir, SocketDir string; Dial func(ctx, path) (*rpc.Conn, error) }
func (c *Catalog) Available(ctx) ([]Available, error)   // dirs: each subdir with plugin.json + executable named loomux-plugin-<name>; sockets: *.sock, describe with a 5 s deadline; unreadable entries are logged and skipped, never fail the listing
func (c *Catalog) Find(ctx, name string, source registry.PluginSource) (*Available, error)   // ErrNotAvailable
type Verifier interface { Verify(ctx, dir string) (trust string, err error) }   // the PR 3 seam; nil means unsigned
```

- [ ] **Step 1: Tests** with temp dirs: a bundled plugin → `bundled`/`none`; a dir plugin → `unsigned`; a
  socket (the fake served by `sdk.Serve` on a unix listener in the test) → `unsigned`/`container` with the
  manifest from describe; a dir without an executable is skipped with a log; a manifest that fails Validate is
  skipped; `Find` with the wrong source → `ErrNotAvailable`.
- [ ] **Step 2–5:** FAIL → implement → PASS → commit `LOOM-178: plugins: catalog over bundle, dir and socket sources`.

### Task 6: Instance supervisor

**Files:**
- Create: `plugins/instance.go`, `plugins/instance_test.go` (`TestMain` builds `cmd/loomux-plugin-fake` into a temp dir once)

**Interfaces:**
- Produces:

```go
type Instance struct{ ... }   // one running plugin: subprocess or socket
type InstanceOptions struct {
    Label string; Source registry.PluginSource; Path string; Manifest *Manifest
    Config map[string]any            // merged plain+secrets, passed to configure
    Host sdk.HostInfo
    Logger *slog.Logger; Metrics *metrics.Metrics
    CallTimeout time.Duration        // default 30 s
}
func Start(ctx context.Context, o InstanceOptions) (*Instance, error)   // spawn or dial; describe (manifest must equal o.Manifest: name, version, protocol); configure; returns; ErrManifestMismatch
func (i *Instance) Check(ctx) (sdk.CheckResult, error)
func (i *Instance) Call(ctx, method string, params, result any) error   // "unavailable" while restarting/disconnected
func (i *Instance) Status() InstanceStatus                             // Running | Restarting | Failed{Reason}
func (i *Instance) Stop(ctx) error                                     // shutdown, then kill after 10 s
```

Subprocess: `exec.Command(path/loomux-plugin-<name>)`, `Env = []string{"PATH=...","HOME=...","TMPDIR=...", "LANG=..."}` from the host's own values, never `LOOMUX_*`; stderr piped to the logger line by line with `plugin=<label>`; on exit: restart with backoff 1 s, 2 s, 4 s; three exits within five minutes → `Failed` with the last stderr line as reason. Socket: dial; on error mark disconnected and retry every 2 s; calls meanwhile return `&rpc.Error{Code:"unavailable"}`.

- [ ] **Step 1: Tests** (against the built fake binary): start+configure+check ok and the check's `env_leak`
  problem is empty (the test sets `LOOMUX_SECRET=x` in its own env first); `TestInstanceRefusesManifestMismatch`
  (an `InstanceOptions.Manifest` with another version → `ErrManifestMismatch`, process gone); mode
  `exit_after_configure` → status `Restarting` then `Failed` after three; `hang_on_check` with a 200 ms
  `CallTimeout` → `DeadlineExceeded`; `Stop` returns within 11 s with the process gone;
  `TestSocketInstanceReconnects` (serve the fake on a socket, start, close the listener → `Call` → unavailable,
  serve again → `Call` ok within 5 s).
- [ ] **Step 2–5:** FAIL → implement → PASS (`-race`) → commit `LOOM-178: plugins: instance supervisor`.

### Task 7: Manager

**Files:**
- Create: `plugins/manager.go`, `plugins/manager_test.go`

**Interfaces:**
- Produces:

```go
type Manager struct{ ... }
func NewManager(store registry.Store, catalog *Catalog, host sdk.HostInfo, logger *slog.Logger, met *metrics.Metrics) *Manager
type InstallRequest struct { Name string; Source registry.PluginSource; Label string; Config map[string]any }
type View struct { registry.Plugin; AvailableVersion string; Check *sdk.CheckResult; Instance InstanceStatus; Machines int }  // Secrets always nil; Config secrets replaced by map[string]any{"set": true/false}
func (m *Manager) Available(ctx) ([]Available, error)
func (m *Manager) Install(ctx, r InstallRequest) (*View, error)      // ErrNotAvailable, ErrInvalidConfig{Field, Msg}, registry.ErrConflict, registry.ErrNoMasterKey, ErrCheckFailed{Result}
func (m *Manager) Get(ctx, id) (*View, error); List(ctx) ([]*View, error)
func (m *Manager) SetConfig(ctx, id, config map[string]any) (*View, error)   // omitted secrets keep stored values; "" clears
func (m *Manager) Enable(ctx, id) (*View, error); Disable(ctx, id) (*View, error)
func (m *Manager) Upgrade(ctx, id) (*View, error)                    // catalog version != installed: stop, start new, describe/check; on failure keep old row status error
func (m *Manager) Check(ctx, id) (*View, error)
func (m *Manager) Uninstall(ctx, id string, targets string) error      // targets "" | "destroy" | "keep"; ErrHasMachines{N} when machines > 0 and targets == "" (always 0 in PR 1; MachineCounter is a func field defaulting to zero, PR 2 sets it)
func (m *Manager) StartAll(ctx) error                                 // at boot; each failure → row error, logged, continue
func (m *Manager) Health(ctx) health.ComponentResult                   // healthy if every enabled plugin's instance is Running and its last check ok; degraded otherwise with details
func (m *Manager) Close(ctx) error
var ErrNotAvailable, ErrHasMachines, ErrCheckFailed, ErrInvalidConfig ...
```

Install flow: Find → `Schema.Validate(ApplyDefaults(config))` → `Split` → `CreatePlugin(status installing)` →
`Start` → `Check` → status installed (ok) or `error` with the problems' messages joined, row kept, instance
stopped. Metrics: `PluginsByStatus` gauge (`loomux_plugins{plugin,status}`), `PluginCalls` counter
(`loomux_plugin_calls_total{plugin,method,outcome}`) added to `internal/metrics` in this task.

- [ ] **Step 1: Tests** (store from `sqlite.Open` with a master key; catalog over a temp bundle dir holding the
  fake): install → view installed, `Config["token"] == map{"set": true}`, no `Secrets`; install with an
  unknown key → `ErrInvalidConfig`; duplicate label → ErrConflict; `TestStartAllMissingBinary` (install,
  Close, delete the binary, new Manager, StartAll → row `error`, List works); disable → instance stopped,
  enable → running; SetConfig with token omitted keeps it (the fake's `token_set` problem still reported);
  with `""` clears it; Upgrade with the same version is a no-op; uninstall with `targets=""` and no machines
  → row gone, instance stopped; a JSON-marshal of every `View` contains no token value (string scan).
- [ ] **Step 2–5:** FAIL → implement → PASS → commit `LOOM-178: plugins: manager (install, configure, lifecycle)`.

### Task 8: API routes

**Files:**
- Create: `api/plugins.go`, `api/plugins_test.go`
- Modify: `api/server.go` (routes, `WithPlugins(Option)`, field), `api/contract_test.go` (`apiV1Bodies`),
  `api/testdata/api-v1-contract.txt` (`-update`), `api/README.md` (a "Plugins" section)

**Interfaces:**
- Consumes: `plugins.Manager` through an interface `Plugins` declared in `api/plugins.go` (Available, Install,
  Get, List, SetConfig, Enable, Disable, Upgrade, Check, Uninstall).
- Produces the routes of the design §5 with these bodies: `listAvailablePluginsResponse{Plugins []availablePluginResponse}`,
  `listPluginsResponse{Plugins []pluginResponse}`, `installPluginRequest{Plugin, Source, Label string; Config map[string]any}`,
  `pluginResponse{ID, Name, Label, Version, AvailableVersion, Protocol, Source, Path, Trust, Status, StatusReason string; Enabled bool; Capabilities []string; Config map[string]any; Check *checkResponse; Machines int; InstalledAt, UpdatedAt time.Time}`,
  `setPluginConfigRequest{Config map[string]any}`, `checkResponse{OK bool; Problems []problemResponse}`.
  Status mapping: ErrNotAvailable 404, ErrInvalidConfig 400 (message names the field), ErrConflict 409,
  ErrNoMasterKey 503 `"plugin configuration needs LOOMUX_MASTER_KEY"`, ErrCheckFailed 422 with the problems,
  ErrHasMachines 409, unknown `targets` 400. Without `WithPlugins` the routes answer 404 like router settings.

- [ ] **Step 1: Tests** (`newTestServerWith` + a Manager over the fake in a temp bundle dir): each route's
  happy path and the status mapping above; `TestSetPluginConfigKeepsOmittedSecret`; the LOOM-139 scan: every
  plugin route's JSON never contains the token value; unauthenticated → 401; contract regenerated.
- [ ] **Step 2–5:** FAIL → implement → `go test ./api -run TestAPIv1Contract -update` → PASS all → commit
  `LOOM-178: /api/v1/plugins`.

### Task 9: App wiring, image, docs

**Files:**
- Modify: `app/config.go` (`LOOMUX_PLUGIN_BUNDLE_DIR`, `LOOMUX_PLUGIN_DIR` default `<db dir>/plugins`,
  `LOOMUX_PLUGIN_SOCKET_DIR`), `app/app.go` (instance id from `settings`, Manager, StartAll in `bg`, Close),
  `cmd/loomuxd/main.go` (`api.WithPlugins`), `internal/health/health.go` (`WithPluginHealth(func(ctx) ComponentResult)` option; `Deep` adds component `plugins` in parallel with the sidecar), `Dockerfile` (the two env defaults; `mkdir /usr/local/lib/loomux/plugins /run/loomux/plugins` owned by loomux), `docs/deploy/container.md` (the variables), `docs/design/core-design.md` (§1 paragraph on plugins, §7 note), `CLAUDE.md` status line
- Create: `plugins/README.md`, `docs/deploy/plugins.md`, `changes/loom-178-plugin-framework.md`
- Test: `app/plugins_test.go` (Build with a bundle dir holding the fake → `GET /health/deep` has `plugins`; instance id persists across two Builds on the same DB)

- [ ] **Step 1: Tests. Step 2: FAIL. Step 3: Implement. Step 4: PASS. Step 5: Commit** `LOOM-178: wire plugins into the app, health and the image; docs`.

### Task 10: Conformance suite and the final pass

**Files:**
- Create: `plugins/plugintest/plugintest.go`, `plugins/plugintest/plugintest_test.go` (runs it against the fake)

**Interfaces:**
- Produces: `func Run(t *testing.T, launch func(t *testing.T) (*rpc.Conn, func()))`: describe is a valid
  manifest; describe twice is equal; configure with the schema's defaults ok; configure with an unknown key
  → `invalid_config`; check returns a well-formed result; unknown method → `method_not_found`; a 2 MiB frame
  is refused and the connection survives a following valid call (or closes cleanly: both acceptable, assert
  no hang); shutdown returns.

- [ ] **Step 1: Write the suite and its test. Step 2: PASS. Step 3:** `gofmt -l .`, `go vet ./...`,
  `go test -race -shuffle=on ./...`, `sh deploy/changelog.sh check`. **Step 4: Commit**
  `LOOM-178: plugintest conformance suite`. **Step 5:** push, open the PR titled "LOOM-178: plugin
  framework (PR 1)" with the design link and "Tracked in Vikunja Loomux #145, Plane LOOM-178".
