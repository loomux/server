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
- **MINOR (0.2.0, 0.3.0…) is manual,** one per milestone (user
  decision 2026-10-06; e.g. a later UI overhaul is 0.3.0). Cut it as in
  "Cutting a MINOR" below: a pull request folds `changes/` into the
  version's `CHANGELOG.md` section, and its merge commit is tagged by
  hand.
- **0.x until API v1 is declared stable; that release is 1.0.0** (see
  `docs/release/v0.2.0.md`, "What 1.0.0 will mean"). It needs the user's
  go.
- Every 0.y.z and `-pre` version is a GitHub **pre-release**, never
  marked "latest".

## How the server pipeline does it (`.github/workflows/image.yml`)

1. **`version` job** (main pushes and hand tags only):
   - **A merge:** it **reserves** the next version, so every merge gets its own:
     - `deploy/release.sh reserve` takes the highest `vX.Y.Z` tag in the repository plus one;
     - it creates that tag on the merge commit through the GitHub API, which refuses a tag that already exists;
     - when two merges race, the loser fetches the tags and takes the next number.
     Nothing is queued or cancelled: no concurrency group, so every merge's run builds. A re-run of a commit that already has a version keeps it. If a newer merge's run has already released, an older merge it contains releases nothing of its own (its changes ship in the newer version), so versions follow main's order.
   - **A hand-pushed tag** (`v0.1.0`, a MINOR milestone, 1.0.0) must be `v` + SemVer 2.0.0 with no build metadata, on `main`, with a `CHANGELOG.md` section.
   - **A plain manual rebuild, or a PR,** has no version.
2. **`image`** builds with that version (`server_version` = `0.1.N (web 0.1.M)`) and pushes `:<sha>`, `:main` for main, and `:0.1.N`. The tag already exists by then.
3. **A failed build burns its number.** v* tags are immutable (a
   ruleset on both repositories: no update or delete), so a reserved tag
   is never given back. A re-run of that commit finds its tag and builds
   the same version; otherwise the next merge takes the next number, and
   the line has a gap. SemVer allows gaps, and no version is ever reused.
4. **`release`** creates the GitHub pre-release for the tag (never "latest"). The notes are the merged PR's title, or the version's CHANGELOG section for a milestone, plus the web version pinned.
5. **MINOR:** a hand-pushed tag, as below. (The workflow's
   **bump = minor** input doesn't work on a merge commit: that commit
   already has its patch tag, and a re-run keeps a commit's version.)
6. **Deploy** as before: `loomux-deploy-test-instance <sha> "v0.1.N"`.

loomux/web does the same in its publish job: reserve, bundle `web-<sha>`, release.

## Release notes and `CHANGELOG.md`

- A patch release's notes are generated: the merged pull request's
  title and number, plus (for the server) the web version it pins.
- `CHANGELOG.md` (Keep a Changelog) is curated. A pull request with a
  user-visible change doesn't edit it: it adds a fragment,
  `changes/<slug>.md`, holding its entry under Keep a Changelog headings
  (format in `changes/README.md`; CI checks it with
  `deploy/changelog.sh check`). Every PR editing the same `[Unreleased]`
  lines made each merge conflict with the next under the up-to-date
  branch rule.
- A milestone (each MINOR, later 1.0.0) folds the fragments in:
  `deploy/changelog.sh release 0.2.0 2026-10-20` writes them as a
  `## [0.2.0] - 2026-10-20` section, updates the compare links and
  deletes them. Edit the section into prose, then merge that PR and tag
  its merge commit; the section becomes the release's notes.
  `deploy/changelog.sh assemble` previews it.
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

## Cutting a MINOR (as for 0.2.0)

1. loomux/web: a pull request runs `scripts/changelog.sh release 0.Y.0
   <date>`, edits the section into prose, and merges. Tag its merge
   commit `v0.Y.0` and push the tag: CI's `version-release` job makes the
   GitHub release from the section. The merge also got its own patch
   number first; the next merge continues from 0.Y.0 (0.Y.1).
2. loomux/server: the same with `deploy/changelog.sh`, after pinning the
   web release (`deploy/web-ref` + `deploy/web-sha256`). The tag push
   builds the image as `0.Y.0` and releases it with the section as notes.
3. Deploy that commit's image (`loomux-deploy-test-instance <sha>`); it
   reports `0.Y.0 (web 0.Y.0)`.

## The API v1 contract (`api/testdata/api-v1-contract.txt`)

From 1.0.0, `/api/v1` is additions only (`docs/release/v0.2.0.md`, "What
1.0.0 will mean"). `TestAPIv1Contract` (`api/contract_test.go`) enforces
it:

- It reads the routes from the `mux.HandleFunc("METHOD /api/v1/...")`
  calls in `api/`, and each route's bodies (request, response, stream
  events, the shared error) from `apiV1Bodies` in the test, reflecting
  on their `json` tags: one line per field, with its JSON kind,
  `omitempty` and `nullable`. A route missing from `apiV1Bodies`, a
  `...Request`/`...Response`/`...Event` type no route uses, a map
  literal written as a response or a stream event that isn't
  registered fails the test.
- It reads each route's status codes, query parameters and headers from
  the source (`api/contract_http_test.go`): starting at the functions
  its `mux.HandleFunc` registers (the handler, and `s.requireAuth`
  around it, hence the 401s and `Authorization`), it walks every
  function in `api/` they reach and records one line per fact:
  `status <code>` for each `http.StatusX` written through
  `w.WriteHeader`, `http.Error` or a helper that passes its status on
  (`writeJSON`, `writeError`); `query <name>` for each
  `r.URL.Query().Get/Has` or `r.FormValue`; `request-header <Name>` for
  each `r.Header.Get/Values`; `response-header <Name>` for each
  `w.Header().Set/Add` (connection-level headers such as `Connection`
  aside). What the walk can't follow fails the test instead of being
  skipped: a status that isn't a constant, a query or header named by a
  variable, the request or response handed to a function outside
  `api/` that isn't in the test's short list of ones known not to
  touch them, or either one aliased. The walk follows `w` and `r` by
  name, so they (and `r.URL`, `r.Header`, `w.Header()`) may only appear
  as the receiver of the methods and fields it reads (`w.Header().Set`,
  `w.WriteHeader`, `r.Context()`, `r.URL.Query().Get`, ...), as a call
  argument (to a function in `api/` taking them as a parameter of the
  same type, or one on that short list), or as `w.(http.Flusher)`;
  copying one (`rw := w`, a struct literal, `&w`, a return, a channel
  send, `u := r.URL`) fails with "request/response aliased". It is
  name-based, not scoped: an unrelated local named like a handler's
  `w` or `r` anywhere in the same function is held to the same rules.
- It compares that with the golden file. **A line gone or changed**
  (a field removed, renamed, retyped or made `omitempty`; a route
  removed; a status code, query parameter or header no longer
  answered with or read) fails: it breaks v1 and waits for `/api/v2`.
  **A new line** fails too, until it is recorded with
  `go test ./api -run TestAPIv1Contract -update` and the golden file is
  committed, so every API change shows up in review.
- Until 1.0.0 a deliberate break is still allowed: record it with
  `-update` and say so under `### Changed` or `### Removed` in its
  `changes/` fragment.

It covers which status codes, query parameters and headers a route
has, not what they mean: the condition a status is answered for, a
parameter's accepted values and a header's value are still reviewed by
hand, as are the mux's own 404 and 405 for an unknown path or method,
and the implicit 200 of a route that writes a body without naming a
status (every route today names its 2xx).
