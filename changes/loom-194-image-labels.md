### Added

- Every published image (`ghcr.io/loomux/server`, `agent`, `plugin-kubernetes`, `plugin-docker`) carries OCI labels naming its version, commit, source and what it is (the agent image no longer claims to be the server), and the server image gets a GitHub build-provenance attestation like the plugin images (LOOM-194). The agent image also labels its contract (`io.loomux.agent.contract`) and its CLI versions.
