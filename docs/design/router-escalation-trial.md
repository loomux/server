# Trial: the router's escalation tier on Claude Haiku via Anthropic's OpenAI-compatible endpoint

LOOM-187 (GitHub loomux/server#362). This document is the plan for a
live trial. The code it uses (configuration, the evaluation harness, the
fake endpoint) is merged ahead of it and has made no live calls. Every
step below that spends money or touches an instance is yours to run.

## What is being trialled

The escalation tier (`docs/design/core-design.md` §6,
`router/llmrouter/README.md`) answers only when the primary tier fails.
The trial points it at Claude Haiku through Anthropic's **OpenAI SDK
compatibility endpoint**:

| Setting | Value |
|---|---|
| provider | `openai` |
| base URL | `https://api.anthropic.com/v1` (the `/v1` is required; without it the tier is refused at start-up and by Settings) |
| model | a config value: `claude-haiku-4-5` or `claude-haiku-5-5` |
| key | an Anthropic API key (`sk-ant-…`) |

Nothing needed adding to make this configurable: since LOOM-186 every
tier is `{provider, base URL, key, model}`, from the environment
(`LOOMUX_ROUTER_ESCALATION_*`) or from Settings (`/api/v1/settings/router`,
which overrides the environment without a restart). LOOM-187 adds:

- **Validation.** An `openai` tier on `api.anthropic.com` must name
  `/v1`. `ConfigFromEnv`, `New`, `SetConfig` and the Settings API refuse
  anything else, with a message that points at `/v1` or at the native
  `anthropic` provider.
- **Request shaping.** On that endpoint the router also sends
  `max_completion_tokens: 8192` (the native provider's bound). The
  Messages API behind it needs a bound; we don't rely on the endpoint
  choosing one. Other OpenAI-protocol endpoints get the same request as
  before.

### Known differences of the endpoint, and how the router meets them

| Difference | Effect on the router |
|---|---|
| `strict` on a function is ignored | Tool calls are not schema-guaranteed (the native provider's are). Validation and the one corrective retry catch a bad call; the harness counts these as corrective retries. |
| `response_format`, `seed`, `logprobs`, penalties, `logit_bias`, `store`, `metadata` are ignored | The router sends none of them (tested). |
| `n` must be 1 | The router never sets it. |
| No `refusal` field; no prompt caching | A refusal arrives as a reply without a tool call: retried once, then the call fails (and, as the escalation tier, surfaces to the user). Every call pays full input price. |
| Forced `tool_choice` | The router pins `tool_choice` to the function. Anthropic's native API refuses this on Opus 5.5, Sonnet 5.5 and Fable 5.1, and refuses it alongside extended thinking. The issue reports the endpoint still accepts it for Haiku 4.5. **For Haiku 5.5, whose thinking is on by default, this is unverified.** The smoke step settles it. A refusal shows up as the error class `400 tool_choice not supported`. |

The fake endpoint (`router/llmrouter/llmroutertest`) models all of these.
Its doc comment separates Anthropic's documented behaviour from the two
points it only assumes (a missing token bound is a 400; forced
`tool_choice` can be refused).

## Haiku 4.5 or Haiku 5.5?

**Recommendation: trial both, and adopt Claude Haiku 5.5 (`claude-haiku-5-5`) if it passes.**

- Haiku 5.5 is the current Haiku, and list price is a tenth of Haiku
  4.5's: $0.10/$0.50 per MTok against $1/$5 (prompts up to 100K tokens;
  a routing call is about 2.4K). It has a 1M-token context and
  `effort`.
- The risks are specific to this endpoint:
  1. Forced `tool_choice` with thinking on by default may be refused
     (see above).
  2. Its safeguards can refuse, with no server-side fallback.
  3. Its newer tokenizer counts about 30% more tokens for the same
     text. The estimates below already include that.
- Haiku 4.5 is what the issue named and is known to accept forced
  `tool_choice`. Keep it as the fallback candidate.
- If Haiku 5.5 fails the smoke step only on `tool_choice`, the fix is
  not a workaround in the `openai` path. Run the same model on the
  **native `anthropic` provider** (LOOM-186). It uses `tool_choice: auto`
  with `strict: true` schemas and prompt caching, and was built because
  newer Claude models refuse forced tool use. The optional third arm
  below measures exactly that.

## Metrics compared

`loomux-routereval` reports these per run (JSON and Markdown).
`loomux-routereval compare` puts two runs side by side.

| Metric | Why it matters for escalation | Adopt if |
|---|---|---|
| Accuracy, answered calls only | Model quality, net of endpoint errors | ≥ baseline, and no category (`provision`, `command`, `open-task`, …) more than one case worse |
| Accuracy (all calls) | What users would see | ≥ baseline |
| Errors by class | A non-transient class (`400 …`, `404`, `401`) fails every escalation | none except transient `429`/`5xx` |
| Corrective retries | Schema misses, since `strict` is ignored | ≤ 10% of calls |
| Latency p50 / p95 | Escalation runs after the primary has already failed; its timeout is 45s | p95 ≤ 10s |
| Cost per 1k calls | Escalation volume × this is the bill | ≤ baseline |

The fixture set (`router/llmrouter/routereval/fixtures/escalation.json`)
has 24 routing and 6 relay cases. The routing cases cover provisioning,
commands, agents named in the message, reusing workspaces, direct
answers, open tasks, follow-ups and harder messages (multi-constraint,
noisy, misspelt). The data is synthetic, with no user data. Each case is
scored on the decision the router acts on, after `router.ApplyAffinity`.

## Budget

**Cap: $5 for the whole trial.** Each live run takes `--budget-usd`. The
run refuses to start if its expected cost is over that budget, and stops
before any call that could take the spend past it.

Measured offline: about 2.4K prompt tokens per call. The estimates
assume 1,024 output tokens per answer. Worst case means every call needs
the corrective retry.

| Run (3 runs × 30 cases = 90 calls) | Expected | Worst case | `--budget-usd` |
|---|---|---|---|
| Smoke, per model (`--max-calls 2`) | < $0.01 | < $0.03 | 0.10 |
| Baseline: current escalation model | depends on model (Sonnet 5.5 ≈ $1.4) | ≈ $3.4 | 2.00 |
| Claude Haiku 4.5 | $0.68 | $1.70 | 1.00 |
| Claude Haiku 5.5 | $0.07 | $0.17 | 0.50 |
| Optional: Haiku 5.5, native provider | ≈ $0.07 | ≈ $0.17 | 0.50 |

If the baseline model's expected cost is over $2, run it with `--runs 1`
(or `--cases` for a subset). Leave the cap where it is.

## Steps

All of these are run by you, from a checkout of `main` once this is
merged. Steps 1–5 call Anthropic's API from your machine. They touch no
Loomux instance.

0. **Offline check (free).** This confirms the harness and fixtures and
   prints a cost estimate. It needs no key and makes no network calls:

   ```sh
   go run ./cmd/loomux-routereval run --model claude-haiku-5-5 --runs 3
   ```

   It must report 100% (the fake answers with each case's expected
   decision).

1. **Key.** Create an Anthropic API key for the trial, in a workspace
   with a spend limit of $5 set in the Console. That limit is the hard
   stop behind the harness's own. Export it as `ROUTEREVAL_API_KEY`
   (`--api-key-env` names another variable). Never pass it as a flag.

2. **Smoke: does the endpoint take our request?** Two calls per model:

   ```sh
   for m in claude-haiku-4-5 claude-haiku-5-5; do
     go run ./cmd/loomux-routereval run --live --model $m --runs 1 --max-calls 2 \
       --cases codex-by-name,finished --budget-usd 0.10
   done
   ```

   Both should pass. Note any error class. `400 tool_choice not
   supported` on Haiku 5.5 means skip to the native arm (step 5) for
   that model. `404` means the base URL or the model id is wrong.

3. **Baseline.** Read the current escalation tier from TEST's Settings
   page (or `GET /api/v1/settings/router`: provider, base URL and model;
   the key is never shown). Run it with the same values and your copy of
   its key:

   ```sh
   ROUTEREVAL_API_KEY=… go run ./cmd/loomux-routereval run --live \
     --provider <provider> --base-url <base URL> --model <model> \
     [--price-in <$/MTok> --price-out <$/MTok>] \
     --runs 3 --budget-usd 2 --out baseline.json --md baseline.md
   ```

   Pass prices for a model the harness doesn't know. A free or
   self-hosted model can take `--price-in 0.0001 --price-out 0.0001`.
   For a free-tier rate limit, raise `--pace` (e.g. `20s`).

4. **Candidates** over the compatible endpoint (the defaults:
   `--provider openai --base-url https://api.anthropic.com/v1`):

   ```sh
   go run ./cmd/loomux-routereval run --live --model claude-haiku-4-5 --runs 3 --budget-usd 1 --out haiku45.json --md haiku45.md
   go run ./cmd/loomux-routereval run --live --model claude-haiku-5-5 --runs 3 --budget-usd 0.5 --out haiku55.json --md haiku55.md
   ```

5. **Optional: native arm.** The same model on the native provider:

   ```sh
   go run ./cmd/loomux-routereval run --live --provider anthropic --base-url https://api.anthropic.com \
     --model claude-haiku-5-5 --runs 3 --budget-usd 0.5 --out haiku55-native.json
   ```

   Offline estimates fake only the `openai` protocol. This run has no
   estimate up front, but its budget still stops it.

6. **Compare** each candidate with the baseline and apply the "Adopt
   if" column:

   ```sh
   go run ./cmd/loomux-routereval compare baseline.json haiku55.json
   go run ./cmd/loomux-routereval compare baseline.json haiku45.json
   ```

   Read the failures in each `.md` as well. A run that `stopped early`
   isn't comparable.

7. **TEST, only if a candidate passes.** Point TEST's escalation tier at
   it through Settings (provider `openai`, base URL
   `https://api.anthropic.com/v1`, the model, the key). Then press
   *Test*. This is one call of at most one token. On a model that
   thinks by default, a one-token cap may itself be refused. If *Test*
   fails that way after step 4 passed, trust step 4. The change applies
   without a restart, is audited, and *Clear* reverts to the
   environment's tier. The issue's "add the key to the deployment
   secret" is the alternative (`LOOMUX_ROUTER_ESCALATION_*`, then a
   restart). Settings is easier to undo. Watch
   `loomux_router_calls_total{tier="escalation"}`,
   `loomux_router_retries_total` and `loomux_router_tokens_total` for a
   few days. PROD is out of scope.

Report the outcome (the JSON reports, or their comparisons) back on
LOOM-187.
