# llmrouter

A real, LLM-backed `router.RoutingModel` (design spec §6). Replaces the
deterministic `routertest.StubRoutingModel` with an actual model call for
both `Decide` (routing) and `Relay` (condensation).

## Design

Vendor and model are **config-swappable**, not hardcoded: `Model` is a
generic client for any provider speaking the OpenAI Chat Completions wire
protocol (Groq, Google Gemini's OpenAI-compatible endpoint, OpenRouter,
self-hosted Ollama/vLLM, Anthropic's own OpenAI-compat surface if used,
etc.) — swapping vendor is a config change, not a code change. This is
deliberate: routing/relay are simple extraction/summarization tasks that
don't need a frontier model, so the intended primary tier is a free or
extremely cheap, fast one.

There are **two tiers**, primary and an optional escalation, each a
`{BaseURL, APIKey, Model}` triple (`Tier`). `Decide`/`Relay` try the primary
tier first; if it fails — a transport/rate-limit error (after the SDK's own
default retry policy, `MaxRetries: 2`, is exhausted) or, for `Decide` only,
an unparseable/invalid tool call — they fall back to the escalation tier
when one is configured. If escalation also fails, the returned error
mentions both failures; if escalation isn't configured, the primary's
error surfaces directly. This is the "dynamic" half of the design: a
tiny/free model handles the common case, and a per-request escalation
absorbs both quality problems (a weak model's malformed output) and
availability problems (the primary provider being down/rate-limited),
without a hand-rolled retry loop duplicating what the SDK's own retry
policy already covers.

`Decide` gets structured output via a **forced tool/function call**
(`tool_choice` pinned to a single `route_decision` function), not JSON
mode — the schema mirrors `router.Decision` + `router.ProvisionSpec`
flattened into one object. `agent_type` and `workspace_id` are
enum-constrained to the caller-supplied valid sets when non-empty, which
constrains the model's choice at the schema level rather than only
validating after the fact. `Relay` is plain free-text completion — no
schema — since its output is prose, not structured data.

**Known risk**: whether every OpenAI-compatible provider honors a *forced,
named* `tool_choice` (vs. only `auto`) is unverified for any specific
target vendor. This isn't handled with a separate JSON-mode fallback — a
primary tier that doesn't support it will simply fail every `Decide` call,
which the escalation mechanism already absorbs if one is configured. An
operator picking such a provider as primary with no escalation configured
will see every `Decide` call fail; verify `tool_choice` support against
your chosen vendor before deploying without an escalation tier.

## Configuration

```
LOOMUX_ROUTER_PRIMARY_BASE_URL      (required)
LOOMUX_ROUTER_PRIMARY_API_KEY       (required)
LOOMUX_ROUTER_PRIMARY_MODEL         (required)

LOOMUX_ROUTER_ESCALATION_BASE_URL   (optional, all-or-nothing with the two below)
LOOMUX_ROUTER_ESCALATION_API_KEY
LOOMUX_ROUTER_ESCALATION_MODEL
```

`ConfigFromEnv()` fails fast (mirrors `registry/sqlite/crypto.go`'s
`KeyFromEnv` convention) if any primary var is missing, or if the
escalation vars are set partially rather than all-or-nothing.

## Layout

- `config.go` — `Tier`, `Config`, `ConfigFromEnv`, env var names,
  `ErrConfigInvalid`.
- `schema.go` — the forced tool-call JSON schema for `Decide`, and the
  system-prompt text for both `Decide` and `Relay`.
- `llmrouter.go` — `Model`, `New`, functional `Option`s
  (`WithPrimaryTimeout`, `WithEscalationTimeout`).
- `decide.go` — `Decide` (escalation orchestration) and `decideWith` (one
  tier's round trip: build the schema, force the tool call, parse and
  validate the result).
- `relay.go` — `Relay` (escalation orchestration) and `relayWith` (one
  tier's plain free-text round trip).
- `client.go` — `buildClient`, the one place `option.WithBaseURL`/
  `WithAPIKey` are applied.

## Testing

All tests run against `httptest.Server` fakes standing in for both tiers —
no real network calls, no live API keys. Run `go test ./router/llmrouter/...`.
