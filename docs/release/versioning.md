# Versioning and releases (LOOM-129)

Loomux follows [Semantic Versioning 2.0.0](https://semver.org/spec/v2.0.0.html).

## The scheme (user decisions, 2026-10-05)

- **Two independent version lines.** loomux/server and loomux/web each
  release on their own merges, so a shared number would drift.
  `GET /api/v1/version` reports both, e.g. `0.1.7 (web 0.1.4)`, and each
  server release says which web version it pins.
- **Every merge to `main` is a patch pre-release**: 0.1.0, 0.1.1, 0.1.2…
  One merged pull request is one release, however many commits it had.
  A web-ref pin bump is just another server merge, so it's a patch too.
- **MINOR (0.2.0, 0.3.0…) is manual,** for a milestone or a breaking
  change: run the `image` workflow on `main` with **bump = minor**
  (Actions → image → Run workflow). It releases that commit as the next
  minor version. Give it a curated `CHANGELOG.md` section first.
- **0.y.z while the API and schema still change.** Then
  **v1.0.0-rc.N** and **v1.0.0**, both pushed as tags by hand and only on
  the user's go.
- Every 0.y.z and `-pre` version is a GitHub **pre-release**, never
  marked "latest".

## How the server pipeline does it (`.github/workflows/image.yml`)

1. A push to `main` builds the image as before.
2. It also finds the highest `vX.Y.Z` tag reachable from the commit
   (`deploy/release.sh latest`) and computes the next patch. The image
   is built with that version (`server_version` = `0.1.N (web …)`) and
   pushed as `:0.1.N` and `:<sha>`; `:main` still follows main.
3. The `release` job then tags the merge commit `v0.1.N` and creates
   the GitHub pre-release.
4. Guards:
   - Runs on `main` and tag pushes share one concurrency group, so two
     merges in quick succession get 0.1.N and 0.1.N+1, never the same.
   - A commit that already has a version tag (a re-run) keeps it and
     isn't tagged again.
   - A manual rebuild (`workflow_dispatch` without bump = minor)
     releases nothing.
   - Until the first tag exists there is no line, and nothing is
     released.
5. A hand-pushed tag (`v0.1.0`, `v1.0.0-rc.1`) must be `v` + SemVer
   2.0.0 without build metadata, on `main`, with a `CHANGELOG.md`
   section; it's built and released the same way.
6. The deploy script keeps taking the short sha:
   `loomux-deploy-test-instance <sha> "v0.1.N"`.

loomux/web does the same in its own CI (its version line, its tags).

## Release notes and `CHANGELOG.md`

- A patch release's notes are generated: the merged pull request's
  title and number, plus (for the server) the web version it pins.
- `CHANGELOG.md` (Keep a Changelog) is curated. Its `[Unreleased]`
  section collects what's coming, and a milestone (each MINOR, the base
  0.1.0, rc and 1.0.0) gets a written section, which then becomes that
  release's notes.
- Not every patch is committed back to `CHANGELOG.md` by a bot. That
  would be a bot commit to `main` per merge, triggering CI again, and
  loomux/web's protected `main` (pull requests only) rejects it. The
  GitHub releases list is the per-merge changelog.

## Cutting the base, v0.1.0

1. loomux/web: give `CHANGELOG.md` a `[0.1.0]` section, merge, tag the
   merge commit `v0.1.0` and push the tag.
2. loomux/server: the same, plus pinning that web commit
   (`deploy/web-ref` + `deploy/web-sha256`, digest hashed from the
   downloaded tarball). Merge, tag the merge commit `v0.1.0` and push.
3. Deploy. From then on, every merge releases itself.
