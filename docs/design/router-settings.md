# Router settings: provider keys and models from the API (LOOM-185)

Status: implemented — the server half in loomux/server, the Settings UI
(specified below) in loomux/web.

## Problem

The router model's two tiers (`router/llmrouter`, design spec §6) are
configured only from the environment:

```
LOOMUX_ROUTER_{PRIMARY,ESCALATION}_{BASE_URL,API_KEY,MODEL}
```

Rotating a key or trying another model means editing the deployment's
secrets and redeploying. The owner should be able to do it from
Settings, with the key treated like every other secret Loomux holds.

## Decisions

**Stored per tier, env is the fallback.** A tier (`primary`,
`escalation`) is either *stored* — set through the API, in the database —
or comes from the environment as today. A stored tier wins over the env
for that tier, wholly (provider, base URL, model and key together: a
tier is never half env, half stored). Deleting the stored tier falls back
to the env again. `LOOMUX_ROUTER_PRIMARY_*` stays required at startup:
it is the bootstrap that gets a fresh server running before anyone can
log in to Settings, and the fallback if a stored tier is removed or can't
be read. Escalation stays optional in both places.

**Write-only keys.** The key goes in with `PUT` and never comes out: no
response field can hold it. Responses carry `key_fingerprint`
(`sha256:` + the first 12 hex digits of SHA-256 of the key) and
`key_last4` (the last four characters, only for keys of 16 characters
or more, so a short test key isn't mostly revealed), plus `set_at`.
The same shape describes an env tier, so the owner can tell which key
is live without seeing it.

**Encrypted like `ssh_keys`.** Table `router_settings`, one row per
tier, the key AES-256-GCM-sealed under `LOOMUX_MASTER_KEY` with
`router_tier:<tier>` as additional data (LOOM-138's `ssh_key:<id>`
scheme), so a ciphertext copied onto the other tier's row doesn't
decrypt. Without a master key the endpoints that write answer 503, as
`/ssh-keys` does. A row that doesn't decrypt at startup (master key
changed) is skipped: that tier falls back to the env and `GET` says
`stored_unreadable: true`, so the owner can re-enter or delete it
rather than the server refusing to start.

**Provider field.** Every tier has `provider`, LOOM-186's
`llmrouter.Provider`: `openai` (any OpenAI-Chat-Completions-compatible
endpoint, the default) or `anthropic` (the native Messages API).
`llmrouter.Providers()` is what's accepted and `GET` returns it. An
anthropic tier may leave `base_url` out: it gets Anthropic's own API
(`https://api.anthropic.com`), as an env tier does.

**A saved key never follows the tier somewhere new.** `api_key` may be
left out of a `PUT` to keep the stored key only while `provider` and
`base_url` stay exactly as stored; changing either needs the key
again, so the existing secret is never sent (by a routing call or the
test call) to an endpoint it wasn't entered for.

**https only.** A stored `base_url` must be `https`; plain `http` is
accepted only for a loopback host (`localhost`, `127.0.0.0/8`, `::1`:
a local model server). A key never crosses the network in the clear,
and the test call can't be aimed at plain-http services on the
network.

**Hot reload.** `llmrouter.Model` holds its config in an
`atomic.Pointer`; `Decide`/`Relay` load it once per call, so a call in
flight finishes on the config it started with and the next call uses
the new one. `Model.SetConfig` validates before swapping and resets the
primary circuit breaker when the primary tier changed (an outage of the
old provider says nothing about the new one). Each config carries a
primary generation, bumped with the primary; a call still running on
the old primary when it's replaced records nothing into the new one's
breaker. The settings service
(`llmrouter.Settings`) serializes changes under a mutex: store write,
then swap, then audit, so the live config always matches the last
successful write. No restart.

**Redaction.** Every router key Loomux has held in this process — env
keys at startup, stored keys at load and on each `PUT`, including the
ones rotated away from — joins a process-wide set in
`credentials` (`AddSystemSecret`) that `RedactValues` (and so
`RedactAll` and every scoped scrub in `router`) always removes. Agent
output, relay input, command output and event-log text can't carry a
router key out. Keys are never passed to a logger.

**Test.** `POST /api/v1/settings/router/{tier}/test` makes one call in
the tier's protocol (a chat completion, or a Messages request for
`anthropic`) with the tier's *effective* config (stored or env), a
one-word prompt and `max_tokens: 1`, with a 15s timeout, and returns
`{ok, status, error_class, error, model, source, duration_ms}`. A
failure is reported by the provider's HTTP status and an error class
alone — `auth_failed` (401/403), `not_found` (404), `rate_limited`
(429), `bad_request` (other 4xx), `provider_error` (5xx), `timeout`,
`unreachable` — with `error` saying it in words from those two. The
provider's response body is never returned: it may quote the request,
and the key with it.

**Audit.** Table `router_settings_audit`: id, tier, action (`set`,
`clear`), the names of the fields that changed (`provider`, `base_url`,
`model`, `api_key` — the name only), actor and time. Written in the same
transaction as the change. The actor is `session:<id>` of the login that
made it — Loomux has one owner account (one password, design spec §9),
so every authenticated session is the owner's, and the session id says
which device. `GET /api/v1/settings/router/audit` lists the latest.

**Auth.** Every endpoint is behind `requireAuth`, like the rest of
`/api/v1`. There are no non-owner accounts to exclude; if roles arrive,
these endpoints are owner-only.

## API (additive to v1)

```
GET    /api/v1/settings/router
PUT    /api/v1/settings/router/{tier}          {provider?, base_url, model, api_key?}
DELETE /api/v1/settings/router/{tier}
POST   /api/v1/settings/router/{tier}/test
GET    /api/v1/settings/router/audit?limit=N   (default 50, at most 200)
```

`{tier}` is `primary` or `escalation`; anything else is 404.

`GET` →

```json
{
  "providers": ["openai", "anthropic"],
  "tiers": [
    {
      "tier": "primary",
      "source": "stored",          // "stored" | "env" | "none" (escalation only)
      "provider": "openai",
      "base_url": "https://api.groq.com/openai/v1",
      "model": "llama-3.1-8b-instant",
      "key_fingerprint": "sha256:3f9a0c1e22b4",
      "key_last4": "x9Qa",
      "set_at": "2026-10-08T12:00:00Z",   // stored only
      "env_configured": true,             // is there an env tier to fall back to
      "stored_unreadable": false
    },
    { "tier": "escalation", "source": "none", "env_configured": false, ... }
  ]
}
```

`PUT` validates: `provider` (default `openai`, case-insensitive) must be
in `providers`; `base_url` an `https` URL without userinfo (`http` only
for a loopback host), optional for `anthropic`; `model`
non-empty, at most 200 characters, no control characters; `api_key` 8
characters to 4 KiB, no whitespace or control characters. `api_key` may be
omitted only when the tier already has a stored key and `provider` and
`base_url` are unchanged; the key is then kept — so changing the model
doesn't mean re-entering the key, but pointing the tier elsewhere does. A `PUT` that
changes nothing is 200 with no audit entry. Answers the tier as `GET`
shows it. 400 on invalid input, 503 without a master key.

`DELETE` removes the stored tier (204; 404 if none is stored). For the
escalation tier with no env escalation, that turns escalation off.

`POST .../test` → `{"ok": true, "model": "...", "source": "stored",
"duration_ms": 412}` or `{"ok": false, "status": 401, "error_class":
"auth_failed", "error": "the provider refused the key (HTTP 401)", ...}`
(200 either
way: the request worked, the provider didn't). 404 for a tier with no
config (`escalation` with source `none`).

`GET .../audit` → `{"entries": [{"id", "tier", "action", "fields":
["model","api_key"], "actor": "session:…", "created_at"}]}`, newest
first.

## Web UI (loomux/web)

A **Router model** section on `/settings` (`src/routes/SettingsPage.tsx`),
between Appearance and Devices, in the same card style
(`rounded-card border border-line bg-surface p-5`). Data through
`src/lib/api.ts` only (apiBoundary test), react-query keys
`["router-settings"]` and `["router-settings-audit"]`.

- One card per tier, "Primary" and "Escalation". Header line: source
  badge — "Saved in Loomux" (`stored`), "From the environment" (`env`),
  "Off" (`none`). Body: provider, base URL, model, and the key as
  `•••• x9Qa · sha256:3f9a0c1e22b4` (fingerprint alone when there's no
  `key_last4`), and "Set <relative time>" for a stored tier. If
  `stored_unreadable`, a warning: "The saved settings for this tier can't
  be decrypted with this server's master key; the environment's are in
  use. Save them again or remove them."
- **Edit** opens an inline form: provider (select from `providers`;
  hidden while there's only one), base URL, model, and API key as a
  `type="password"` field with `autocomplete="off"`, empty, placeholder
  "Leave empty to keep the saved key" when the tier is stored (required
  otherwise). Save → `PUT`; the key field is cleared on success and on
  failure, and its value is never put in query cache, URL, or storage.
  Errors show the server's `error` text.
- **Test** → `POST .../test`, shows "Works · 412 ms" or the error, inline,
  until the next edit. Offered for any tier whose source isn't `none`.
- **Use environment settings** (stored tiers only, confirm dialog: "Go
  back to the environment's settings for the primary tier?" / for
  escalation without env: "Turn escalation off?") → `DELETE`.
- Below the cards, **Recent changes**: the latest 20 audit entries,
  "<Tier>: model, api_key changed · <time>" ("removed" for `clear`). The
  actor (an opaque session id) isn't shown: there is one owner.
- If `GET` is 404 (an older server), the section hides itself, like
  WebClientUpdate. 503 on save: "This server has no LOOMUX_MASTER_KEY, so
  it can't store keys."
- Tests (vitest + testing-library): renders each source; the key never
  appears in the DOM after save (assert the rendered HTML doesn't contain
  it); empty key on a stored tier sends no `api_key`; test result shown;
  delete confirms; 404 hides the section. axe check in e2e.

## Not in scope

- Turning env escalation off without removing it from the env (a
  stored "disabled" escalation); the owner can unset the env var.
- Starting with no env primary at all (a fresh server needs the
  bootstrap until it can be logged in to).
- Per-tier timeouts.
