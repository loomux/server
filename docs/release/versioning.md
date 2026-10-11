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
2. **`agent`**, **`image`** and **`plugins`** build with that version: the agent image is reused or rebuilt and published first (`agent.yml`), then the server image (`image` job, built with the agent pin) and the plugin images (`plugins.yml`) are built and tested. **`publish`** then pushes every image at once, each as `:0.1.N`, `:sha-<short>` and `:main` (the server also its legacy bare `:<short>`), once every build and test job passed. The tag already exists by then. See "Images" below.
3. **A failed build burns its number.** v* tags are immutable (a
   ruleset on both repositories: no update or delete), so a reserved tag
   is never given back. A re-run of that commit finds its tag and builds
   the same version; otherwise the next merge takes the next number, and
   the line has a gap. SemVer allows gaps, and no version is ever reused.
4. **`release`** creates the GitHub pre-release for the tag (never "latest"). The notes are the merged PR's title, or the version's CHANGELOG section for a milestone, plus the web version pinned and each image's `tag@digest`.
5. **MINOR:** a hand-pushed tag, as below. (The workflow's
   **bump = minor** input doesn't work on a merge commit: that commit
   already has its patch tag, and a re-run keeps a commit's version.)
6. **Deploy**: pin the release's images by digest ("Images" below).

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

## Images (LOOM-194, user decisions 2026-10-10)

One release run names every image. The server's `vX.Y.Z` is the version
of `ghcr.io/loomux/server`, `ghcr.io/loomux/plugin-kubernetes` and
`ghcr.io/loomux/plugin-docker` built from that commit, and of the
`ghcr.io/loomux/agent` built from it when `deploy/agent` changed.

### Tags

| Tag | Moves? | server | plugin-kubernetes, plugin-docker | agent |
|---|---|---|---|---|
| `X.Y.Z` | never | every release | every release | releases where `deploy/agent/**` changed |
| `sha-<short>` | never | every published build | every published build | every agent build |
| `<short>` (bare) | never | **legacy**, kept until theWyseKube and `loomux-deploy-test-instance` pin `X.Y.Z@digest`; then dropped | — | — |
| `main` | yes, follows `main` | yes | yes | yes |
| `X.Y` | — | not published (decided against for now) | — | — |
| `latest` | — | not before 1.0.0: every 0.x is a pre-release | — | — |

"Never moves" is a rule of the pipeline, not of the registry (GHCR has
no immutable tags): `deploy/publish-image.sh` accepts an existing
`X.Y.Z` or `sha-<short>` only when its `org.opencontainers.image.revision`
label is the commit being published **and** its `.version` label is the
version this run publishes (a re-run), and then reuses its digest rather
than pushing a second image for the same commit. A commit tag of the
same commit under another version (a hand-pushed MINOR on a commit the
merge already released as a patch) is left alone, and the image built
as the MINOR is pushed under its version tag; a plain rebuild of a
released commit pushes nothing. An existing tag from another commit
fails the run. Nothing in the pipeline deletes a tag or a package
version. A consumer pins **`X.Y.Z@sha256:…`**: the tag says what it is,
the digest is what runs.

### Labels and attestation

Every image carries `org.opencontainers.image.version` (the release, or
`sha-<short>` when the build has none), `.revision` (the commit),
`.source` (`https://github.com/loomux/server`), `.created`, and its own
`.title` and `.description`. The agent image adds
`io.loomux.agent.contract` and its CLI versions
(`docs/deploy/agent-image.md`). Every published digest has a GitHub
build-provenance attestation, pushed to the registry beside the image:

```sh
# by the digest a release's notes give (what a deployment pins):
gh attestation verify oci://ghcr.io/loomux/server@sha256:<digest> --owner loomux
# or by tag, for any release published after the tags became copies
# of the attested manifest (see below):
gh attestation verify oci://ghcr.io/loomux/server:<X.Y.Z> --owner loomux
```

Every tag of an image names the same digest, the one attested: after
the first push, the other tags are set by copying the manifest's bytes
through the registry API (`deploy/ghcr.sh tag`), never by a tool that
wraps the image in a manifest list. The version tags of 0.3.77 to
0.3.81 were first published that way (a one-entry manifest list with a
digest of its own, so they verified only by the digest in their notes
or by the bare `:<short>`) and were re-pointed to the attested digest
afterwards; the lists stay in the registry as untagged versions.

The signer workflow is `.github/workflows/image.yml` for the server and
plugin images and `.github/workflows/agent.yml` for the agent, at
`refs/heads/main` for a patch release (the run is the merge's) and at
`refs/tags/v0.Y.0` for a hand-pushed MINOR. The provenance names the
commit; for a release it is the commit `vX.Y.Z` tags.

### The release run (`.github/workflows/image.yml`)

1. **`version`** reserves or reads the version (above).
2. **`agent`** (`agent.yml`, reusable): if `deploy/agent` is unchanged
   since the commit the current `:main` agent image was built from
   (its `revision` label), the current image is reused; otherwise it is
   rebuilt, smoke tested, scanned, published and attested. Either way the
   run has one agent reference, `ghcr.io/loomux/agent:<tag>@sha256:…`.
3. **`image`** builds the server with `RELEASE` and that agent reference
   (the bundled plugin manifests are stamped with both), verifies it by
   running it, and uploads it as an artifact. **`plugins`**
   (`plugins.yml`, reusable) runs each plugin's unit tests, the end to
   end on kind and on the runner's docker engine when plugin code
   changed since the previous push to `main` (`plugins/`,
   `deploy/test/kind`, `deploy/agent`, the two workflows; always on a
   pull request), builds each plugin image with the same stamp, smoke
   tests it (the manifest it serves says the release) and scans it, and
   uploads it.
4. **`publish`**, once every job above passed: each of the three images
   is pushed under its tags, attested, and its `tag@digest` recorded.
5. **`release`** creates the GitHub pre-release; its notes list the web
   version pinned and each image's `tag@digest`.

A run without a version (a merge a newer release already covers; a
plain `workflow_dispatch` rebuild) publishes `sha-<short>` (and the
server's bare `<short>`) only and moves nothing. A pull request builds
and verifies the server image, and plugin pull requests run
`plugins.yml` on their own; nothing is pushed.

### What the version means for plugins

The manifests in the tree say `version: 0.0.0-dev`. A build stamps
them (`plugins/manifeststamp`): `version` = the release,
`min_host_version` = its `X.Y.0`, and the `agent_image` setting's
default = the agent reference of the run. The bundled binaries in the
server image and the sidecar images of the same release are built from
the same source and stamped the same, so they can't drift. The host
refuses a plugin below its `min_host_version` or from another major,
warns about one built for an older `0.Y` and runs it, and adopts a
bundled or sidecar plugin's new version on its own when its
capabilities and configuration schema (defaults aside) are unchanged;
a setting still equal to the plugin's previous default follows the new
one then, so an install whose `agent_image` is still the shipped
default moves to the release's agent pin, while a value that differs
from the previous default stays (to freeze an image, pin a value other
than the one the plugin shipped with: the server can't tell a hand-set
copy of the default from the default). `GET /api/v1/plugins/available`
says `incompatible` for a plugin the server refuses and
`incompatible_warning` for one that runs with a warning
(`docs/deploy/plugins.md`, "Upgrades and compatibility").

### Deploying a release

Pin the server and its sidecar at the same `X.Y.Z`, each by digest, in
one change:

```yaml
image: ghcr.io/loomux/server:0.3.80@sha256:…
image: ghcr.io/loomux/plugin-kubernetes:0.3.80@sha256:…
```

The digests are in the release notes and in the run's `publish` job
summaries; verify them with `gh attestation verify` before pinning.
Flux image automation is not used: TEST deploys are deliberate and
reviewed, and production is pinned by hand. After the rollout the
server adopts the new sidecar version at its next start, or on Enable
when the sidecar wasn't answering at that moment, as long as the
plugin's capabilities and schema are unchanged (the usual case); when
they changed, the plugin shows "upgrade the plugin" and Upgrade moves
it.

**The first stamped deploy moves every install still on
`ghcr.io/loomux/agent:main` to the release's pinned agent image**, TEST
included: that value is the old shipped default, so it follows. Machines
on another digest then show `update_available`; recreate them when
convenient. An install meant to stay on a floating or other image must
be set to a value other than the shipped default before that deploy.

## The API v1 contract (`api/testdata/api-v1-contract.txt`)

From 1.0.0, `/api/v1` is additions only (`docs/release/v0.2.0.md`, "What
1.0.0 will mean"), with one exception decided before the freeze (API v1
freeze review, item 15):

- **`/api/v1/web/*` is an operations API, outside the v1 promise.**
  `GET /web/version`, `POST /web/update` and `POST /web/rollback`
  describe and drive the in-place web bundle updater; their bodies
  mirror the release pipeline (`repo`, `tag`, `tarball`, `sha256`,
  `schema`, `source`, `latest_error`) and may change with it in any 1.x
  release. Production runs with web updates off. The contract test still
  records them, so a change shows up in review, but changing them isn't
  a v1 break: record it with `-update` and say so in the changes
  fragment.

`TestAPIv1Contract` (`api/contract_test.go`) enforces the promise:

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

Lists that can grow carry `has_more` (API v1 freeze review, item 17;
`api/README.md`, "Lists and paging"), so paging them later is an
addition, not a break: a v1 client must check `has_more` rather than
assume a list is complete. Paging a list without that field would be a
break.

It covers which status codes, query parameters and headers a route
has, not what they mean: the condition a status is answered for, a
parameter's accepted values and a header's value are still reviewed by
hand, as are the mux's own 404 and 405 for an unknown path or method,
and the implicit 200 of a route that writes a body without naming a
status (every route today names its 2xx).
