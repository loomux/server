### Fixed

- `POST /api/v1/dispatch` refuses a `message` over 36 KiB with `413`
  (`too_large`) at once (LOOM-155). Such a message was stored and sent to
  the router model before the router refused it as over the 32 KiB an
  agent is sent; now nothing runs and no model call is spent on it.
