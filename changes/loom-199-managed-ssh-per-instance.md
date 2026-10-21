### Fixed

- Managed SSH (the key agents, keys directory and SSH proxy managed targets are reached with) and the local-target switch (`LOOMUX_LOCAL_TARGETS`) are each server instance's own, never process-wide settings the last start overwrote (LOOM-199). Host-key scans and SSH-config migrations use the instance's own proxy too. The agents' sockets and the public-key files are in directories of the instance's own, `$TMPDIR/loomux/agents-<tag>` and `keys-<tag>` (named for its database), so one instance starting a managed connection no longer removes another's agent sockets.
