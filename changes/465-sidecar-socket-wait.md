### Fixed

- A sidecar plugin whose socket isn't there yet when loomuxd starts (the sidecar starts a moment later, or is restarting) is no longer left in `error` until someone presses Enable (#465). loomuxd retries the start with backoff (1 s, doubling to 30 s) for up to 10 minutes, the plugin showing `restarting`, "waiting for the sidecar's socket", and adopts a new sidecar version as it would at startup once the socket answers. After 10 minutes the plugin goes to `error` as before. A socket that answers but fails the handshake still fails at once.
