# Agent image (LOOM-178 PR 3) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `ghcr.io/loomux/agent`, the image a plugin's machines run (design §3): tmux, sshd as the agent user on
2222, the three agent CLIs pinned, `/data` for home and work; built, smoke-tested with a real ssh login,
scanned and published with an attestation by a workflow.

**Architecture:** `deploy/agent/` holds the Dockerfile, sshd config, entrypoint and the smoke script;
`.github/workflows/plugins.yml` builds and tests on every change and publishes on `main` and release tags.
No Go code changes; the server's `Dockerfile` bundles plugin binaries only once a plugin exists (PR 4).

**Tech Stack:** Debian bookworm (node:22-bookworm-slim), OpenSSH 9.2, docker buildx, trivy,
actions/attest-build-provenance.

**Spec:** `docs/design/target-providers.md` §3, §12 "Agent image", §11 row 3.

## Global Constraints

- The image sets no `LOOMUX_*` variable and holds no credential; sshd runs as uid 10002, public-key only, no
  forwarding; the root filesystem works read-only; `/data` and tmpfs are the only writable places.
- Every CLI pinned to an exact version at or above `agents/README.md`'s floors; `DISABLE_AUTOUPDATER=1`.
- Pull requests never push; `main` publishes `:main` + `:sha-*`; a `v*` tag publishes `:<version>`.

## Review Focus

1. The Secret volume's 0644 root-owned files and a root-owned world-writable tmpfs for `/run/loomux`
   (a Kubernetes emptyDir): sshd must still start and accept the key (the entrypoint copies to 0600;
   `StrictModes no`) — covered by the smoke test's read-only 0644 mount and 1777 tmpfs.
2. A stranger's key must be refused; a password prompt must never appear (BatchMode) — smoke test.
3. The login must land in `/data/work` as uid 10002 with tmux on Loomux's socket working — smoke test.
4. No `LOOMUX_*` in the image's environment — smoke test.
5. `/run/sshd` must exist or sshd refuses to start as non-root — Dockerfile creates it; smoke test starts sshd.
6. A fresh, empty volume at `/data`: `/data/work` must end up writable by agent (the runtime creates a
   missing `WORKDIR` as root first, so the image sets none) — smoke test's empty tmpfs + `test -w`.

---

### Task 1: Image, config, entrypoint, smoke script
- [x] Write `deploy/agent/Dockerfile`, `sshd_config`, `entrypoint.sh` (executable), `smoke.sh`.
- [x] `docker build -t loomux-agent:dev deploy/agent && bash deploy/agent/smoke.sh loomux-agent:dev` → `smoke ok`.
- [x] Commit `LOOM-178: the agent image`.

### Task 2: Workflow and docs
- [x] `.github/workflows/plugins.yml`, `docs/deploy/agent-image.md`, `changes/loom-178-agent-image.md`;
  `docs/deploy/container.md` gets a pointer.
- [x] `sh deploy/changelog.sh check`; push; PR #345.
- [ ] Review round 1: actions pinned by commit (trivy's tag was wrong), a separate publish job pushing the
  tested image from an artifact, outputs via `env:`, base image by digest; CI's `agent-image` job green.
