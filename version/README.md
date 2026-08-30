# version

`loomuxd`'s own release version (design spec §10 axis 4) — a single
`var Version = "dev"`, overridable at build time:

```sh
go build -ldflags "-X github.com/Loomux/server/version.Version=1.2.3" ./cmd/loomuxd
```

Deliberately independent of `api.APIVersion` (axis 1, the `v1` in
`/api/v1/...`): a server patch release doesn't force an API version bump,
only a breaking API change does. `loomuxd -version` prints it; the HTTP
API also exposes it (unauthenticated) via `GET /api/v1/version` alongside
`api.APIVersion`, so clients can check compatibility before logging in.
