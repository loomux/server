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

1. **`version` job** (main pushes and hand tags only):
   - **A merge:** it **reserves** the next version, so every merge gets its own:
     - `deploy/release.sh reserve` takes the highest `vX.Y.Z` tag in the repository plus one;
     - it creates that tag on the merge commit through the GitHub API, which refuses a tag that already exists;
     - when two merges race, the loser fetches the tags and takes the next number.
     Nothing is queued or cancelled: no concurrency group, so every merge's run builds. A re-run of a commit that already has a version keeps it. If a newer merge's run has already released, an older merge it contains releases nothing of its own (its changes ship in the newer version), so versions follow main's order.
   - **A hand-pushed tag** (`v0.1.0`, a milestone, an `-rc`) must be `v` + SemVer 2.0.0 with no build metadata, on `main`, with a `CHANGELOG.md` section.
   - **A plain manual rebuild, or a PR,** has no version.
2. **`image`** builds with that version (`server_version` = `0.1.N (web 0.1.M)`) and pushes `:<sha>`, `:main` for main, and `:0.1.N`. The tag already exists by then.
3. **A failed build burns its number.** v* tags are immutable (a
   ruleset on both repositories: no update or delete), so a reserved tag
   is never given back. A re-run of that commit finds its tag and builds
   the same version; otherwise the next merge takes the next number, and
   the line has a gap. SemVer allows gaps, and no version is ever reused.
4. **`release`** creates the GitHub pre-release for the tag (never "latest"). The notes are the merged PR's title, or the version's CHANGELOG section for a milestone, plus the web version pinned.
5. **MINOR:** run the workflow on `main` with **bump = minor**.
6. **Deploy** as before: `loomux-deploy-test-instance <sha> "v0.1.N"`.

loomux/web does the same in its publish job: reserve, bundle `web-<sha>`, release.

## Release notes and `CHANGELOG.md`

- A patch release's notes are generated: the merged pull request's
  title and number, plus (for the server) the web version it pins.
- `CHANGELOG.md` (Keep a Changelog) is curated. Its `[Unreleased]`
  section collects what's coming, and a milestone (each MINOR, the base
  0.1.0, rc and 1.0.0) gets a written section, which then becomes that
  release's notes.
- Nothing commits back to `main`: loomux/web's `main` is protected
  (pull requests only, admins included), and loomux/server is meant to
  get the same protection once v0.1.0 exists. Releases only create tags,
  which branch protection doesn't cover. The GitHub releases list is the
  per-merge changelog.

## Cutting the base, v0.1.0

1. loomux/web: give `CHANGELOG.md` a `[0.1.0]` section, merge, tag the
   merge commit `v0.1.0` and push the tag.
2. loomux/server: the same, plus pinning that web commit
   (`deploy/web-ref` + `deploy/web-sha256`, digest hashed from the
   downloaded tarball). Merge, tag the merge commit `v0.1.0` and push.
3. Deploy. From then on, every merge releases itself.
