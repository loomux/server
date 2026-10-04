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

## Launch profiles (LOOM-78)

A fresh interactive agent used to stop at its trust dialog or at a
per-tool approval prompt. Its first message was typed in with send-keys
before the TUI was ready: Claude Code's trust dialog has **"No, exit"**
pre-selected, so a typed message plus Enter could quit the agent. Each
adapter now carries a `router.LaunchProfile`. Every part of it is passed
on the command line, for that launch only:

```
<binary> <permission args> <trust args> <completion hook args> -- '<first message>'
```

### Defaults

| | `claude-code` | `codex` |
|---|---|---|
| Permission args (mode `auto`) | `--permission-mode auto` | `--ask-for-approval on-request --sandbox workspace-write` |
| Runs without asking | Whatever Claude Code's auto-mode classifier judges routine: edits and ordinary commands. | Commands inside Codex's sandbox: read anywhere, write inside the workspace and temp dirs, **no network**. |
| Still asks a human | Anything the classifier flags as risky. | Anything the model wants to run outside the sandbox (network, writes elsewhere). |
| Workspace pre-trust | Before each launch, `hasTrustDialogAccepted` for exactly that workspace path in `~/.claude.json` (see below). | `-c 'projects={"<workspace>"={trust_level="trusted"}}'` for this process. Nothing is written to `~/.codex/config.toml`. |
| First message | Positional argument after `--`. | Positional argument after `--`. |

Agents run in each CLI's own **automatic** mode, never the "skip
everything" modes (`--dangerously-skip-permissions`, `bypassPermissions`,
`--dangerously-bypass-approvals-and-sandbox`); user decision 2026-10-04. A
target can tighten this with `permission_mode` (`auto`, `accept-edits` or
`manual`; empty means the agent-type's default, `auto`). Each agent-type
maps the modes to its own flags (`LaunchProfile.PermissionModes`), so
per-target policy (LOOM-89) can restrict, say, a work-only host. Anything
a mode still asks about stops at a prompt in the pane. LOOM-97 surfaces
those in chat: `DetectPrompt` (prompts.go) reads the prompt off the pane,
the task becomes `needs-attention`, and the next chat message answers it
(see the design spec's failure modes). `testdata/` holds the Claude Code
screens it is tested against, captured from a real target. The Codex ones
are reconstructed, not captured, so re-check them against a real Codex.

**Claude Code workspace trust.** Claude Code has no per-launch flag to
trust a folder; trust lives in the user's `~/.claude.json`. Before every
launch Loomux runs a small Python 3 script on the target
(`claudeTrustCommand`) that sets `projects["<realpath of the workspace>"]
.hasTrustDialogAccepted = true`. It keeps every other key, writes
atomically and keeps the file's mode. This is the one write Loomux makes to
an agent's own config (approved 2026-10-04 for this narrow purpose). It is
scoped to that exact path, never a global trust, and covers both directories
Loomux creates and existing ones a workspace is attached to. It needs
`python3` on the target. If it fails, the launch goes ahead and the trust
dialog shows in the pane.

**First message on the command line.** Passing the first message as an
argument removes the send-keys race on turn 1. Turns 2 and later are
still typed in, after the previous turn's completion marker. That marker
shows the agent is waiting for input. **Trade-off: on a shared host
the first message is visible to other users.** While the agent runs, the
message is in its argv, which any user on the target can read with `ps`
or `/proc/<pid>/cmdline`. Where that matters, set `prompt_as_arg` to
`false` for that agent type in `LOOMUX_AGENT_PROFILES` (e.g.
`{"claude-code":{"prompt_as_arg":false}}`), and the first message is
typed into the pane like the rest.

### Overrides

`LOOMUX_AGENT_PROFILES` (read by `app.LoadConfig`, applied by
`router.AgentTypeRegistry.ApplyProfileOverrides`) is a JSON object keyed
by agent type. Every field is optional; an absent field keeps the
default.

```json
{
  "claude-code": {"permission_args": ["--permission-mode", "plan"]},
  "codex": {"permission_args": ["--ask-for-approval", "untrusted", "--sandbox", "read-only"],
            "pre_trust": false,
            "prompt_as_arg": false}
}
```

- `permission_args`: replaces the default list. `[]` means no flags (the
  CLI's own interactive default).
- `pre_trust: false`: drops the trust args. `true` can't add a trust
  mechanism to an agent that has none.
- `prompt_as_arg`: on or off.

Unknown keys and unknown agent types fail startup, so a typo can't leave
the default posture in force. The effective profiles are logged at
startup (`agent launch profile`). Per-target overrides (a stricter
posture on a shared host than on a dedicated one) are LOOM-89.
