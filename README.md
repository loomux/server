<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/brand/banner-dark.svg">
  <img alt="Loomux" src="docs/brand/banner-light.svg" width="100%">
</picture>

# Loomux — server

Chat-driven interface to on-demand, tmux-backed coding agents, with a
lightweight swappable router model deciding routing/relay between the chat
and the agent fleet. A human can SSH into wherever an agent is running and
attach to its tmux session directly, without breaking the automated side.

**Status: pre-alpha — domain layer (registry, targets, orchestrator,
completion detection, credentials, router, LLM-backed routing model,
task-continuation) plus a client-facing HTTP API with login-gated auth are
wired into a runnable `cmd/loomuxd` process. Web/Android/iOS clients
themselves remain future, separate work.**

## Design

The core design spec lives at [`docs/design/core-design.md`](docs/design/core-design.md).
It covers the workspace registry, execution targets (local/remote via SSH),
the tmux interaction and takeover model, tiered completion detection, the
hybrid credential model, pluggable storage, client auth, and versioning.

Explicitly deferred to their own future specs: the web/Android/iOS clients,
the specific router-model vendor, and concrete agent-type adapters beyond
the interface they implement.

## Deployment

`loomuxd` ships as a container image, `ghcr.io/loomux/server` — see
[`docs/deploy/container.md`](docs/deploy/container.md) for how it's built
(including why the runtime base is not `scratch`/distroless, and how the
web client's static bundle is sourced), its configuration, and its health
probe.

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

## Layout

Each top-level directory corresponds to a component boundary from the
design spec — see each directory's own README for what belongs there and
which spec section it implements.

## Tracking

- Vikunja project `Loomux` (command-center instance)
- Plane project `LOOM` (command-center instance), epic "Loomux — Chat-Driven
  Multi-Agent Orchestration Platform"
