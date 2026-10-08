# API v1 freeze review (before 1.0.0)

**Status:** proposals for the user to decide (2026-10-07). Nothing here
changes behaviour yet.

1.0.0 declares `/api/v1` stable: from then on it gains endpoints, fields,
status codes, parameters and headers, but loses none and changes the
meaning of none (`docs/release/v0.2.0.md`, "What 1.0.0 will mean";
enforced by `api/testdata/api-v1-contract.txt`). Anything below that we
would rather not live with for all of 1.x has to be decided **before**
the freeze. Each item says what is there today, a concrete proposal, and
whether the proposal is **breaking**: whether today's clients (the web
client is the only one; scripts against the API are the other kind) would
need to change.

Sources: the contract snapshot (every route, field, status, query
parameter and header), the handlers, and loomux/web's use of each field.

## Summary

| # | Item | Proposal | Breaking? | Recommendation |
|---|---|---|---|---|
| 1 | `ssh_key_ref` on targets | remove | web: yes (form field) | **remove before 1.0** |
| 2 | `is_dynamic` on workspaces | remove | web: yes (one badge) | **remove before 1.0** |
| 3 | `capabilities` on workspaces | remove, or wire up | web: one read | **decide** |
| 4 | `?async=true` on dispatch | remove (keep `Prefer` and `?wait`) | scripts only | **remove before 1.0** |
| 5 | Error text in unauthenticated `/health` | fixed strings | no | do it (non-breaking) |
| 6 | `/targets/{id}/agents/refresh`, `/probe`, `/test` overlap | drop `agents/refresh` | scripts only | **decide** |
| 7 | `id` vs `dispatch_id`/`conversation_id`/`message_id` | one rule, documented | rename: web yes | **decide**; cheapest: document |
| 8 | `POST /dispatch` vs `/dispatches/{id}` | add `POST /dispatches` | no (alias) | add alias; remove old: decide |
| 9 | Enum casing: `awaiting-input` vs `usage_limit` | snake_case everywhere | web: yes | **decide before 1.0** (impossible later) |
| 10 | `confirmations[].workspace` holds a name | `workspace_name` | web: yes (2 reads) | **rename before 1.0** |
| 11 | `attention.resets_at` is free text | `resets` (text) | no client reads it | **rename before 1.0** |
| 12 | `""` meaning "the default" (`purpose`, `permission_mode`, `relay`) | always show the effective value too | mostly no | add `*_effective` consistently |
| 13 | `PUT /targets/{id}` is a partial update | add `PATCH`; document `PUT` | no | add `PATCH` |
| 14 | `health.components{}.detail` is `any` | type it, or exclude it from the promise | no | exclude (document) |
| 15 | `/web/*` exposes release internals | trim, or mark ops-only | web: maybe | **decide** |
| 16 | Errors are prose only | add a machine-readable `code` | no | add before 1.0 |
| 17 | Unbounded lists | decide paging now | maybe | **decide before 1.0** |
| 18 | Stream has no resume | add SSE `id:` later | no | later |
| 19 | Admin-only routes once there are several users | reserve 403 now | no | document now |
| 20 | "Not configured" is `404` for credentials, `501` elsewhere | `501` everywhere | web: check | **decide before 1.0** |

The rest of this document is the detail behind each row.

## A. Dead or leaky fields: remove before the freeze

### 1. `ssh_key_ref` on targets

**Today.** Accepted on `POST`/`PUT /targets`, stored, returned on every
target. Nothing reads it: SSH keys come from the mounted secret
(`docs/deploy/ssh.md`), and the per-target overrides are `ssh_port` and
pinned host keys (LOOM-114). The web edit form shows it.

**Proposal.** Remove it from the request and the response (keep the DB
column until a later migration, or drop it).
**Breaking:** the web client sends and pre-fills it (`src/lib/targets.ts`):
a small change there. Scripts that send it would get it ignored if we
keep accepting-and-ignoring for a while.
**Recommendation:** remove before 1.0. Freezing a field that does nothing
invites someone to think it selects a key.

### 2. `is_dynamic` on workspaces

**Today.** Always `true`: every workspace is created by provisioning
(`router.go`, `command.go`); the static workspaces it once distinguished
no longer exist. The web dashboard shows a badge for it.
**Proposal.** Remove from the response.
**Breaking:** web, one badge (`DashboardPage.tsx`).
**Recommendation:** remove before 1.0.

### 3. `capabilities` on workspaces

**Today.** Documented as "the MCPs/tools available in this workspace", but
nothing sets it (always `[]`); the router passes it to the routing model.
**Proposal.** Either remove it, or decide what fills it (an agent probe?)
before 1.0. An always-empty frozen field is noise.
**Breaking:** web reads it once.
**Recommendation:** remove unless there's a plan for it.

### 4. `?async=true` on `POST /dispatch`

**Today.** Three ways to ask for the same thing: dispatch is async by
default, `Prefer: respond-async` (RFC 7240, what the web client sends) and
`?async=true`; `?wait=true` asks for blocking. `async` and `Prefer` are
redundant.
**Proposal.** Drop `?async`; keep `Prefer: respond-async` (standard) and
`?wait=true`.
**Breaking:** scripts only; the web client doesn't use it.
**Recommendation:** remove before 1.0.

### 5. Error text in the unauthenticated `/health`

**Today.** `GET /api/v1/health` needs no session. Its `database`
component carries the database ping's error text as is, and
`router_model` says "router model not configured": internals to an
anonymous caller (the deep check, which lists targets, is
authenticated).
**Proposal.** Unauthenticated health returns `status` and each
component's `status` only, with a fixed `error` ("unavailable"); the
detail stays in `/health/deep` and the logs.
**Breaking:** no (same shape).
**Recommendation:** do it; not a freeze blocker, since it isn't shape.

### 6. Overlapping target checks

**Today.** `POST /targets/{id}/probe` re-probes health **and** agent CLIs;
`POST /targets/{id}/agents/refresh` re-probes the agent CLIs only;
`POST /targets/{id}/test` (LOOM-114, a user decision) runs the probe and
answers `reachable`/`tmux_version`/`host_key_problem`. The web client uses
none of them yet.
**Proposal.** Drop `agents/refresh` (`probe` is its superset); keep
`probe` (full refresh, records results) and `test` (the onboarding
check).
**Breaking:** scripts only.
**Recommendation:** decide; removing is free now and impossible later.

## B. Naming

### 7. How identifiers are named

**Today.** Most resources call their own id `id` (targets, workspaces,
tasks, messages, confirmations, events, credentials, sessions). Some
don't: a dispatch's own id is `dispatch_id`, a conversation row's is
`conversation_id`, the stream's `message_added` uses `message_id`, and
attach-info has `task_id`.
**Proposals,** pick one:
- (a) **Document the rule and its exceptions** ("a resource's own id is
  `id`; a reference to another is `<thing>_id`; dispatches and the
  conversation list predate the rule") and freeze as is. Not breaking.
- (b) **Add `id` beside `dispatch_id`** on dispatch responses (additive)
  and say new clients use `id`; `dispatch_id` stays for all of 1.x.
- (c) **Rename now.** Breaking: the web client reads `dispatch_id` in 19
  places.

**Recommendation:** (a), maybe with (b). A rename buys little.

### 8. `POST /dispatch` vs `GET /dispatches/{id}`

**Today.** Creation is singular, everything else plural.
**Proposal.** Add `POST /dispatches` as the canonical route; keep `POST
/dispatch` as an alias for all of 1.x (or remove it before 1.0, which
breaks the web client's one call).
**Breaking:** the alias isn't.
**Recommendation:** add the alias; keep the old route.

### 9. Enum casing

**Today.** Task statuses are kebab-case (`awaiting-input`,
`needs-attention`, `human-takeover`), and so is a conversation's
`status`, which mirrors them. Every other enum is snake_case: attention
kinds (`usage_limit`), relay (`last_message`), error classes
(`target_unreachable`), confirmation kinds (`run_command`).
**Proposal.** snake_case everywhere: `awaiting_input`,
`needs_attention`, `human_takeover`, mapped at the API boundary (the
database can keep its values).
**Breaking:** yes. The web client compares these strings.
**Recommendation:** decide before 1.0; after, it's mixed forever.

### 10. `confirmations[].workspace`

**Today.** Holds a workspace **name**, while the same object has
`target_id` and `target_name` side by side.
**Proposal.** Rename it to `workspace_name` (and add `workspace_id` when
there is one).
**Breaking:** web, two reads.
**Recommendation:** rename before 1.0.

### 11. `attention.resets_at`

**Today.** Free text as the agent printed it ("5pm (Europe/Istanbul)"),
but every other `*_at` is an RFC 3339 timestamp.
**Proposal.** Rename it to `resets` (text); add `resets_at` as a real
timestamp later, when it can be parsed.
**Breaking:** no client reads it.
**Recommendation:** rename before 1.0.

### 12. `""` meaning "the default"

**Today.** `purpose: ""` means personal; `permission_mode: ""` means each
agent's default; `relay: ""` means the purpose's default, with
`relay_effective` showing what applies.
**Proposal.** Keep `""` as "the default" in requests, and give every such
field an effective companion in responses (`purpose_effective`,
`permission_mode_effective`), as `relay` has.
**Breaking:** no (additive).
**Recommendation:** add them; not a blocker.

### 13. `PUT /targets/{id}` is a partial update

**Today.** Optional fields left out keep their stored value (LOOM-119),
but `name`, `kind`, `host` and `user` are required: neither PUT
(replace) nor PATCH (merge) semantics.
**Proposal.** Add `PATCH /targets/{id}` with true merge semantics;
document `PUT` as it behaves.
**Breaking:** no.
**Recommendation:** add `PATCH` when the UI needs it; document `PUT` now.

## C. Shapes

### 14. `health.components{}.detail` is `any`

**Today.** Each component puts its own structure there (the deep check's
`targets` lists targets). The contract can only record it as `any`.
**Proposal.** Either give each component a typed detail, or state that
`detail` is informational and outside the v1 promise.
**Breaking:** no.
**Recommendation:** exclude it in the docs.

### 15. `/web/version`, `/web/update`, `/web/rollback`

**Today.** They expose the release pipeline's internals: `repo`, `tag`,
`tarball`, `sha256`, `schema`, `source`, `latest_error`. `current` is
nullable, while `latest` and `previous` are nullable **and**
omitempty.
**Proposal,** either:
- (a) **Trim** to `{version, commit, built_at}` per bundle;
- (b) **Declare `/web/*` an operations API** outside the v1 promise
  (documented).
**Breaking:** (a) touches the web update panel; (b) none.
**Recommendation:** (b). Production runs with web updates off anyway.

### 16. Errors are prose only

**Today.** Every error is `{"error": "<sentence>"}`. The busy 409 adds
`dispatch_id`. A client that wants to react (a busy conversation, a
message too large, a stale offer) has to match the text.
**Proposal.** Add a machine-readable `code` to every error
(`conversation_busy`, `not_found`, `invalid_request`, `rate_limited`,
`message_too_large`, …), and keep `error` as the human text.
**Breaking:** no (additive).
**Recommendation:** add before 1.0, so v1 clients are written against
codes from the start.

### 17. Unbounded lists

**Today.** `GET /conversations`, `GET /conversations/{id}` (every message,
task, dispatch and confirmation), `/events`, `/workspaces`, `/targets`,
`/credentials` and `/sessions` return everything. Only the task
transcript pages (`limit`, `before`, `has_more`, `next_before`).
**Proposal.** Decide now whether lists may page:
- **Either** freeze "complete lists" (paging later only through new,
  opt-in parameters);
- **or** say now that lists may be paged: a `has_more` field (always
  `false` today) and the transcript's `limit`/`before` convention, so
  adding paging later breaks nobody.

  Conversation history is the one that grows without bound.
**Breaking:** adding `has_more: false` isn't. Paging a list a client
assumes is complete would be.
**Recommendation:** add `has_more` and the convention to
`GET /conversations/{id}` and `/conversations` before 1.0.

### 18. The stream can't resume

**Today.** `GET /conversations/{id}/stream` sends no SSE `id:` lines, so a
reconnecting client refetches the conversation; `message_added` carries
ids, not content.
**Proposal.** Add `id:` and honour `Last-Event-ID` later.
**Breaking:** no (additive).
**Recommendation:** later; not a blocker.

## D. Access

### 19. Routes that would be admin-only with several users

**Today.** Loomux has one user, and any session may do anything.
LOOM-114's scan/pin endpoints already say they must become admin-only if
that changes.
**Proposal.** Document now which routes need an admin role once roles
exist: target CRUD, scan/pin/test/probe, credentials, `/web/update` and
`/web/rollback`, `/health/deep`, `/conversations/{id}/events` (the audit
trail), and others' sessions. They would answer **403** to a non-admin.
Adding a status code is additive under the contract, so this needs no
change now, only the stated intent.
**Breaking:** no.
**Recommendation:** document it in api/README.md at 1.0.

Unauthenticated today, which is fine: `/health` (with item 5) and
`/version`.

### 20. "Not configured on this server": 404 or 501

**Today.** A feature the server wasn't set up for answers differently by
route. Every `/credentials` route answers `404 {"error": "credentials
are not available on this server"}` without a vault master key, which a
client can't tell from `404 no such credential`. Every other optional
feature (task cancellation, the audit trail, host key pinning, target
probing, SSH keys, workspace management) answers `501` ("… is not
configured on this server"). Found in LOOM-175 (item 10) and left as it is: changing
a status code isn't additive.
**Proposal.** `501` for "this server doesn't have that feature" on every
route, credentials included, so `404` only ever means "no such thing".
**Breaking:** for a client that treats the credentials `404` as "no
vault"; check loomux/web's credentials page before deciding.
**Recommendation:** decide before 1.0; impossible after.

## E. Missing, but addable after 1.0 (not blockers)

These can all be added after 1.0 without breaking anything:
- `GET /targets/{id}`, `GET /workspaces/{id}` and `GET /tasks/{id}` (today
  clients filter lists).
- `POST /workspaces` (today workspaces are only created through chat).
- Filters on `GET /conversations` (by workspace, by status).
- A `relay` control in the web client (waits on the UI direction).

## How to proceed

For each item marked **decide**, pick one; the "before 1.0" ones need a
PR each (server, plus web where noted), merged before the 1.0.0 release
PR. Breaking changes before 1.0 are allowed but must be recorded with
`go test ./api -run TestAPIv1Contract -update` and called out under
`### Changed`/`### Removed` in their changes fragment
(`docs/release/versioning.md`).
