# agents

The agent-type adapters: what Loomux knows about launching each supported
agent CLI (design spec §6). `app.DefaultAgentTypes` registers them; the
router only sees the resulting `router.AgentType` values.

Rule for every adapter: whatever the agent needs from Loomux is passed
**per launch** (CLI flags, plus the `LOOMUX_*` env vars `router.launchAgent`
sets). Never edit the target's own agent config (`~/.claude/settings.json`,
`~/.claude.json`, `~/.codex/config.toml`). Targets can be shared work hosts
(sc1 is one), and that config belongs to the person who uses them.

## Completion hooks (LOOM-75)

Both agents are `completion.TierMarker`: a turn is finished when
`$LOOMUX_MARKER_PATH` exists on the target. The adapter injects the hook
that creates it:

| agent-type | injected args | fires on | side effects |
|---|---|---|---|
| `claude-code` | `--settings '{"hooks":{"Stop":[…]}}'` | `Stop` (each finished turn) | Layered over the user's settings for this process only. The user's own hooks still run. |
| `codex` | `-c 'notify=["sh","-c","…","loomux-notify"]'` | notify event `agent-turn-complete` | Replaces the user's own `notify` program for this process only. |

The marker directory is created by the router before launch: a per-user
`0700` directory on the target, or the configured `LOOMUX_MARKER_DIR`
(see `completion/README.md`). The hook command (`touchMarker` in
`agents.go`) re-creates it with umask 077 if it has gone missing, then
`touch`es the marker. It does nothing when `LOOMUX_MARKER_PATH` is unset.

`agents_test.go` checks the generated settings and notify values, and
also runs the hook commands under `sh` to confirm they create the marker
(and ignore events that aren't a finished turn).

### Version floor

Each adapter sets a `VersionCheck` with a `Min` version. A CLI below it
fails the dispatch before any task is created, with the error
`agent version too old for completion hooks (…)`. The floors are the
oldest releases on which a real turn was verified to write its marker
(2026-10-02): claude **2.1.0** (verified 2.1.287), codex **0.150.0**
(verified 0.150.1). sc1 had claude 2.1.287 and codex 0.159.3/0.154.0 on
that date. Only lower a floor after verifying an older release.

The check runs `claude --version` / `codex --version` on the target's
non-interactive `PATH`. LOOM-79 (absolute agent paths) changes how the
binary is found; the check should follow it.
