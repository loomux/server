<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/brand/banner-dark.svg">
  <img alt="Loomux" src="docs/brand/banner-light.svg" width="100%">
</picture>

**One chat for your coding agents, across your own machines.** Loomux starts Claude Code or Codex in tmux on
your servers, or on machines it creates on your own Docker or Kubernetes. It tells you when an agent is stuck,
and you can attach to the live session whenever you want.

Self-hosted and MIT-licensed. Not to be confused with loomux.dev, an unrelated commercial product.

<!-- LOOM-310: the demo GIF and screenshots go here (a chat routing a task to a machine, the Approve / Deny /
Reply card, the same session in tmux). -->

## What it is

`loomuxd` is a small server for people who run coding agents in tmux on more than one machine and have too many
panes to watch. You write in a chat, in any browser. A small router model you choose reads the message and picks
the machine and the workspace. Loomux starts the agent there in its normal interactive mode, or passes the
message to the one already running, and the answer comes back into the conversation.

- **Your machines, or machines it makes.**
  - Register any host you reach over SSH. Loomux scans and pins its host key and keeps its own key for it.
  - Or let the bundled Docker or Kubernetes plugin create a container or pod per machine, with a persistent
    volume.
- **Real sessions, not a wrapper.** Agents run as the CLI you already use, in tmux, as the machine's user.
  Attach to any of them and look or type; nothing runs headless.
- **Blocked prompts reach you.** When an agent stops at a screen Loomux recognises (a permission request, the
  agent's own question, folder trust, an expired login, a usage limit), you get a card with **Approve / Deny /
  Reply**, and a push notification through [ntfy](https://ntfy.sh) if you set it up.
- **Policy per machine:** which agents may run there, whether new work needs your confirmation, the agents'
  permission mode, and how much of their output the router model sees (`relay: full | last_message | none`).
- **A credential vault.** API keys and tokens for the agents are stored encrypted and handed to the agent's
  environment at launch, never on a command line.
- **Verifiable images.** The server and agent images carry GitHub build-provenance attestations, and each
  release names its image digests.

## What it isn't

- **Not for teams.** One user, one password, in v1.
- **Not a hosted service.** You run it; nothing passes through a Loomux service. The router-model provider you
  pick, and the agents' own providers (Anthropic, OpenAI), still receive what they process.
- **Not a native mobile app.** The web client works in a phone's browser.
- **Not headless automation.** Agents run interactively, and some of their prompts wait for you.

## Status

**Pre-alpha.** Expect rough edges:

- every release is a pre-release, and the API may still change before 1.0;
- the router model is a third-party API unless you run an OpenAI-compatible endpoint that supports forced tool
  calls (`tool_choice`) yourself;
- the images are Linux/amd64 only;
- the terminal client, [loomux/tui](https://github.com/loomux/tui), isn't usable yet.

## Quickstart

One Linux host with Docker, about 10 minutes to a signed-in agent on a 50 Mbit/s connection. You need a router-model API key (Anthropic, or any
OpenAI-compatible endpoint that supports forced tool calls) and a Claude Code or Codex account. The full
walkthrough, with troubleshooting and how to remove it all, is [docs/quickstart.md](docs/quickstart.md).

> [!WARNING]
> This gives Loomux your Docker engine, which is root on that host. Use a machine or VM you don't mind that for.

```sh
mkdir loomux && cd loomux
curl -fsSLO https://raw.githubusercontent.com/Loomux/server/main/deploy/quickstart/compose.yaml
LOOMUX_VERSION=0.3.155    # the newest release
printf 'Choose a Loomux password: '; stty -echo; read -r PW; stty echo; echo
printf 'Router model API key: '; stty -echo; read -r KEY; stty echo; echo
HASH=$(printf '%s\n' "$PW" | docker run --rm -i ghcr.io/loomux/server:$LOOMUX_VERSION -hash-password)
umask 077
cat > .env <<EOF
LOOMUX_VERSION=$LOOMUX_VERSION
DOCKER_GID=$(stat -c %g /var/run/docker.sock)
LOOMUX_AUTH_PASSWORD_HASH='$HASH'
LOOMUX_MASTER_KEY=$(head -c 32 /dev/urandom | base64)
LOOMUX_ROUTER_PRIMARY_PROVIDER=anthropic
LOOMUX_ROUTER_PRIMARY_MODEL=claude-haiku-5-5
LOOMUX_ROUTER_PRIMARY_API_KEY=$KEY
EOF
unset PW KEY HASH
docker compose up -d
```

Loomux starts at <http://localhost:8080> and creates one machine, `dev`, pulling the 2 GB agent image. Once
**Machines** shows `dev` running, sign the agent in once:

```sh
docker exec -it $(docker ps -qf label=loomux.io/target-name=dev) claude auth login
```

Open the link, approve, then paste the code once and press Enter: it doesn't show as you paste.

Then start a conversation: *"In a new workspace called hello on dev, create hello.txt containing "hi"."* To
watch the agent work, `docker exec -it $(docker ps -qf label=loomux.io/target-name=dev) tmux -L loomux attach`.

Your own servers over SSH, or Kubernetes: [Installing Loomux](docs/deploy/install.md).

## How it compares

- **Claude Code's Remote Control, the Codex app:** they bring one agent session to the vendor's app, through
  the vendor's service. Loomux runs on your hardware, routes one conversation across many machines and both
  CLIs, and can create the machines.
- **Remote-control wrappers such as Happy:** they wrap the CLI and relay it to a phone app. Loomux leaves the
  CLI untouched in tmux, and adds deciding where work goes.
- **tmux dashboards and session managers:** they show and drive the sessions you start. Loomux also starts them,
  on the machine the conversation calls for, and keeps the conversation's history.
- **Coder, Codespaces and other dev-environment platforms:** they make the machines. Loomux makes them too, on a
  much smaller scale, but is mostly about the agents running on them.

## Security

Loomux v1 is **single-user**: one password protects everything. Whoever logs in, or holds a session token,
can:
- start agents and run shell commands on every machine you registered, as that machine's user;
- create and destroy machines through the plugins. The Docker plugin uses its Docker engine, which is root on
  that host.

So **don't expose it to the internet as it is.** Keep it on `localhost`, a VPN or a tailnet, or put an
authenticating proxy with multi-factor login in front: Loomux has no MFA of its own.

What else to know:

- **Logins.** The password is a bcrypt hash; failed logins back off. A login gets a bearer token that stays
  valid for 30 days after its last use and 90 days at most (`LOOMUX_SESSION_TTL`, `LOOMUX_SESSION_MAX_AGE`).
  **Settings → Devices** lists them and logs any one out.
- **Agents run with your accounts, on your machines.** Each agent is the real Claude Code or Codex CLI, signed in
  with your account (or given a key from the vault), running as the machine's user. Their default permission
  modes are Claude Code's `--permission-mode auto` (its classifier lets routine edits and commands through and
  stops risky ones for approval, which Loomux shows you) and Codex's `--ask-for-approval on-request --sandbox
  workspace-write`. A machine can be set stricter (`permission_mode`); Loomux never uses a bypass mode.
- **Machines are not a network sandbox.** A Docker machine reaches whatever its host reaches, LAN and tailnet
  included. A Kubernetes machine gets `internet` egress unless you ask for `none`.
- **The router model sees what you send.** Every chat message goes to the router-model provider you configure,
  and so does the agents' output, as the machine's `relay` says: all of it with `full` (the default), only each
  agent's final message with `last_message`, nothing with `none` (the default only for a machine whose purpose is
  `work`). Pick a provider you're comfortable with.
- **Credentials.** API keys and tokens in the vault, plugin secrets and the machines' SSH keys are encrypted with
  AES-256-GCM under `LOOMUX_MASTER_KEY`, each bound to its record and scope (AEAD additional data). A credential
  is handed to the agent's environment at launch, so the agent can read it. Known values are redacted from what
  Loomux relays and stores, as a best effort, not a guarantee.
- **What is checked.** SSH host keys are pinned: scanned and confirmed by you for your own hosts, generated and
  pinned before a machine a plugin makes exists. The web client is served with a strict Content Security Policy
  and `X-Frame-Options: DENY`, and HSTS behind a TLS proxy that sets `X-Forwarded-Proto: https`. The server and
  agent images carry GitHub build-provenance attestations:
  `gh attestation verify oci://ghcr.io/loomux/server:X.Y.Z -R loomux/server`.
- **Logs** never contain message text, but error text is logged as it is, and can name hosts, users and paths.

Report a vulnerability privately, as [SECURITY.md](SECURITY.md) describes; please don't open a public issue.

## Documentation

| | |
|---|---|
| [Quickstart](docs/quickstart.md) | one Docker host, a machine Loomux creates |
| [Installing](docs/deploy/install.md) | a real deployment: secrets, configuration, TLS in front, checks |
| [Your own hosts](docs/deploy/targets.md), [SSH](docs/deploy/ssh.md) | preparing a machine Loomux reaches over SSH |
| [Plugins](docs/deploy/plugins.md) | machines on Docker or Kubernetes |
| [Configuration](docs/deploy/container.md) | every environment variable, the image, health probes, metrics |
| [Operations](docs/deploy/operations.md), [backup and restore](docs/deploy/backup-restore.md) | upgrades, rotation, restores |
| [Design](docs/design/core-design.md) | how it works and why, decision by decision |

## Project

| Repository | What it is |
|---|---|
| [loomux/server](https://github.com/loomux/server) | `loomuxd`: the API, routing, the agents' tmux sessions, plugins, the images |
| [loomux/web](https://github.com/loomux/web) | the web client, served by `loomuxd` |
| [loomux/tui](https://github.com/loomux/tui) | `loomux`, the terminal client (in progress, not usable yet) |

Each directory of this repository is a component of the design, with a README saying what belongs there.
Contributing: [CONTRIBUTING.md](CONTRIBUTING.md). License: [MIT](LICENSE).
