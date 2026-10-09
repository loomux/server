# The agent image (LOOM-178)

`deploy/agent/Dockerfile` builds **`ghcr.io/loomux/agent`**: what runs
inside a machine a plugin makes (`docs/design/target-providers.md` §3).
It is not the server image. A plugin's configuration names the image it
creates machines from (`agent_image`), pinned by tag or digest the way
theWyseKube's manifests pin the server image; an update is a recreate.

## What is in it

| | |
|---|---|
| Base | `node:22-bookworm-slim` (Claude Code requires Node 22 or later; the CLIs ship native binaries, so glibc) |
| Agent CLIs | `@anthropic-ai/claude-code`, `@openai/codex`, `opencode-ai`, each pinned to an exact version in the Dockerfile's `ARG`s, above the floors in `agents/README.md`; `DISABLE_AUTOUPDATER=1`, so what runs is what was tested |
| Tools | `tmux`, `openssh-server`, `git`, `python3` (the folder-trust script Loomux runs before a Claude Code launch), `ripgrep`, `jq`, `curl`, `procps`, CA certificates, tzdata |
| Not in it | Loomux itself, any `LOOMUX_*` variable, Docker or cloud CLIs, compilers. An agent that needs one asks in chat and the install offer runs in its pane |

## The user and the volume

Everything runs as **`agent`, uid/gid 10002** (distinct from the server's
10001). `$HOME` is `/data/home` and `CLAUDE_CONFIG_DIR` is
`/data/home/.claude`, so a one-time interactive sign-in and Claude Code's
folder trust (which lives in `.claude.json`) survive restarts and image
updates on a persistent machine. Loomux creates workspaces under
`/data/work`, the target's workspace root. The root filesystem is meant
to be read-only; `/tmp` and `/run/loomux` are tmpfs.

The image sets no `WORKDIR` on the volume, on purpose: the runtime
(docker, runc on Kubernetes) creates a missing working directory as
root before the entrypoint runs, which left `/data/work` unwritable on
a fresh volume in the first build. The entrypoint creates `/data/home`
and `/data/work` itself, as `agent`, and refuses to start when `/data`
isn't writable (on Kubernetes the plugin sets `fsGroup: 10002`; a
volume someone else prepared needs ownership 10002), so a bad volume
fails at machine start, not later inside an agent.

## sshd

`sshd` runs **as the agent user** on port 2222 (`deploy/agent/sshd_config`):
public-key authentication only, `AllowUsers agent`, no forwarding of any
kind, `UsePAM no`, `LogLevel VERBOSE` to the container log (the only
place a refused login is visible). It needs two files, which the plugin
mounts read-only at `/etc/loomux/ssh-src`:

| File | Who makes it |
|---|---|
| `host_ed25519` | Loomux: the machine's host key, generated and **pinned on the target before the machine exists**, so there is no scan and no trust-on-first-use. The same key is given to a recreated machine, so the pin never changes |
| `authorized_keys` | Loomux: the target's own managed key, with `no-port-forwarding,no-agent-forwarding,no-X11-forwarding` |

The entrypoint copies them to `/run/loomux/ssh` with mode 0600 (sshd
refuses a world-readable host key, and a Secret volume lands 0644) and
execs `sshd -D -e`. `StrictModes` is off: it checks that no *other* user
could have written `authorized_keys` or a parent directory, and the
machine has no other user (sshd, the entrypoint and every agent process
are uid 10002), while the parent is a tmpfs the plugin mounts, which on
Kubernetes is root-owned and world-writable. On Kubernetes the plugin puts both in a Secret of the
machine's; on Docker it uploads them into the container before start.

## How agents authenticate

Two ways, never a third (design §3): vault credentials scoped to the
target (`POST /api/v1/credentials` with `target_id`), injected into the
pane's environment at every launch like on any target, so
`ANTHROPIC_API_KEY` or a `CLAUDE_CODE_OAUTH_TOKEN` (from
`claude setup-token`), `OPENAI_API_KEY`, a `GITHUB_TOKEN` reach every
agent on the machine; or a one-time interactive sign-in kept on the
data volume. The machine never sees a credential of Loomux's.

## Building and checking

```sh
docker build -t loomux-agent:dev deploy/agent
bash deploy/agent/smoke.sh loomux-agent:dev
```

The smoke test checks the user, the CLIs' versions, `sshd -t`, that the
root filesystem isn't writable, then starts the container the way a
plugin would (read-only, tmpfs, no capabilities, the two files mounted
read-only), logs in over ssh with a generated key and a pinned host key,
starts and kills a tmux session on Loomux's socket, and checks a
stranger's key is refused. `.github/workflows/plugins.yml` runs it on
every change, scans the image with trivy (critical, fixable), and on
`main` publishes `:main` and `:sha-<short>`, on a release tag
`:<version>`, each with a GitHub build-provenance attestation.

## Changing a version

Edit the `ARG`s in the Dockerfile in a PR; the workflow builds and
smoke-tests the result. Keep every pin at or above the floors in
`agents/README.md`, which are the oldest releases on which a real turn
was verified to write its completion marker.
