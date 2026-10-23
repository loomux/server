### Added

- Created machines can stop when idle (LOOM-217). With `LOOMUX_MACHINE_IDLE_STOP` set (for example `4h`; off by default), a persistent machine a target-provider plugin made is stopped at the next reconcile once nobody has used it for that long. "Used" means a task still unfinished there, a task's last change, its last start or its last dispatch. Its status reason says it was stopped for being idle. The next request to it starts it again, with its data. Ephemeral machines are never stopped this way.
