# syntax=docker/dockerfile:1
#
# The agent image (LOOM-178, docs/design/target-providers.md §3): what
# runs inside a machine a plugin makes. Published as ghcr.io/loomux/agent.
#
# Not the server image. This one holds tmux, sshd and the agent CLIs,
# runs as user "agent" (uid 10002) with /data as its only writable place
# besides tmpfs, and is reached over SSH on port 2222 with a Loomux key.
# It carries no Loomux code and no credential of Loomux's: the two files
# sshd needs (the host key Loomux generated, the authorized_keys line of
# the target's key) are mounted at /etc/loomux/ssh-src by the plugin and
# copied into a private directory by the entrypoint.

FROM node:22-bookworm-slim

# Pinned exactly, above the floors in agents/README.md (claude 2.1.0,
# codex 0.150.0): a version change is an image change, reviewed as a PR.
ARG CLAUDE_CODE_VERSION=2.1.295
ARG CODEX_VERSION=0.162.0
ARG OPENCODE_VERSION=1.18.35

# openssh-server: sshd. tmux: what Loomux drives. git, python3 (the
# folder-trust script Loomux runs before a Claude Code launch), ripgrep,
# jq, curl, procps: what agents reach for. /run/sshd: sshd's privilege
# separation directory, which the package creates only at service start.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
        ca-certificates tzdata curl git tmux openssh-server python3 ripgrep jq procps \
 && rm -rf /var/lib/apt/lists/* \
 && mkdir -p /run/sshd

RUN npm install -g --no-fund --no-audit \
        "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}" \
        "@openai/codex@${CODEX_VERSION}" \
        "opencode-ai@${OPENCODE_VERSION}" \
 && npm cache clean --force

# uid/gid 10002, distinct from the server's 10001 so a misconfigured
# volume can never be read across. $HOME is on the data volume, so a
# one-time interactive sign-in and Claude Code's folder trust
# (CLAUDE_CONFIG_DIR puts .claude.json there too) survive restarts.
RUN groupadd -g 10002 agent \
 && useradd -u 10002 -g 10002 -d /data/home -s /bin/bash -M agent \
 && mkdir -p /data/home /data/work /run/loomux /etc/loomux \
 && chown -R agent:agent /data /run/loomux

COPY sshd_config /etc/loomux/sshd_config
COPY entrypoint.sh /usr/local/bin/loomux-agent-entrypoint

# DISABLE_AUTOUPDATER: the CLIs never update themselves; the image's
# pin is what runs. No LOOMUX_* variable: the machine knows nothing of
# the server.
ENV HOME=/data/home \
    CLAUDE_CONFIG_DIR=/data/home/.claude \
    DISABLE_AUTOUPDATER=1

# No WORKDIR on the volume: the runtime (docker, runc on Kubernetes)
# creates a missing working directory as root before the entrypoint
# runs, which would leave /data/work unwritable on a fresh volume. The
# entrypoint creates /data/home and /data/work as agent instead.
USER agent
EXPOSE 2222
ENTRYPOINT ["/usr/local/bin/loomux-agent-entrypoint"]

LABEL org.opencontainers.image.title="loomux-agent" \
      org.opencontainers.image.description="Loomux agent machine: tmux, sshd and the agent CLIs, for machines a plugin makes" \
      org.opencontainers.image.source="https://github.com/Loomux/server" \
      org.opencontainers.image.licenses="MIT"
