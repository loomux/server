# Agent sign-in: prepared login and auth status (LOOM-316)

**Status:** implemented in the PR for #555, for review · **Ticket:** LOOM-316 (Vikunja Loomux #283 / id 2714, GitHub #555)
**Builds on:** LOOM-86 (agent probe with its auth check), LOOM-97/109 (the sign-in and usage-limit
detectors), LOOM-71 (install offers), LOOM-93 (the orphan sweep).

The user's design (2026-11-20): *"we tmux attach to sign in, the orchestrator opens the agent and
prepares it for login, we can have methods and functions for agent auth status."*

## What main already has (read at `78da37b`)

- **The probe asks the CLI** (LOOM-86, `router/availability.go`). Every probe of an agent CLI (before
  each fresh launch, every 5 min with the target's health, `POST /targets/{id}/probe`) runs its auth
  check through the resolved binary: `claude auth status` (JSON, `"loggedIn"`) and `codex login status`
  (`Logged in using …` / `Not logged in`). Only the classification is kept: `target_agents.auth_status`
  = `logged_in | logged_out | unknown`. The output, which carries the account's email, is never
  stored or logged. `GET /targets/{id}/agents` shows it.
- **Nothing acts on it.** A launch probes, records `logged_out`, then launches the agent anyway.
- **The turn detects sign-in screens** (LOOM-97, `agents.DetectPrompt`): Claude Code's onboarding and
  login-method screens, `Please run /login`, `OAuth token has expired`, `Invalid API key`, Codex's
  `Sign in with ChatGPT`. The task fails with `login_required`, its pane is kept, and the error says to
  attach to *that task's pane* and finish the login. Usage limits fail with `agent_rate_limited` and
  the reset text (LOOM-109). Neither is remembered against the target: the next status read still
  says `logged_in`.
- **Claude Code's pane is not always at a login step** when that happens: after `Please run /login`
  or an expired token it sits at its prompt with an error, and the person has to know to type
  `/login`.

## What this adds

1. **Richer status from the same probe.** The auth check also yields, from an allow-list of fields:
   - Claude Code (`claude auth status`, JSON): `authMethod` (`claude.ai`, `api_key`, `oauth_token`,
     `third_party`, `none`), `subscriptionType` (`max`, `pro`, …) and `loginExpiresAt`.
   - Codex (`codex login status`, text): the method word after "Logged in using" (`chatgpt`,
     `api_key`, `access_token`).

   Stored as `auth_method`, `auth_plan`, `auth_expires_at` on `target_agents`. Every value is
   re-checked against `^[a-z0-9][a-z0-9._-]{0,31}$` (lower-cased) and dropped otherwise, so a CLI that
   changes its output can't put free text there. `email`, `orgId`, `orgName`, key fragments
   (`codex login status` prints part of an API key) are never parsed.
2. **What turns saw.** A turn that hits a sign-in screen records `sign_in_problem`
   (`expired` | `invalid_credentials` | `signed_out`, a fixed word chosen from which marker matched,
   never the pane line) with its time; a usage limit records `limited_at` and the reset text the
   detector already extracts. Both are cleared by the next turn of that agent on that target that
   finishes normally; the problem is also cleared when the sign-in completes (4). Probes never
   overwrite these columns.
3. **Prepared sign-in.** `Router.PrepareSignIn(target, agent type)`:
   - probes the agent; not installed is an error (the install offer is LOOM-71's);
   - already `logged_in` (and no turn-observed problem): returns the status, opens nothing (unless
     `restart`);
   - otherwise opens a tmux session `loomux-signin-<agent type>` on the instance's socket on that
     target, in the target user's home, running the CLI's own sign-in command through the probed
     absolute path: `claude auth login`, `codex login --device-auth`. When that command exits the
     session drops to the user's shell (`exec "${SHELL:-sh}" -l`), so a failed or cancelled login
     can be retried by hand in the same attach;
   - records the session and its start time on the `target_agents` row and returns the attach
     command (`tmux -L <socket> attach -t <session>`) plus the same `attach_commands` list
     attach-info gives (ssh form for SSH targets, plugin forms for machines).

   It is idempotent: a sign-in session already open is returned as is (`reused: true`), so two
   clients, or a client and a dispatch, never kill each other's half-done login. `restart: true`
   kills it and opens a fresh one. One lock per (target, agent type) keeps two callers from both
   creating it.

   Loomux never types into the sign-in session, never captures its pane, never completes the
   browser or device flow and never sees a credential: the person does all of it by attaching.
4. **Cleanup.** The sign-in is done, and its session killed and forgotten, when:
   - a probe (any: pre-launch, periodic, on demand) finds the agent `logged_in` where the previous
     probe had not, with a sign-in session recorded (the person signed in there); or
   - a turn of that agent on that target finishes normally after the session was opened (which
     covers the case where the CLI's own check said `logged_in` all along, an expired token the
     check doesn't see).

   A probe that keeps saying `logged_in` doesn't close a session opened with `restart` (signing in
   to another account): only a finished turn, `DELETE …/sign-in` or the sweep does. An abandoned
   session is a `loomux-` session no task owns, so the existing orphan sweep (LOOM-93) kills it
   after 24 h; the API stops reporting a recorded session older than that.
5. **The dispatch gate.** `launchAgent` already probes right before a fresh launch. When that probe
   says `logged_out`, and the launch would inject none of the agent type's own credential variables
   from the vault (`ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`, `CLAUDE_CODE_OAUTH_TOKEN` for Claude
   Code; `OPENAI_API_KEY`, `CODEX_API_KEY` for Codex), it does not launch an agent that can only stop
   at a sign-in screen. It prepares the sign-in and fails the dispatch at once with
   `login_required`: the message names the target, the attach command for the sign-in session, and
   the API-key alternative. No task row, no workspace pane. Verified on Claude Code 2.1.329: an
   `ANTHROPIC_API_KEY`, `CLAUDE_CODE_OAUTH_TOKEN` or Bedrock in the probe's own environment already
   reads `"loggedIn": true`, so only vault-injected keys need the exception.

   A turn that still reaches a sign-in screen (the token expired since the probe, an API key was
   rejected) fails as today, records the problem (2), and also prepares the sign-in session, whose
   attach command the error now gives first; the task's own pane is kept as before.

## API

All behind the existing session auth. Additive only (API v1).

- `GET /api/v1/targets/{id}/agents` and `POST /api/v1/targets/{id}/probe` (existing): each agent
  entry gains `auth`, omitted for an agent type whose CLI has no auth check or that isn't
  installed:

  ```json
  {
    "agent_type": "claude-code", "available": true, "version": "2.1.329 (Claude Code)",
    "auth_status": "logged_in", "checked_at": "2026-11-20T10:00:00Z",
    "auth": {
      "state": "logged_in | logged_out | expired | rate_limited | unknown",
      "method": "claude.ai", "plan": "max", "expires_at": "2026-12-11T01:04:18Z",
      "problem": {"kind": "expired", "observed_at": "…"},
      "rate_limit": {"observed_at": "…", "resets": "5pm (Europe/Berlin)"},
      "sign_in": {
        "tmux_session": "loomux-signin-claude-code", "started_at": "…",
        "attach_command": "tmux -L loomux attach -t loomux-signin-claude-code",
        "attach_commands": [{"via": "ssh", "command": "ssh dev@jet01 -t 'tmux …'", "argv": ["…"]}]
      }
    }
  }
  ```

  `state` is the one word a client shows: `logged_out` from the probe wins; then a turn-observed
  problem (`expired` for `expired`/`invalid_credentials`, else `logged_out`); then `expires_at` in
  the past (`expired`); then a recorded usage limit (`rate_limited`); then the probe's
  `logged_in`/`unknown`. `auth_status` keeps its LOOM-86 meaning (what the CLI itself said).
- `POST /api/v1/targets/{id}/agents/{agent_type}/sign-in`, body optional `{"restart": true}` →
  `200 {"agent": <the entry above>, "opened": bool, "reused": bool}`. `404` unknown target or agent
  type, `409 agent_unavailable` CLI not installed, `422` agent type has no sign-in command,
  `502 target_unreachable`.
- `DELETE /api/v1/targets/{id}/agents/{agent_type}/sign-in` → `204`, also when none is open.
- Fresh status: `POST /api/v1/targets/{id}/probe` (existing) re-runs every agent's auth check.

## CLI

`loomuxd agents` is a small client of the API above (it never opens the database: `app.Build`
starts the server's sweepers and plugins, which a second process must not run). It reads
`LOOMUX_URL` (default `http://127.0.0.1:8080`) and `LOOMUX_TOKEN` (a session token from
`POST /api/v1/login`):

```sh
loomuxd agents status [-target NAME|ID] [-refresh]     # table: target, agent, state, method, plan, expires, checked
loomuxd agents sign-in -target NAME|ID -agent TYPE [-restart]   # prints the attach command(s)
loomuxd agents sign-in -close -target NAME|ID -agent TYPE
```

The TUI client (`loomux`, its own repo) can grow the same commands later on these endpoints.

## Security

- **No secrets in status output.** Stored and returned: enums, allow-listed short words, a parsed
  timestamp, the usage-limit reset text the detector already stores on tasks, tmux session names and
  attach commands (socket, session, user@host, port: what attach-info already returns). Never: the
  auth check's raw output, email, organisation, account IDs, key fragments, the sign-in pane's
  contents (URLs with OAuth state, device codes). Logs carry the same fields as the response.
- **Loomux never signs in.** No `send-keys`, no paste, no `capture-pane` on a sign-in session. The
  command it runs is fixed adapter configuration; neither the routing model nor an API caller
  supplies or alters it (the agent type is looked up, never interpolated: the session name is the
  agent type reduced to `[a-z0-9-]`).
- **Credentials land where the agents read them.** The session runs as the target's user on the
  same tmux server as the agents, so `~/.claude/.credentials.json` / `~/.codex/auth.json` are the
  ones the next launch uses; nothing is copied to or through Loomux.
- **The trailing shell** gives the person attaching nothing they don't already have: attaching
  needs SSH (or the plugin's exec) to that target as that user.

## Out of scope / follow-ups

- Showing the sign-in URL or device code in the web UI instead of attaching (would mean capturing
  the pane; deliberately not done).
- Routing around a signed-out agent (the routing snapshot could mark it, like an unhealthy target).
- The account's email in status, if the user wants it for telling accounts apart: a decision, since
  LOOM-86 settled "never stored".
- `loomux agents …` in the TUI repo.
