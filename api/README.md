# api

Server-to-client API surface and auth. See design spec §9, §10.

Single-user client auth (login-gated, not network-position trust) for the
web/Android/iOS clients (each their own future repo/spec). The API is
explicitly versioned (`/api/v1/...`) with clients declaring what they
expect, independent of the server's own semver release version.
