# loomuxd

The Loomux server process. A thin wrapper around `app.Build` (the domain
layer — see `app/README.md`) and `api.NewServer` (the client-facing HTTP
surface — see `api/README.md`, including its auth design and known gaps).

```sh
# print the server's own release version and exit (design spec §10 axis 4)
loomuxd -version

# generate a bcrypt hash for LOOMUX_AUTH_PASSWORD_HASH (standalone; no
# other config needed)
echo -n 'my password' | loomuxd -hash-password

# default: start the HTTP server and block until SIGINT/SIGTERM
LOOMUX_ROUTER_PRIMARY_BASE_URL=... LOOMUX_ROUTER_PRIMARY_API_KEY=... LOOMUX_ROUTER_PRIMARY_MODEL=... \
  LOOMUX_AUTH_PASSWORD_HASH=... \
  loomuxd

# debugging only: dispatch one message directly, bypassing HTTP/auth entirely
loomuxd -message "hello"
```

Build a release with a real version baked in (see `version/README.md`):
`go build -ldflags "-X github.com/Loomux/server/version.Version=1.2.3" ./cmd/loomuxd`
— without it, `-version` prints `dev`.

`loomuxd` itself only ever speaks plain HTTP — the assumed deployment is
a reverse proxy (or an overlay network like Tailscale) in front
terminating TLS. See `api/README.md`'s Design section for why, and for
the other security/deployment decisions confirmed with the user before
this shipped.

`-conversation` pins the conversation ID used with `-message` (default: a
freshly generated one per process start).

## Configuration

All via environment variables:

```
# app.LoadConfig — the domain layer
LOOMUX_DB_PATH               SQLite file path (default: loomux.db)
LOOMUX_MARKER_DIR             completion-marker directory (default: completion's own package default)
LOOMUX_MASTER_KEY             base64 AES-256 key for the credential vault (optional; a startup
                               warning is printed if unset, since credential operations will then fail)
LOOMUX_REAP_IDLE_THRESHOLD    idle reaper threshold, time.ParseDuration syntax (default: 24h)
LOOMUX_REAP_INTERVAL          idle reaper sweep interval, time.ParseDuration syntax (default: 1h)
LOOMUX_TARGET_PROBE_INTERVAL  how often every target's health is probed (default: 5m; LOOM-86)
LOOMUX_LOG_LEVEL              debug | info | warn | error (default: info); structured JSON on stderr
                               for routing decisions, provisioning and dispatch (LOOM-63)

LOOMUX_ROUTER_PRIMARY_BASE_URL / _API_KEY / _MODEL       (required — see router/llmrouter)
LOOMUX_ROUTER_ESCALATION_BASE_URL / _API_KEY / _MODEL    (optional, all-or-nothing)

# api.LoadConfig — the HTTP/auth layer (only read in the default server mode)
LOOMUX_AUTH_PASSWORD_HASH    bcrypt hash of the single v1 user's password (required — see loomuxd -hash-password)
LOOMUX_HTTP_ADDR             address to listen on (default: :8080)
LOOMUX_SESSION_TTL           sliding-expiration window, time.ParseDuration syntax (default: 720h / 30 days)
```
