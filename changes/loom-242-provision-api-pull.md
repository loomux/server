### Fixed

- A workspace provisioned through `POST /api/v1/workspaces` on a stopped machine that is still pulling its image no longer fails at 4 minutes (LOOM-242). Starting and checking the target now waits out the pull, as a chat dispatch has since LOOM-222: up to 40 minutes, then the recipe's own 10. The reaper that fails provisioning left behind by a restart now counts 15 minutes from the recipe's start. Before the recipe starts, it allows 45 minutes from the workspace's creation. So it never fails a provisioning that is still live.
