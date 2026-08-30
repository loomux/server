# loomuxd

The Loomux server process. A thin CLI wrapper around `app.Build` — see
`app/README.md` for the actual composition/wiring design.

No network surface exists yet (login-gated auth and the versioned
HTTP/WS client API are LOOM-9, not yet built), so this binary's
"minimal internal interface" is local only:

```sh
# single message, print the reply, exit
LOOMUX_ROUTER_PRIMARY_BASE_URL=... LOOMUX_ROUTER_PRIMARY_API_KEY=... LOOMUX_ROUTER_PRIMARY_MODEL=... \
  loomuxd -message "hello"

# interactive: one chat message per line on stdin, one reply per line on stdout
loomuxd
```

`-conversation` pins the conversation ID used for every dispatched
message in that run (default: a freshly generated one per process
start).

## Configuration

All via environment variables (see `app.LoadConfig`):

```
LOOMUX_DB_PATH               SQLite file path (default: loomux.db)
LOOMUX_MARKER_DIR             completion-marker directory (default: completion's own package default)
LOOMUX_MASTER_KEY             base64 AES-256 key for the credential vault (optional; a startup
                               warning is printed if unset, since credential operations will then fail)

LOOMUX_ROUTER_PRIMARY_BASE_URL / _API_KEY / _MODEL       (required — see router/llmrouter)
LOOMUX_ROUTER_ESCALATION_BASE_URL / _API_KEY / _MODEL    (optional, all-or-nothing)
```
