### Fixed

- A plugin's `targets.changed` notification no longer outlives the server's shutdown (LOOM-196): the reconcile it starts is cancelled and waited for when the providers manager closes, so it never uses the database after it has closed, and one arriving after that starts nothing.
