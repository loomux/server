### Added

- The agent image `ghcr.io/loomux/agent` (LOOM-178,
  `docs/deploy/agent-image.md`): what runs inside a machine a plugin
  makes. tmux, sshd and the agent CLIs (Claude Code, Codex, opencode,
  pinned, auto-updates off) on Node 22, as user `agent` (uid 10002) with
  `/data` for workspaces and home, reached over SSH on port 2222 with
  the Loomux key and the host key Loomux generated. Built, smoke-tested
  (a real ssh login), scanned and published with a build-provenance
  attestation by the `plugins` workflow.
