### Added

- A one-machine quickstart (LOOM-309): `docs/quickstart.md` and `deploy/quickstart/compose.yaml` run Loomux on one Linux Docker host, with the bundled Docker plugin on the host's own engine and one persistent machine, `dev`, declared in the plugins seed, so there is no SSH host to prepare. `docs/deploy/install.md` now points at it, shows current `X.Y.Z` tags instead of `0.1.N`, and says the Kubernetes manifests it describes aren't published.
