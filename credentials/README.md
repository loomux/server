# credentials

Hybrid credential model. See design spec §7.

OAuth-based agent CLIs (Claude Code, etc.) manage their own session/token
lifecycle already — this component doesn't reimplement that, it only
ensures the pane's environment points at an already-authenticated config
dir. Everything else (GitHub/GitLab PATs, MCP tokens, custom API keys)
goes through an encrypted-at-rest secrets store owned here, scoped per
agent-type/workspace, decrypted only at pane-launch time.

## OAuth-native agent CLIs — documented expectation, no code

- **Local targets**: the agent CLI runs in the same container as the
  Loomux server process itself (design spec §1: "`local` is the Loomux
  server's own container"), so it inherits the same `HOME` and finds its
  normal config dir (e.g. `~/.claude`) without Loomux doing anything.
  Already-authenticated is a precondition, not something Loomux manages.
- **Remote targets**: design spec §1 says OAuth-authenticated agent CLIs
  "are expected to already be logged in locally on machines you own" —
  the SSH-connected user's own default config dir must already hold
  valid session state. Loomux doesn't push or manage it.

Actually wiring either of these into a real launch happens wherever
`orchestrator.Launch` gets called from (a future entrypoint or the
router) — nothing in this package does that composition.

## The vault (everything else)

`registry.Store` (`registry/store.go`) is extended with credential CRUD
(`CreateCredential`/`GetCredential`/`ListCredentials`/`DeleteCredential`)
— per design spec §8, the storage interface covers both the registry
tables and the credential vault as one abstraction, so this isn't a
second storage mechanism. `registry/sqlite` encrypts values with
AES-GCM, bound to the row's id as additional data so a ciphertext
copied onto another row doesn't decrypt (LOOM-175), before they touch
the database and decrypts on read (the master
key is a `registry/sqlite.Option` — see `sqlite.WithMasterKey` /
`sqlite.KeyFromEnv` — not a required parameter, since most callers never
touch credentials).

This package (`credentials/`) is the resolution layer on top of that
storage:

- `Resolver.Resolve(ctx, workspaceID, agentType)` — returns the
  decrypted secrets applicable to a workspace + agent-type pair,
  applying scope precedence (most specific wins: workspace+agentType >
  workspace-only > agentType-only > global; workspace-scoped wins over
  agentType-scoped when both are single-dimension matches for the same
  name — see the doc comment on `Resolve` for the full rule).
- `ShellEnvPrefix(secrets)` — turns a resolved secret map into a
  POSIX-shell-safe `"VAR='value' "` prefix ready to prepend to a launch
  command. Rejects any secret name that isn't a valid shell identifier —
  the name itself can't be quoted the way a value can.

**Neither is wired into a live `orchestrator.Launch` call** — that
composition belongs to whichever ticket actually builds a real launch
call site (router / a future entrypoint), not here.

Run `go test ./...` from the repo root to run the full suite, including
a real-shell round-trip test proving `ShellEnvPrefix`'s output is
genuinely safe to execute, not just plausible-looking.
