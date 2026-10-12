### Fixed

- Pinned host keys' known_hosts files are written to each server instance's own data directory, never a process-wide setting the last start repointed (LOOM-197). The health probe now writes them there too, rather than to a temporary directory.
