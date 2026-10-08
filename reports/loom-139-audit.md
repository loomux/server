# LOOM-139: audit of Loomux (correctness, security, UX)

Tracked in Vikunja Loomux #106, Plane LOOM-139.

## What was tested

| | |
|---|---|
| Server | 0.3.33, `c148af5` (`ghcr.io/loomux/server:c148af5`) |
| Web | 0.3.11, `5983a69` |
| Live instance | **TEST, `https://loomux-test.jellyor.net`**, 2026-10-08 08:00–08:30 UTC |
| Code | loomux/server `c148af5`, loomux/web `5983a69` |

All findings apply to that version. A parallel session (LOOM-138) may deploy
to test after this; re-check live-only findings against the version then.

**Method.**
- **Black box against test:**
  - unauthenticated and authenticated sweeps of every `/api/v1` route;
  - input validation, paging, error shapes;
  - static serving (traversal, source maps, bundle secrets, headers);
  - the login throttle (two wrong attempts, no brute force);
  - DOM checks of every route in a signed-in Chrome tab.
- **Code review** of server and web.
- **Local reproduction:** every testable finding was reproduced with a Go test or a Playwright test against a throwaway local loomuxd (the web repo's e2e harness).

**Not covered.**
- **Visual screenshots on the live instance.** The Chrome tab was in the background, so `captureScreenshot` timed out. Visual and a11y checks at phone and desktop sizes, in both themes, ran in Playwright instead (`e2e/specs/audit.spec.ts`).
- **Restart mid-turn.** Not repeated live. It is covered by the code review (R2, R3 below).
- **Load or DoS testing.** Out of scope.
- **SSH injection on a live target.** Done in Go tests only.

**Two process findings for command-center:**
1. **The LOOM-139 brief named `https://loomux.jellyor.net` as the TEST instance. That host is PROD** (it answers `0.2.0`). Test is `loomux-test.jellyor.net` (`theWyseKube-loomux/services/base/loomux/40-loomuxd.yaml`). Before I noticed, I sent four unauthenticated read-only requests to prod: `GET /api/v1/version`, `GET /api/v1/health`, `HEAD /` and one `OPTIONS /api/v1/login` (which returned 405). Nothing else touched prod.
2. **`sc1` (the work cluster) is a registered target on the TEST instance**, with `allow_shell: true` and `allow_provision: true` (`require_confirmation: true`, relay `none`). A misrouted test message can offer to run something there, and the health prober SSHes into it on every sweep. I suggest removing it from test, or at least setting `allow_shell`/`allow_provision` to false there.

**Test data left on test.** Conversation `not-a-uuid` (one direct answer, "hi"). It was created by the F15 probe and can't be deleted: there is no conversation-delete endpoint (F15).

## Findings, by severity

Severity reflects a single-user, self-hosted tool behind a reverse proxy. "Test" names the regression test that pins the finding:
- **Go:** `LOOMUX_AUDIT_XFAIL=1 go test ./... -run TestAudit_` runs the skipped ones.
- **Playwright:** `test.fail()` marks the open bugs.

### High

#### F01 — An idempotent retry of a new conversation's first message fails with 422 and leaves an orphan turn
- **Where:** `dispatch/service.go:193-211`. `Submit` assigns `req.ConversationID = uuid.NewString()` before `requestHash(req)`, so a retry with the same `Idempotency-Key` hashes differently and gets `ErrKeyReused`.
- **Effect:**
  - The design says to reuse the key on a network retry (`docs/design/async-dispatch-design.md:197`). A retry of a new conversation's first message gets **422 `idempotency_conflict`**.
  - The first dispatch keeps running in a conversation whose id the client never received.
- **Repro:** POST `/api/v1/dispatch?wait=true` twice with body `{"message":"x"}` and the same `Idempotency-Key`. The second returns 422.
- **Evidence:** `TestAudit_Dispatch_IdempotentRetryOfNewConversation` → `statuses = 200, 422`.
- **Fix:** hash the request before defaulting the conversation id, and return the stored row's id on a match.
- **Test:** `api/audit_loom139_test.go`.

#### F02 — Agents on a *local* target inherit all of loomuxd's secrets
- **Where:** `targets/local.go:21-22, 41, 140` set no `cmd.Env`, and `deploy/entrypoint.sh:68` execs loomuxd with the full environment.
- **What leaks:** the first `tmux -L loomux new-session` starts the tmux server with loomuxd's env, so every local pane sees `LOOMUX_MASTER_KEY`, the router API keys, `LOOMUX_NTFY_TOKEN`, `LOOMUX_WEB_RELEASES_TOKEN` and the password hash.
- **Effect:** an agent in auto mode (or one prompt-injected by a repo) can run `env`. As the same uid, it can then read the SQLite DB, decrypt every vault value in every scope, read `~/.ssh` (a pivot to every remote target), or write itself a session row.
- **Scope:** the test instance has no local target today; the e2e harness and single-box installs do.
- **Fix:**
  - Read secrets into memory and then `os.Unsetenv` them.
  - Give the local executor an allow-listed `cmd.Env`.
  - Document that a local target is fully trusted, or run local agents as another uid.

### Medium

#### F03 — Parallel requests bypass the login backoff
- **Where:** `api/server.go:416-433`. `wait()` is checked, then bcrypt runs, then `recordFailure()`. Nothing is reserved between the check and the record, so every request that arrives in the same window gets its own guess.
- **Evidence:** `TestAudit_LoginThrottle_ConcurrentGuessesShareOneWindow`: **15 of 20** concurrent wrong passwords were checked (401) instead of 1.
- **Fix:** reserve the attempt under the mutex. Either add an in-flight flag (429 while one is running), or record the attempt time before bcrypt.

#### F04 — No browser security headers, while the bearer token sits in localStorage
- **Live evidence (test, `HEAD /` and `/api/v1/*`):** no `Content-Security-Policy`, `X-Frame-Options`/`frame-ancestors`, `Strict-Transport-Security` or `Referrer-Policy`. `X-Content-Type-Options` is present only on error responses.
- **Token storage:** the web client keeps a 30-day sliding token in `localStorage["loomux.token"]`.
- **Effect:**
  - Any XSS (a markdown regression, a dependency compromise) can read the token and send it anywhere. Nothing restricts `connect-src`.
  - The app can be framed, so Approve, Delete and web update can be clickjacked.
- **Fix:** add middleware to `Server.ServeHTTP` that sets:
  - `Content-Security-Policy: default-src 'self'; script-src 'self' 'sha256-<theme snippet>'; connect-src 'self'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; object-src 'none'`
  - `X-Content-Type-Options: nosniff`
  - `Referrer-Policy: no-referrer`
  - `Cache-Control: no-store` on `/api/v1`
  - HSTS, or a documented requirement that the proxy sets it.

  `index.html` has one inline theme script; hash it.
- **Tests:** `TestAudit_Static_SecurityHeaders`; web `audit.spec.ts` "served with CSP".

#### F05 — Logout, revoke and expiry don't end open event streams
- **Where:** `api/server.go:1101-1253`. `requireAuth` runs only at connect.
- **Effect:** a stream opened with a token that is later revoked keeps delivering `message_added` and `dispatch_update` events, which include reply text, indefinitely. "Log out this device" in Settings doesn't cut off a stolen token's open stream.
- **Evidence:** `TestAudit_Stream_StopsAfterSessionRevoked`: the event was delivered after logout.
- **Fix:** re-validate the session on each heartbeat (about every 15s), and close the stream when the session is gone or expired.

#### F06 — Each open stream re-reads the whole tasks table every 500 ms
- **Where:** `api/server.go:1101-1253`, `registry/sqlite/sqlite.go:698`.
- **Cost:** every tick runs `ListTasks()` (every task row, with `output_tail`), plus every message and every dispatch of the conversation. Messages can be up to 1 MiB each (F16).
- **Effect:** a few tabs on a long conversation create steady SQLite load that competes with dispatch writes and makes F07 more likely.
- **Fix:** query by conversation (`ListTasksByConversation`), and read messages and dispatches after a cursor.

#### F07 — A failed dispatch bookkeeping write blocks the conversation until the next restart
- **Where:** `dispatch/service.go:288-293, 316-325`. If `TransitionDispatch` fails (busy timeout, disk full), the error is only logged. The job leaves the map, but the row stays `queued` or `running`.
- **Effect:**
  - Every `POST /dispatch` to that conversation gets 409 `conversation_busy`.
  - Cancel answers "isn't running".
  - `GET /dispatches/{id}` says `running` forever.
- **Fix:** retry the terminal write with backoff. Failing that, have a periodic sweep interrupt non-terminal rows that have no live job.

#### F08 — Shutdown doesn't drain: `main` exits as soon as `Shutdown` starts
- **Where:** `cmd/loomuxd/main.go:145-166`. `ListenAndServe` returns `ErrServerClosed` as soon as `Shutdown` is called, so `runServer` returns and the deferred `loomux.Close()` closes the store under in-flight handlers.
- **Effect:**
  - The 10 s grace window never happens.
  - A `?wait=true` handler released by the drain can 500 or get truncated.
  - SSE streams never go idle, so a working `Shutdown` would always use the full 10 s.
- **Fix:** wait for `Shutdown` to return before returning from `runServer`, and cancel streams via `RegisterOnShutdown` or `BaseContext`.

#### F09 — Web: a stale 401 deletes a newer token, including across tabs
- **Where:** `src/lib/auth.tsx:36-45`, `useApiClient.ts:17-25`, `api.ts:657-662`. `handleUnauthorized` removes whatever token is stored, not the token that was rejected.
- **Repro:**
  1. Tab A logs out, then logs back in (new token T2).
  2. Tab B's 15 s poll fails with 401 on the old token T1.
  3. Tab B wipes T2 from storage.

  Tabs also never sync login or logout: there is no `storage` listener.
- **Fix:** clear only if the stored token equals the rejected one, and listen for `storage` events.

#### F10 — Web: a failed send (5xx or dropped connection) loses the draft and can double-run a turn
- **Where:** `src/conversation/useConversation.ts:163-207`. The composer is cleared before the request. On any failure other than 409, `pendingUser` sticks and `onRejected` is never called.
- **Effects:**
  - **Draft lost:** the text is gone from the composer and the bubble has no Retry.
  - **Prompt card hidden:** an agent's prompt card at the end of the thread stays hidden (`ConversationPage.tsx:201`).
  - **Double run:** each send mints a new idempotency key, so retyping after an ambiguous network failure can start the turn twice.
- **Evidence:** `audit.spec.ts` "a failed send gives the draft back": the composer is `""` after an aborted POST.
- **Fix:** on failure, restore the draft and show "Not sent · Retry". Keep the idempotency key per composed message until it is accepted.

#### F11 — Web a11y: the running turn card is a live region that re-announces every second
- **Where:** `src/conversation/TurnCard.tsx:40`. `role="status"` (polite, atomic) wraps a 1 s elapsed timer and the live terminal (`PanePanel`, which re-renders every 1.5 s).
- **Effect:** screen readers re-read the whole card for as long as a turn runs.
- **Fix:** put `role="status"` on the stage label only. Mark the timer `aria-hidden` and keep the terminal out of the region.

#### F12 — The global login backoff lets anyone lock the owner out
- **Where:** the throttle is global by design (`api/throttle.go`, so that it doesn't trust X-Forwarded-For). An unauthenticated attacker sending one wrong password every ≤30 s keeps the backoff at its 30 s cap, and the owner's correct password gets 429 for most of each window.
- **Scope:** existing sessions are unaffected. Losing a device or the session TTL running out then means no access.
- **Fix:**
  - Trust X-Forwarded-For from the configured proxy only.
  - Keep a per-client backoff alongside a global ceiling, or exempt a client whose last login succeeded.
  - At minimum, document the trade-off and the recovery path. The counter is in memory, so restarting loomuxd resets it, but only until the attacker's next guess.

### Low–medium

#### F13 — A model-chosen clone skips confirmation if its URL is a substring of the message
- **Where:** `router/router.go:444-445`, `strings.Contains(message, spec.GitRemote)`.
- **Repro:** the user writes `…/alice/tools-fork` and the model picks `…/alice/tools`. The clone runs with no confirmation, and the cloned repo's own agent hooks then run.
- **Evidence:** `TestAudit_Provision_CloneConfirmationNotSkippedBySubstring`.
- **Fix:** match the URL as a whole token (split on whitespace and quotes/brackets, trim trailing punctuation).

#### F14 — The provisioned workspace path can be spoofed from clone output
- **Where:** `router/provision.go:110-118` takes the *first* line starting with `loomux-workspace-path:` in the captured pane.
- **Effect:** git sideband `remote:` lines from a hostile server, carrying ANSI escapes (depending on the git version on the target), can render as such a line before the recipe's own line. The agent then starts in, say, `$HOME`, which defeats the root-confinement check meant to keep it out of `~/.ssh`.
- **Fix:** print the path with a random per-run nonce, take the last match, and re-check in Go that the path is under the root.

### Low

| ID | Finding | Where | Evidence / test | Fix |
|---|---|---|---|---|
| F15 | `conversation_id` isn't validated: any string, of any length up to the 1 MiB body, is accepted, including `a/b` (which can't be fetched back through `/conversations/{id}`). Conversations also can't be deleted, so junk accumulates | `api/dispatch.go:146-160`, `dispatch/service.go:193` | **Live on test:** `{"conversation_id":"not-a-uuid","message":"hi"}` → 202, and the conversation now exists. `TestAudit_Dispatch_RejectsMalformedConversationID` | Require a UUID (or `[A-Za-z0-9-]{1,64}`); also bound `workspace_hint`. Consider `DELETE /conversations/{id}` |
| F16 | Message size is checked only deep in the router, after routing calls and even provisioning. A 900 KB message is stored twice, sent in full to the LLM (up to 4 calls), then refused | `api/body.go:17`, `router/router.go:826/840` | `TestAudit_Dispatch_RejectsOversizedMessageUpFront`: 200, and the router was called | 413 at `POST /dispatch` above `MaxPasteBytes` plus a margin |
| F17 | Value redaction iterates a map, so when one vault value is a prefix of another, the longer value's tail leaks | `credentials/redact.go` | `TestAudit_RedactValues_OverlappingValuesNeverLeak`: `token=[redacted]XYZ123456` | Sort values longest first |
| F18 | `CapturePane` without `-J`: a secret wrapped across pane lines escapes exact-match redaction | `targets/remote.go:353`, `targets/local.go:102` | Code | Use `-J`; also match with whitespace stripped |
| F19 | A missing `/assets/*.js` answers 200 with `index.html`. After a web update, a stale chunk gets a MIME error instead of a clean 404 retry | `api/server.go:1326-1370` | **Live:** `GET /assets/nonexistent-XYZ.js` → 200 `text/html`. `TestAudit_Static_MissingAssetIs404` | No SPA fallback under `/assets/` or for paths with a file extension |
| F20 | Web: the SSE reconnect retries at a fixed 1 s forever, with no jitter; it retries 403/404 and keeps going in hidden tabs (`openWhenHidden: true`); each flap refetches history | `src/lib/api.ts:652-690` | `audit.spec.ts`: 11 reconnects in 12 s against a 502 | Exponential, jittered backoff from `onerror` (cap 30 s); stop on 403/404; `openWhenHidden: false` |
| F21 | Web: fetch failures shown as empty states. Machine says "There's no such machine"; Today says "No conversations yet"; **Vault says "Only in a deleted workspace… You can delete it" while workspaces are still loading or failed**, which invites deleting valid secrets; every vault 404 reads "no vault set up" | `MachinePage.tsx:67-77`, `TodayPage.tsx:183-186`, `VaultPage.tsx:22, 239-240` | Code | Separate error from empty; show scope wording only after a successful load |
| F22 | Unknown `/api/v1/*` paths and wrong methods return a plain-text 404/405, not `{error, code}` | ServeMux defaults | **Live:** `GET /api/v1/nonexistent` → `404 page not found`. `TestAudit_UnknownAPIPath_IsJSONError` | Add a JSON catch-all for `/api/v1/` and a 405 wrapper |
| F23 | Unauthenticated `/health` returns the DB ping's `err.Error()`, and reports `degraded` when the DB is down; `/health/deep` probes targets one by one at 10 s each, with no overall deadline | `internal/health/health.go:72-77, 103-121` | Code | Fixed strings; `unhealthy`; parallel probes under one deadline |
| F24 | The startup reconcile and notifier goroutines outlive `App.Close`, causing `database is closed` errors and lost notifications on a quick stop | `app/app.go:507-511`, `app/notify.go:39-60` | Code | WaitGroup plus cancel before `store.Close()` |
| F25 | A host-key scan can be pinned after `PUT` changed the target's host or port (it fails closed, but confuses onboarding) | `api/hostkeys.go:122-164` | Code | Key scans by host:port, or drop them on update |
| F26 | The conversation lock ignores context, so a job blocked on it can't be cancelled | `router/convlock.go:20-41` | Code | Channel lock with a `select` on `ctx.Done()` |
| F27 | SSH ControlMaster sockets live in a predictable `/tmp/loomux/ssh-cm`, with no ownership check (matters on multi-user hosts only) | `targets/remote.go:116-120, 182` | Code | Move them under the data dir, or `Lstat` and check uid and mode |
| F28 | Web: path ids are not `encodeURIComponent`-ed in several API calls; `/conversations/..%2F..%2Fsessions` becomes `GET /api/v1/sessions` | `src/lib/api.ts` (`getConversation`, `openConversationStream`, `deleteWorkspace`, `setWorkspaceStatus`, `updateTarget`, `deleteTarget`, `setCredentialValue`, `deleteCredential`, `getAttachInfo`) | Code | Encode every segment |
| F29 | Web: the attach command omits `ssh_port` (LOOM-114) and doesn't shell-quote the host | `src/lib/attach.ts:5-10`; attach-info has no port | Code | Add the port to attach-info and emit `-p`; quote the arguments |
| F30 | Web: Enter during IME composition sends a half-composed message | `src/conversation/Composer.tsx:62-68` | Code | Ignore Enter while `isComposing` or for keyCode 229 |
| F31 | Web: the app-wide error boundary never resets, so one render error blanks the whole app, nav included, until reload | `RouteErrorBoundary.tsx`, `App.tsx:34` | Code | Key it by pathname, or place it inside the shell's `<Outlet>` |
| F32 | Web: an impossible `/today/:date` silently overflows: `/today/2026-13-45` shows "Sunday, February 14" (2027, with no year shown) | `TodayPage` date parsing | **Live**, and `audit.spec.ts` | Validate the date and show not-found |
| F33 | Web a11y: no focus move or skip link on navigation; input borders at 1.3–1.4:1 against the surface (WCAG 1.4.11 needs 3:1); every weave stitch is a tab stop; DayStrip knots are 22 px (WCAG 2.5.8 needs 24 px); on phones, Vault and Settings are never marked current | `tokens.css` `--line`, `Weave.tsx:200-205`, `DayStrip.tsx:73-90`, `AppShell.tsx:130` | Code | Focus `<h1>` on navigation; a `--line-strong` token; roving tabindex; `aria-current` |
| F34 | Web: `ConversationPage` isn't keyed by id, so its state would carry from A to B (latent: no current link does A→B). Answering the end card during the needs_attention window does nothing, with no feedback | `App.tsx:47`, `ConversationPage.tsx:315` | Code | `<ConversationPage key={id}/>`; `disabled={c.busy}` |

### Info

- The `Authorization` scheme is case-sensitive (`bearer x` → 401 "malformed"; RFC 7235 says it's case-insensitive). **Live.**
- A login body over 1 MiB → 400 "malformed request body", not 413. `readJSON` accepts trailing garbage after the JSON value. **Live.**
- A nonexistent conversation id renders as an empty "New conversation" page, so a stale link silently becomes a new chat. **Live.**
- The Inbox DecisionCard shows "From <prompt>.." with a doubled period when the prompt ends in "."; the card doesn't show what the agent actually asked, so you have to open the conversation. **Live.**
- Target `host` accepts `$ ; \` | & ( )` — safe only because OpenSSH ≥ 9.6 rejects them in `%h`. Restrict it to `[A-Za-z0-9.:\[\]-]`.
- The vault's AES-GCM has no AAD, so with DB write access ciphertexts can be swapped between rows and scopes. Bind the id, name and scope as AAD.
- Sessions have no absolute lifetime (30-day sliding). Expired sessions, dispatches, messages and confirmations have no retention.
- `GET /conversations` loads every task and every conversation's activity, plus one preview query per conversation, on every call. Fine at today's scale, but it grows without bound because nothing is deleted.
- An unconfigured credentials feature returns 404 while every other unconfigured feature returns 501. It is frozen in v1; document it.
- Web: Vault inputs use `autoComplete="new-password"` (password managers may offer to save API keys). Logout doesn't `queryClient.clear()`. The colour tokens use `light-dark()` (Safari 17.5+ or Chrome 123+), with no fallback.

## Checked and fine

- **Auth gating:** every route except login, health and version requires a Bearer token. **Live:** all 34 routes returned 401 without a token. Tokens are accepted only from the header, never from the query string. They are 32 random bytes stored as SHA-256 hashes.
- **CORS:** no CORS headers are set (the preflight from a foreign origin got 405), and header-based auth means there is no CSRF.
- **Error bodies:** generic `{error, code}`. Unknown ids → 404 on every resource. Limits are validated (`1..500`, 400 otherwise).
- **Static serving:** no traversal (`/../../etc/passwd`, `%2e%2e`, `..%2f` all fall back to the shell). No source maps or secrets in the bundle (checked live, every asset). The service worker caches nothing.
- **Web update:** the Sigstore attestation binds SAN, issuer, repo, ref, commit and digest. Pin mode is exact. Tar extraction rejects links, absolute paths and `..`, and caps size. Downloads are sha256-checked and capped at 64 MB.
- **SSH and shell:** every remote argument is single-quoted, `--` precedes the destination, and user and host can't start with `-`. Credentials go through an env file, not argv. Paste uses `load-buffer`.
- **Relay policy:** a run without confirmation needs the whole message to match strictly. Confirmation is matched in Go; offers expire after 15 minutes; `relay=none` output never reaches the model.
- **Markdown rendering:** `javascript:` links, raw HTML, `<script>` and remote images are all inert. `audit.spec.ts` "hostile markdown" passes and now guards this.
- **Phone layout and a11y:** at 390×844, in light and dark, Inbox, Today, a conversation, Machines, Vault and Settings have no horizontal overflow and no serious or critical axe violations (`audit.spec.ts`).
- **Restart reconciliation:** `Recover` and `ReconcileTasks` leave nothing stuck except F07.

## Tests added

- **loomux/server:**
  - `api/audit_loom139_test.go`: F01, F03, F04, F05, F15, F16, F19, F22.
  - `router/audit_loom139_test.go`: F13.
  - `credentials/audit_loom139_test.go`: F17.

  They skip while the bug is open; `LOOMUX_AUDIT_XFAIL=1` runs them.
- **loomux/web:** `e2e/specs/audit.spec.ts`.
  - Two guards that pass today: the phone/axe/overflow matrix in both themes, and hostile markdown.
  - Four `test.fail()` cases: F04, F10, F20, F32.

  Each expected failure was checked to fail for the stated reason.
