# llmrouter

A real, LLM-backed `router.RoutingModel` (design spec §6). Replaces the
deterministic `routertest.StubRoutingModel` with an actual model call for
both `Decide` (routing) and `Relay` (condensation).

## Design

Vendor and model are **config-swappable**, not hardcoded. Each tier
names a **provider** — the wire protocol it speaks (LOOM-186):

- `openai` (the default): any endpoint speaking the OpenAI Chat
  Completions protocol (Groq, Google Gemini's OpenAI-compatible endpoint,
  OpenRouter, self-hosted Ollama/vLLM, etc.).
- `anthropic`: Anthropic's native Messages API, through the official
  `anthropic-sdk-go`, so a tier can run on current Claude models.

Swapping vendor is a config change, not a code change. This is
deliberate: routing/relay are simple extraction/summarization tasks that
don't need a frontier model, so the intended primary tier is a free or
extremely cheap, fast one.

There are **two tiers**, primary and an optional escalation, each a
`{Provider, BaseURL, APIKey, Model}` set (`Tier`); the two may use
different providers. `Decide`/`Relay` try the primary
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

Both `Decide` and `Relay` get structured output via a **tool/function
call** to a single function each, not JSON mode — on `openai` tiers a
forced one (`tool_choice` pinned to it). `Decide`'s `route_decision` schema mirrors
`router.Decision` + `router.ProvisionSpec` flattened into one object;
`agent_type` and `workspace_id` are enum-constrained to the
caller-supplied valid sets when non-empty, which constrains the model's
choice at the schema level rather than only validating after the fact.
`Relay`'s `condense_output` schema is just `{reply, done}` — `done`
(design spec §3 step 3: "if the task itself, not just the turn, is
finished") is what tells `Router.Dispatch` whether to tear the task's
pane down or leave it open for the next turn in the same session
(LOOM-13). Structured output for `Relay` too (not free text) is what
makes that signal reliable rather than parsed out of prose.

A tier call that answers but can't be used — no tool call, unparseable
or invalid arguments, an empty relay reply — gets **one corrective retry
on the same tier**, told what was wrong (LOOM-107; `Relay` too since
LOOM-186), before escalating. A refusal isn't retried on the same tier:
it escalates at once.

### The `anthropic` provider

Everything above one exchange — prompts, schemas, validation, the
corrective retry, escalation, the circuit breaker — is shared
(`provider.go`'s `exchange` and `converse`); `anthropic.go` only maps it
onto the Messages API:

- **No forced tool choice.** Claude Sonnet 5.5, Opus 5.5 and Fable 5.1
  reject `tool_choice` `any`/`tool` with a 400, so the tool is offered
  with `tool_choice: auto` (parallel tool use off) and `strict: true`,
  and the system prompt ends by telling the model to call it. A reply
  without the call is caught and retried once, then surfaces as an
  error — the decision is never taken from free text. The retry replays
  the rejected reply unchanged (thinking blocks included) and answers its
  tool call, if any, with an error `tool_result`.
- **Strict schemas.** Every object is closed with
  `additionalProperties: false`; regex `pattern`s are dropped, since
  strict mode compiles a narrower dialect — the workspace name is
  validated in Go (`router.ProvisionSpec.Validate`) as before.
- **Prompt caching.** The system block carries a `cache_control`
  breakpoint, caching the tool definition and system prompt (rendered
  first) across messages; they change only when agent types, workspaces
  or targets do. Below the model's minimum cacheable prefix (4096 tokens
  on Haiku 4.5, 512 on the 5.5 generation) the API silently doesn't
  cache.
- **Refusals.** `stop_reason: "refusal"` is checked before the content
  is read and reported with its category; the call escalates.
- **Effort.** `output_config.effort: low` on models that take it
  (routing and condensing are simple); left out on Haiku 4.5, Sonnet 4.5
  and older, which reject it. Thinking is left at the model's default.
- **Limits and errors.** `max_tokens` 8192 (headroom, thinking
  included). The SDK retries 408/409/429/5xx and connection failures
  twice, honouring `retry-after`; a 400-class error isn't retried. Errors
  are reported with their status and type (`429 rate_limit_error`).
  Token usage counts cache reads and writes as prompt tokens.

**Known risk** (`openai` tiers): whether every OpenAI-compatible provider honors a *forced,
named* `tool_choice` (vs. only `auto`) is unverified for any specific
target vendor. This isn't handled with a separate JSON-mode fallback — a
primary tier that doesn't support it will simply fail every `Decide`/
`Relay` call, which the escalation mechanism already absorbs if one is
configured. An operator picking such a provider as primary with no
escalation configured will see every call fail; verify `tool_choice`
support against your chosen vendor before deploying without an
escalation tier.

## Configuration

```
LOOMUX_ROUTER_PRIMARY_PROVIDER      openai (default) or anthropic
LOOMUX_ROUTER_PRIMARY_BASE_URL      (required for openai; anthropic defaults to https://api.anthropic.com)
LOOMUX_ROUTER_PRIMARY_API_KEY       (required)
LOOMUX_ROUTER_PRIMARY_MODEL         (required)

LOOMUX_ROUTER_ESCALATION_PROVIDER   (optional tier: set it fully, or none of it)
LOOMUX_ROUTER_ESCALATION_BASE_URL
LOOMUX_ROUTER_ESCALATION_API_KEY
LOOMUX_ROUTER_ESCALATION_MODEL
```

`ConfigFromEnv()` fails fast (mirrors `registry/sqlite/crypto.go`'s
`KeyFromEnv` convention) if any primary var is missing, a provider name
is unknown, or the escalation tier is set partially. An `anthropic`
base URL is the API root (`https://api.anthropic.com`, or a proxy's); a
trailing `/v1` is trimmed, as the SDK adds `/v1/messages` itself.

For an all-Claude router, a cheap primary and a stronger escalation:

```
LOOMUX_ROUTER_PRIMARY_PROVIDER=anthropic
LOOMUX_ROUTER_PRIMARY_API_KEY=sk-ant-…
LOOMUX_ROUTER_PRIMARY_MODEL=claude-haiku-4-5
LOOMUX_ROUTER_ESCALATION_PROVIDER=anthropic
LOOMUX_ROUTER_ESCALATION_API_KEY=sk-ant-…
LOOMUX_ROUTER_ESCALATION_MODEL=claude-sonnet-5-5
```

Use model ids without date suffixes. Mixing providers works too (e.g. a
free `openai`-protocol primary escalating to `anthropic`).

Code that builds a `Config` itself (e.g. a router-settings API, LOOM-185)
sets `Tier.Provider`, checks a tier with `Tier.Validate()`, parses a
name with `ParseProvider` and lists the choices with `Providers()`.

## Layout

- `config.go` — `Tier`, `Tier.Validate`, `Config`, `ConfigFromEnv`, env
  var names, `ErrConfigInvalid`.
- `provider.go` — `Provider`, `ParseProvider`, `Providers`; the
  provider-neutral `toolSpec` and `exchange`, and `converse`, the one
  ask / validate / corrective-retry loop Decide and Relay share.
- `client.go` — the `openai` exchange; `buildClient`, the one place its
  `option.WithBaseURL`/`WithAPIKey` are applied.
- `anthropic.go` — the `anthropic` exchange: strict tool, caching,
  effort, refusal and error handling.
- `schema.go` — the tool-call JSON schemas for `Decide` and `Relay`, and
  the system-prompt text for both.
- `llmrouter.go` — `Model`, `New`, functional `Option`s
  (`WithPrimaryTimeout`, `WithEscalationTimeout`).
- `decide.go` — `Decide` (escalation orchestration) and `decideWith` (one
  tier's round trip: build the schema, run the exchange, parse and
  validate the result).
- `relay.go` — `Relay` (escalation orchestration) and `relayWith` (one
  tier's round trip for the `condense_output` call, parsed into
  `router.RelayResult`).

## Testing

All tests run against `httptest.Server` fakes standing in for both tiers —
no real network calls, no live API keys. Run `go test ./router/llmrouter/...`.
`provider_test.go` runs the same scenarios against a fake of each
provider's wire format (tool call, a reply without the tool call retried
once, a refusal escalating, a 429 retried, a 400 not) and checks the
Anthropic request itself (auto tool choice, strict closed schema, cached
system prompt, effort).

### Live Anthropic test

`TestAnthropic_Live` runs `Decide` and `Relay` against Anthropic's API.
It is skipped unless a key is set, so CI never runs it:

```
LOOMUX_ROUTER_LIVE_ANTHROPIC_API_KEY=sk-ant-… LOOMUX_ROUTER_LIVE_ANTHROPIC_MODEL=claude-sonnet-5-5 \
  go test ./router/llmrouter/ -run Anthropic_Live -v
```

(The model defaults to `claude-haiku-4-5`;
`LOOMUX_ROUTER_LIVE_ANTHROPIC_BASE_URL` points it elsewhere.)

### Routing evals (real model)

`eval_test.go` (build tag `routereval`, LOOM-88/LOOM-87) runs ten routing
cases against the real primary tier, three times each, and needs every
run to pass: agent availability, command-vs-agent, agent by name,
reusing a matching workspace, and conversation follow-ups. A decision is
scored after `router.ApplyAffinity`, as the router acts on it. Not part
of the normal run:

```
LOOMUX_ROUTER_PRIMARY_PROVIDER=… LOOMUX_ROUTER_PRIMARY_BASE_URL=… LOOMUX_ROUTER_PRIMARY_API_KEY=… LOOMUX_ROUTER_PRIMARY_MODEL=… \
  go test -tags routereval ./router/llmrouter/ -run Eval -v
```

Calls are paced (`ROUTEREVAL_PACE`, default 20s) for a free-tier
tokens-per-minute limit; errored calls are retried and reported as
infrastructure errors, not wrong decisions. `ROUTEREVAL_OUT` writes a
JSON summary.

