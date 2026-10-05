# Versioning and releases (LOOM-129)

Loomux follows [Semantic Versioning 2.0.0](https://semver.org/spec/v2.0.0.html).
loomux/server and the loomux/web client it pins are one product with
one version.

## The scheme

- **0.y.z while the API and database schema still change.** SemVer
  allows anything to change before 1.0.0. MINOR (0.y) is for features
  or breaking changes, PATCH (0.y.z) for fixes. The first release is
  **v0.1.0**.
- **v1.0.0-rc.N** once the release bar (`docs/release/v1.0.0.md`) is
  met, then **v1.0.0**. From 1.0.0 on, a breaking change to the API
  (`/api/v1`) or an upgrade path that needs manual work bumps MAJOR.
- The API version (`v1` in the path) is separate: a server release
  doesn't change it unless the API breaks.
- Every 0.y.z and every `-pre` version is published as a GitHub
  **pre-release**, never marked "latest". v1.0.0 and the production
  redeploy need the user's explicit go.

## What a version looks like where

| Where | Example |
|---|---|
| git tag (both repos) | `v0.1.0` |
| server image | `ghcr.io/loomux/server:0.1.0`, plus the commit tag `:<sha>` as today |
| `GET /api/v1/version` → `server_version` | `0.1.0 (web 1327591)`; between releases `<sha> (web <sha>)` |
| `loomuxd -version` | the same |
| web `package.json` `version` | `0.1.0` |
| changelogs | `CHANGELOG.md` in each repo, Keep a Changelog |

## Cutting a release

1. **Web first, if it changed.** In loomux/web:
   - move `CHANGELOG.md`'s `[Unreleased]` to `[X.Y.Z] - <date>` and set `package.json` `version`;
   - merge;
   - tag that merge commit `vX.Y.Z` (its `web-<sha>` release is the bundle).
2. **Server.** In a PR:
   - bump `deploy/web-ref` and `deploy/web-sha256` to that commit (hash the downloaded tarball);
   - move `CHANGELOG.md`'s `[Unreleased]` to `[X.Y.Z] - <date>`.
   Get it reviewed and merged.
3. **Tag** the server's merge commit `vX.Y.Z` and push the tag. The `image`
   workflow refuses a tag that isn't SemVer, isn't on `main`, or has no
   CHANGELOG section (`deploy/release.sh`). It pushes
   `ghcr.io/loomux/server:X.Y.Z`, and the `release` job creates the GitHub
   release from the CHANGELOG section (a pre-release for 0.y.z and
   `-pre` versions).
4. **Deploy** to the test instance:
   `loomux-deploy-test-instance <sha> "vX.Y.Z"` (it pins the commit tag,
   which is the same image).
