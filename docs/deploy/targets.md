# Preparing a target: operator checklist

Do these once per execution target (a host Loomux runs agents on, e.g.
sc1 or jet01) before dispatching agent work to it. Loomux sets
everything else **per launch**, on the command line, and never edits the
target's own agent config (`~/.claude.json`, `~/.claude/settings.json`,
`~/.codex/config.toml`). Targets can be shared work hosts.

- [ ] **SSH access works** from the Loomux pod as the target user. See
  `ssh.md`.
- [ ] **Agent CLIs are installed and logged in** as the target user.
  Loomux probes for `claude`/`codex` before each launch and resolves an
  absolute path, so `~/.local/bin` doesn't have to be on the
  non-interactive `PATH`. If a CLI is missing, it offers an install,
  which runs only after you confirm. Logging in is always manual: run
  `claude` once and complete `/login`, and run `codex login`. Versions
  must be at least claude **2.1.0** and codex **0.150.0**; older ones
  are refused at launch (see `agents/README.md`).
- [ ] **Trust the workspace root in Claude Code, once.** Claude Code
  stops at "Do you trust the files in this folder?" in any directory that
  isn't trusted, with **"No, exit"** pre-selected. It has no per-launch
  flag to skip the prompt, and Loomux doesn't edit `~/.claude.json`. A
  directory counts as trusted when one of its ancestors is trusted, so do
  this once on the target:

  ```sh
  mkdir -p <workspace-root> && cd <workspace-root> && claude
  # choose "Yes, I trust this folder", then /exit
  ```

  `<workspace-root>` is the directory Loomux creates workspaces under on
  this target. Every workspace below it is then trusted. To check, run
  `claude` in a fresh subdirectory: it should open with no trust prompt.
  If you skip this step, claude-code tasks in new workspaces sit at the
  trust prompt until someone attaches and answers it. The first message
  waits behind the dialog rather than answering it. Codex needs no setup
  here: Loomux trusts the workspace per launch.
- [ ] **Shared host? Decide about prompts in argv.** By default the
  first message of a task is passed to the agent as a command-line
  argument (it avoids a send-keys race). While the agent runs, **any
  user on the host can read it** with `ps` or `/proc/<pid>/cmdline`. If
  that's not acceptable, set `prompt_as_arg` to `false` for the
  affected agent types in `LOOMUX_AGENT_PROFILES` (for example
  `{"claude-code":{"prompt_as_arg":false},"codex":{"prompt_as_arg":false}}`).
  The first message is then typed into the pane like every later one.
  The setting is global today; per-target policy is LOOM-89.
- [ ] **Nothing to do for completion markers** unless you set
  `LOOMUX_MARKER_DIR`. By default each target uses
  `$HOME/.cache/loomux/completion-markers`, which Loomux creates as
  `0700` and refuses if another user owns it. A configured
  `LOOMUX_MARKER_DIR` is used as-is on every target, so it must be
  writable only by the target user.
