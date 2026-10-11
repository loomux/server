# LOOM-194 Image versioning as a release train — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every image this repo publishes (server, agent, plugin-kubernetes, plugin-docker) is named by the server's `vX.Y.Z` from one release run, carries honest OCI labels and a build-provenance attestation, and the bundled and sidecar plugin manifests are stamped with that version so the host can enforce compatibility and adopt bundled updates silently.

**Architecture:** `image.yml`'s `version` job stays the single source of the number. It calls two reusable workflows: `agent.yml` (resolve the current agent image or rebuild it when `deploy/agent/**` changed; publish it; output `tag@digest`) and `plugins.yml` (test and build the plugin images with that agent pin and the version, upload them as artifacts). One final `publish` matrix in `image.yml` pushes server + plugin images under `:X.Y.Z`, `:sha-<short>`, `:main` (server also the legacy bare `:<short>`) with an immutability rule, attests each digest, and `release` lists `tag@digest` per image. Manifests are stamped at build (`version` = X.Y.Z, `min_host_version` = X.Y.0, `agent_image` default = the agent pin); the host enforces `min_host_version` with caret semantics and auto-adopts a bundled version change whose capabilities and config schema are unchanged.

**Tech Stack:** GitHub Actions (reusable workflows, `docker/metadata-action`, `actions/attest-build-provenance`), POSIX sh + curl + jq (registry reads), Go 1.27 (host, stamp tool, script tests), Docker multi-stage builds.

**Spec:** `~/git/command-center/reports/2026-10-10-loomux-image-versioning.md` (state, defects, PR plan) and `docs/design/target-providers.md` §1.2, §1.4, §1.6, §3, §11. User decisions D1–D8 are copied below.

## Global Constraints

- D1 release train: server + every plugin image get `:X.Y.Z` and `:sha-<short>` (never re-pushed) + `:main` (moves). No `:latest` before 1.0.
- D2 the kind e2e runs only when plugin code changes (paths: `plugins/**`, `deploy/test/kind/**`, `deploy/agent/**`, `.github/workflows/plugins.yml`, `.github/workflows/agent.yml`).
- D3 bundled plugins are adopted automatically when their capabilities + config schema are unchanged. Otherwise "upgrade available" (the row goes to `error` with a reason naming the upgrade, since the old binary no longer exists in the image).
- D4 the agent image is rebuilt only when `deploy/agent/**` changes. Plugins pin it as `tag@digest`.
- D5 keep the bare `:<short>` server tag. D6 no `:X.Y` tags. D7 no Flux image automation. D8 never delete or prune tags/packages.
- Root cause first: patch tags are created with `GITHUB_TOKEN`, so a `tags: v*` trigger never fires; publishing runs from the release run via `workflow_call`.
- Actions: only loomux / GitHub / verified-creator actions; new steps pinned by commit with the version in a comment (as `plugins.yml` does). `docker/*`, `actions/*`, `aquasecurity/trivy-action` are allowed.
- PR flow: branch per PR, stacked (PR N+1 branches from PR N); CI green → handoff `mailctl.py send --from loomux-versioning --to command-center --type handoff` → **no merges by this workspace**. No deploys, no theWyseKube edits, no manual tags.
- Order of PRs: 1 labels+attestation, 2 release run, 3 **host compatibility + adopt**, 4 **stamping**, 5 docs. PRs 3 and 4 are swapped relative to the brief because a deployed stamped image without adopt breaks every installed bundled plugin at startup (the handshake compares the row's manifest version with the binary's); the swap is stated in the handoffs.
- Every PR with a user-visible change adds a `changes/<slug>.md` fragment (`deploy/changelog.sh check`).
- Each plugin module is its own Go module (`replace ../..`): run `go test` in `plugins/kubernetes` and `plugins/docker` too when touching them.

## Review Focus

1. A re-run of a failed release for the same commit: `:sha-<short>` already exists from the earlier attempt; the pipeline must adopt that digest for `:X.Y.Z` and `:main` rather than fail or push a second digest. (Task 7's test `rerun adopts the existing sha tag`.)
2. An existing `:X.Y.Z` built from a different commit must never be overwritten and must fail the run loudly. (Task 7's test `immutable tag for another commit fails`.)
3. A host built without a version (`dev`, or `<short> (web …)`) must install and run any plugin; a host at `0.3.80` must refuse a manifest with `min_host_version: 0.4.0` and accept `0.3.0`; a 1.x host must refuse a 0.x plugin. (Task 13/14 tests.)
4. A server image whose bundled plugin changed its config schema since the row was installed must not start the old row's manifest against the new binary; it must mark the row with a reason that names the upgrade, and `Upgrade` must then succeed. (Task 16's test.)
5. A stamped manifest must be the same bytes on disk and in the embedded binary, or the handshake refuses the bundled plugin; the smoke tests assert the served manifest's version when one is expected. (Task 22/23.)

---

# PR 1 — OCI labels and attestation (branch `orski/loom-194-1-labels`)

### Task 1: Dockerfile labels

**Files:**
- Modify: `Dockerfile:157-160`, `deploy/agent/Dockerfile:68-71`, `plugins/kubernetes/Dockerfile:30-33`, `plugins/docker/Dockerfile:31-34`

- [ ] **Step 1:** In all four files change `org.opencontainers.image.source="https://github.com/Loomux/server"` to `https://github.com/loomux/server`.
- [ ] **Step 2:** In `deploy/agent/Dockerfile` extend the LABEL with the agent contract and CLI pins (ARGs are in scope):
```dockerfile
LABEL org.opencontainers.image.title="loomux-agent" \
      org.opencontainers.image.description="Loomux agent machine: tmux, sshd and the agent CLIs, for machines a plugin makes" \
      org.opencontainers.image.source="https://github.com/loomux/server" \
      org.opencontainers.image.licenses="MIT" \
      io.loomux.agent.contract="1" \
      io.loomux.agent.claude-code="${CLAUDE_CODE_VERSION}" \
      io.loomux.agent.codex="${CODEX_VERSION}" \
      io.loomux.agent.opencode="${OPENCODE_VERSION}"
```
- [ ] **Step 3:** Build the agent image locally and check: `docker build -t loomux-agent:labels deploy/agent && docker inspect --format '{{json .Config.Labels}}' loomux-agent:labels | jq .` → shows `io.loomux.agent.contract: "1"` and the three CLI versions.
- [ ] **Step 4:** Commit `LOOM-194: image labels: lowercase source, the agent contract and CLI pins`.

### Task 2: plugins.yml — one metadata step per image, honest title/description

**Files:**
- Modify: `.github/workflows/plugins.yml:62-80, 186-194, 272-280`

- [ ] **Step 1:** Agent `meta` step gains `labels:` overrides:
```yaml
          labels: |
            org.opencontainers.image.title=loomux-agent
            org.opencontainers.image.description=Loomux agent machine: tmux, sshd and the agent CLIs, for machines a plugin makes
```
- [ ] **Step 2:** Before each plugin's "Build the plugin image" add a metadata step (same pinned action, same `tags:` rules as the agent's so the version label agrees) with `images: ghcr.io/loomux/plugin-kubernetes` / `plugin-docker` and `labels:` title `loomux-plugin-kubernetes` / `loomux-plugin-docker` and the Dockerfile's description; pass `labels: ${{ steps.meta.outputs.labels }}` to the build.
- [ ] **Step 3:** Lint: `go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.7 .github/workflows/plugins.yml`.
- [ ] **Step 4:** Commit `LOOM-194: each plugin image carries its own labels`.

### Task 3: image.yml — labels and a build-provenance attestation for the server

**Files:**
- Modify: `.github/workflows/image.yml:87-97, 206-223, 256-285`

- [ ] **Step 1:** `image` job permissions add `id-token: write` and `attestations: write`.
- [ ] **Step 2:** After `Compute tags`, add a `labels` step:
```yaml
      - name: Image labels
        id: labels
        uses: docker/metadata-action@dc802804100637a589fabce1cb79ff13a1411302 # v6.2.0
        with:
          images: ${{ env.IMAGE }}
          labels: |
            org.opencontainers.image.title=loomux-server
            org.opencontainers.image.description=Loomux server (loomuxd): chat-driven multi-agent tmux orchestration
            org.opencontainers.image.version=${{ steps.meta.outputs.release || steps.meta.outputs.sha }}
```
  and pass `labels: ${{ steps.labels.outputs.labels }}` to the build step.
- [ ] **Step 3:** The `Push` step records the pushed digest (`sed -n 's/.*digest: \(sha256:[0-9a-f]*\).*/\1/p' | tail -n 1` on the first push's output) to `$GITHUB_OUTPUT` as `digest`; add after it:
```yaml
      - name: Attest
        if: github.event_name != 'pull_request'
        uses: actions/attest-build-provenance@4d101475d8b20a2381f78447822ac1eab6504dd8 # v4.2.2
        with:
          subject-name: ${{ env.IMAGE }}
          subject-digest: ${{ steps.push.outputs.digest }}
          push-to-registry: true
```
- [ ] **Step 4:** actionlint; commit `LOOM-194: the server image is labelled and attested`.

### Task 4: docs line and changelog fragment

- [ ] **Step 1:** `docs/deploy/agent-image.md`: a sentence under the contents table: the image labels `io.loomux.agent.contract` (1) and the CLI versions.
- [ ] **Step 2:** `changes/loom-194-image-labels.md`:
```markdown
### Added

- Every published image (`ghcr.io/loomux/server`, `agent`, `plugin-kubernetes`, `plugin-docker`) carries OCI labels naming its version, commit, source and what it is (the agent image no longer claims to be the server), and the server image gets a GitHub build-provenance attestation like the plugin images (LOOM-194). The agent image also labels its contract (`io.loomux.agent.contract`) and CLI versions.
```
- [ ] **Step 3:** `sh deploy/changelog.sh check`; commit.

### Task 5: Verify, PR, handoff

- [ ] Push branch; `gh pr create -B main` with the description; wait for CI (`gh pr checks --watch`); handoff to command-center (what changed, evidence: local label inspect output, CI run URL; note PR ordering swap of 3/4 and why).

---

# PR 2 — One release run (branch `orski/loom-194-2-release-run`, base PR 1)

### Task 6: `deploy/ghcr.sh` — read a public image's digest and labels

**Files:** Create `deploy/ghcr.sh`, Create `deploy/scripts_test.go` (package `deploy_test`, test-only package).

**Interfaces:** `deploy/ghcr.sh digest <image> <tag>` prints the manifest digest, exit 2 when the tag is absent; `deploy/ghcr.sh label <image> <tag> <name>` prints one config label (empty when unset); `deploy/ghcr.sh labels <image> <tag>` prints the labels JSON. `<image>` is `ghcr.io/<owner>/<name>`. Anonymous pull token (public packages only). Honors `GHCR_ACCEPT` default list of the four manifest media types; an index selects `linux/amd64`.

- [ ] **Step 1:** Write the script (sh, `set -eu`; `curl -fsS` + `jq`; token from `https://ghcr.io/token?scope=repository:<owner>/<name>:pull`; manifest via `-D` headers to read `docker-content-digest`; an index → pick the amd64 manifest and fetch it; config blob → `.config.Labels`). A 404 on the manifest → exit 2 silently; any other failure → exit 1 with the message.
- [ ] **Step 2:** Test by hand against the live registry (allowed: read-only): `deploy/ghcr.sh digest ghcr.io/loomux/agent main` prints `sha256:…`; `deploy/ghcr.sh label ghcr.io/loomux/agent main org.opencontainers.image.revision` prints a 40-hex sha; `deploy/ghcr.sh digest ghcr.io/loomux/agent 0.3.75; echo $?` → 2.
- [ ] **Step 3:** Commit `LOOM-194: deploy/ghcr.sh reads a public image's digest and labels`.

### Task 7: `deploy/publish-image.sh` — the immutability rule

**Interfaces:** `deploy/publish-image.sh <image> <revision> <local-ref> <tag>...` → prints the final digest. Env `GHCR` (default `deploy/ghcr.sh`, tests substitute a fake) and `docker` from PATH. `main` is the only moving tag; every other tag is immutable.

Rule: for each immutable tag, in order: if it exists → its `org.opencontainers.image.revision` must equal `<revision>` (else fail "exists for commit X"); the first existing one's digest is adopted; a later existing one with a different digest fails. If nothing was adopted, `docker push` `<local-ref>` tagged as the first pending tag → digest. Every other pending tag and every moving tag is pointed at the digest with `docker buildx imagetools create -t <image>:<tag> <image>@<digest>`.

- [ ] **Step 1:** Write `deploy/scripts_test.go` with a fake `docker` (shell script in a temp bin dir: `push` prints `digest: sha256:<sha of args>`; `tag` and `buildx imagetools create` append to `$FAKE_LOG`) and a fake `ghcr` (reads `$FIX/<name>/<tag>.digest|.revision`). Cases: fresh (push sha tag + create X.Y.Z + main; output digest = pushed), rerun (sha exists with same revision → no push; X.Y.Z + main created from the existing digest), conflict (X.Y.Z exists with another revision → exit non-zero, nothing pushed), server legacy bare tag (treated immutable), no moving tag when `main` isn't listed.
- [ ] **Step 2:** Run `go test ./deploy -run TestPublishImage -v` → fails (script missing). Write the script. Run → pass.
- [ ] **Step 3:** Commit `LOOM-194: deploy/publish-image.sh: immutable tags, adopted on a re-run`.

### Task 8: `.github/workflows/agent.yml` (reusable)

`on: workflow_call` inputs `version` (string, default ""), `publish` (boolean, default false); outputs `image` (`ghcr.io/loomux/agent:<tag>@sha256:…`, "" unless published/resolved) and `built` ("true"/"false"). One job `agent`, `permissions: contents: read, packages: write, id-token: write, attestations: write`.

Steps: checkout (fetch-depth 0) → **Decide** (`id: decide`, only when `inputs.publish`): (a) `version` set and `ghcr.sh digest agent <version>` exists → its revision label must equal `GITHUB_SHA` else fail; reuse. (b) else read `:main`'s revision+version+digest; if the revision is a commit known to git and `git diff --quiet <rev> HEAD -- deploy/agent` → reuse `agent:<version-label>@<digest>`. (c) else build. Not publishing → build. Outputs `build=true|false`, `image`. → when building: buildx, metadata (tags `sha-<short>`, `<version>` when set, `main`; labels as PR 1 plus `org.opencontainers.image.version=<version or sha-short>`), build `load: true`, smoke, trivy → when building and publishing: login; `deploy/publish-image.sh ghcr.io/loomux/agent $GITHUB_SHA ghcr.io/loomux/agent:test sha-<short> [<version>] [main if GITHUB_REF == refs/heads/main]` → digest; attest; `image=ghcr.io/loomux/agent:<version or sha-short>@<digest>`; step summary.

- [ ] Write it; actionlint; commit `LOOM-194: agent.yml: the agent image is resolved or rebuilt per release`.

### Task 9: `plugins.yml` becomes reusable (test + build + artifacts only)

- `on:` → `workflow_call` (inputs `version`, `agent_image`, both strings default "") + `pull_request` (paths as today plus `.github/workflows/agent.yml`) + `workflow_dispatch`. **Remove `push:`.**
- New job `changes` (checkout fetch-depth 0): output `e2e=true` unless the event is `push` with a known `github.event.before` and `git diff --quiet $BEFORE HEAD -- plugins deploy/test/kind deploy/agent .github/workflows/plugins.yml .github/workflows/agent.yml` is clean (D2).
- Plugin jobs `needs: changes`; the kind/docker e2e steps `if: needs.changes.outputs.e2e == 'true'` (kind bring-up, load, e2e, "what the cluster holds"). The agent image for e2e: `if inputs.agent_image != ''` → `docker pull "$AGENT_IMAGE" && docker tag "$AGENT_IMAGE" loomux-agent:test`, else build locally as today.
- Remove the `publish` job and the agent-image job (agent.yml owns it; a PR touching `deploy/agent/**` gets a test-only agent build through a job `agent` that `uses: ./.github/workflows/agent.yml` with `publish: false`, `if: github.event_name == 'pull_request'`).
- Artifacts uploaded when `github.event_name != 'pull_request'` (as today).
- [ ] Write; actionlint; commit `LOOM-194: plugins.yml is called by the release run; the e2e runs when plugin code changed`.

### Task 10: `image.yml` orchestrates the release

- Jobs: `version` (as is) → `agent` (`uses: ./.github/workflows/agent.yml`, `needs: version`, `if: github.event_name != 'pull_request' && needs.version.result == 'success'`, `with: version: ${{ needs.version.outputs.version }}, publish: true`, permissions listed) → `image` (`needs: [version, agent]`, `if: !cancelled() && needs.version.result != 'failure' && needs.agent.result != 'failure'`; build + verify as today; **no login/push/attest**; instead `docker save … | zstd` → upload artifact `server-image` when not a PR; outputs `web_version`, `web_short`, `sha`) ∥ `plugins` (`uses: ./.github/workflows/plugins.yml`, `needs: [version, agent]`, same `if` plus `github.event_name != 'pull_request'`, `with: version, agent_image: ${{ needs.agent.outputs.image }}`) → `publish` (`needs: [version, image, plugins]`, `if: !cancelled() && needs.image.result == 'success' && needs.plugins.result == 'success' && github.event_name != 'pull_request'`; matrix `server` (artifact `server-image`, legacy extra tag `<short>`), `plugin-kubernetes`, `plugin-docker`; steps: checkout, download, load, login, tags = `sha-<short>` [+ `<short>` for server] [+ `<version>`] [+ `main` when `GITHUB_REF == refs/heads/main`], `deploy/publish-image.sh`, attest, write `published/<name>.txt` = `<image>:<version or sha-short>@<digest>`, upload artifact `published-<name>`) → `release` (`needs: [version, publish]`; download `published-*`; append an "Images" list to the notes).
- No version (superseded merge or plain rebuild): `publish` still pushes `sha-<short>` (+ legacy) but never `main` or a version; `release` is skipped as today.
- [ ] Write; actionlint; commit `LOOM-194: one release run publishes every image`.

### Task 11: docs minimal + fragment

- [ ] `docs/release/versioning.md` step 2 and `docs/deploy/agent-image.md` publish paragraph: two sentences each pointing at the new flow (PR 5 writes the full section). `changes/loom-194-release-train.md` (`### Changed`: images published per release with `:X.Y.Z`, `:sha-<short>`, `:main`; the agent rebuilt only when its inputs change; `:main` no longer moves on a run without a version).
- [ ] Commit.

### Task 12: Verify, PR, handoff

- [ ] actionlint all three workflows; `go test ./deploy ./version`; push; PR with base `orski/loom-194-1-labels`; CI (a PR can't exercise publish: say so, and that the first main run after merge is the real test; list what to look for in it); handoff.

---

# PR 3 — Host compatibility and bundled adoption (branch `orski/loom-194-3-host-compat`, base PR 2)

### Task 13: `version.Parse` and caret satisfaction

**Files:** Modify `version/version.go`; Create `version/version_test.go`.

**Interfaces:** `type Semver struct{ Major, Minor, Patch int; Pre string }`; `func Parse(s string) (Semver, bool)` takes the first whitespace-separated token of `s` (so `"0.3.74 (web 0.3.12)"` → 0.3.74), accepts `X.Y.Z` and `X.Y.Z-pre`; `func (v Semver) Compare(o Semver) int`; `func (v Semver) Satisfies(min Semver) bool` = same major, and for major 0 same minor, and `v >= min`.

- [ ] Test cases: `"dev"` → false; `"abc1234 (web 0.3.1)"` → false; `"0.3.74 (web 0.3.12)"` → 0.3.74; `"1.0.0-rc.1"` → pre; Satisfies: 0.3.80 ⊨ 0.3.0 true, 0.3.80 ⊨ 0.4.0 false, 0.4.0 ⊨ 0.3.0 false, 1.2.0 ⊨ 1.0.0 true, 2.0.0 ⊨ 1.0.0 false, 0.3.0-rc.1 ⊨ 0.3.0 false.
- [ ] Implement; `go test ./version`; commit.

### Task 14: Manifest host compatibility and the adopt comparison

**Files:** Modify `plugins/manifest.go`, `plugins/manifest_test.go`.

**Interfaces:** `type IncompatibleError struct{ Name, Version, MinHost, Host string }` (Error: `plugins: <name> <version> needs Loomux <X.Y>.x (min_host_version <M>); this server is <host>`); `func (m *Manifest) HostCompatible(host string) error` — nil when `MinHostVersion == ""` or `host` doesn't parse (a dev host); `Validate` refuses a non-semver `min_host_version`; `func (m *Manifest) SameCapabilitiesAndSchema(o *Manifest) bool` — capability sets equal (order-insensitive) and `config_schema` canonically equal (`json.Unmarshal` both to `any`, `reflect.DeepEqual`; empty/`null` equal).

- [ ] Tests first (table), then implement, `go test ./plugins -run 'Manifest'`, commit.

### Task 15: Catalog marks incompatible entries

**Files:** Modify `plugins/catalog.go`, `plugins/catalog_test.go`.

- `Catalog.HostVersion string`; `Available.Incompatible string` (the `IncompatibleError` text) set in `readDir` and `scanSockets`; the entry stays listed. `NewManager` sets `catalog.HostVersion = host.Version` when empty; `app/app.go` sets it explicitly to `version.Version`.
- [ ] Test: a bundle dir manifest with `min_host_version: "9.0.0"` on a `Catalog{HostVersion: "0.3.80"}` → listed with `Incompatible` non-empty; on `HostVersion: "dev"` → empty. Implement; commit.

### Task 16: Manager — refuse incompatible, adopt bundled changes at start

**Files:** Modify `plugins/manager.go`, `plugins/manager_test.go`, `plugins/main_test.go`, `plugins/fake/fake.go`.

- `fake.go`: `var Version string` (ldflags-settable); `Describe` sets `m.Version = Version` when non-empty. `main_test.go` builds a second binary `fakeBin2` with `-ldflags "-X github.com/Loomux/server/plugins/fake.Version=0.2.0"`; helper `fakeDirVersion(t, root, version, bin)` writes the manifest with that version (string-replace `"version": "0.1.0"`) beside `bin`.
- `Install` and `Upgrade`: `if err := a.Manifest.HostCompatible(m.host.Version); err != nil { return nil, err }` (before any row is written / before the candidate starts).
- `startOne`: after `GetPlugin` and the running check: `if p.Source == registry.PluginSourceBundled { if err := m.adoptBundled(ctx, p); err != nil { return err } }`; then `if err := manifest.HostCompatible(m.host.Version); err != nil { return m.fail(ctx, p, err.Error(), nil) }` (via a helper that reads the installed manifest).
- `adoptBundled(ctx, p)`: `a, err := m.catalog.Find(ctx, p.Name, registry.PluginSourceBundled)`; not found → nil (startup proceeds; the start will say what's wrong); same version → nil; installed manifest `SameCapabilitiesAndSchema(&a.Manifest)` and `a.Incompatible == ""` → copy Version/Protocol/Path/Trust/Capabilities/Manifest into `p`, `m.store.UpdatePlugin`, log `bundled plugin adopted` (label, from, to); else → `m.fail(ctx, p, fmt.Sprintf("the bundled %s is now %s, which changed its capabilities or configuration schema; upgrade the plugin", p.Name, a.Manifest.Version), nil)` and return that error (no start attempted).
- [ ] Tests: (1) install refused with `*IncompatibleError` when the bundle's manifest says `min_host_version: 9.0.0` and the manager's host is `0.3.80` (manager constructed with `HostInfo{Version: "0.3.80"}`); (2) adopt: install 0.1.0, stop the manager, replace the bundle with 0.2.0 manifest + `fakeBin2`, new manager `StartAll` → row version 0.2.0, instance running, `AvailableVersion == "0.2.0"`; (3) not adoptable: same but the 0.2.0 manifest also drops `targets.ephemeral` (string-replace) → after `StartAll` the row is `error` with a reason containing `upgrade the plugin` and `0.2.0`; then `Upgrade` succeeds (0.2.0 running). Run `go test ./plugins -race`; commit `LOOM-194: the host enforces min_host_version and adopts a bundled plugin's new version`.

### Task 17: API — `incompatible` on available plugins, 409 on refusal

**Files:** Modify `api/plugins.go`, `api/plugins_test.go`, `api/testdata/api-v1-contract.txt` (`go test ./api -run TestAPIv1Contract -update`).

- `availablePluginResponse.Incompatible string \`json:"incompatible,omitempty"\`` from `a.Incompatible`; `pluginError` mapping `*plugins.IncompatibleError` → `409` with its message.
- [ ] Tests (stub available entry with `Incompatible`, the error table gains the case); update golden; commit.

### Task 18: docs + fragment

- [ ] `docs/deploy/plugins.md` "Upgrades": the compatibility rule (caret on `min_host_version`; 0.x: same `0.Y`), adopt-on-start for bundled plugins, the error reason otherwise. `changes/loom-194-host-compat.md` (`### Added` + `### Changed`). Commit.

### Task 19: Verify, PR, handoff

- [ ] `gofmt -l .`, `go vet ./...`, `go test -race ./...` (root), the plugin modules' tests unaffected but run them; push; PR base PR 2; CI; handoff.

---

# PR 4 — Manifest stamping (branch `orski/loom-194-4-stamping`, base PR 3)

### Task 20: `plugins/manifeststamp` (package main) + tests

**Interfaces:** `go run ./plugins/manifeststamp -version X.Y.Z -agent-image <ref> <plugin.json>...`; `func stamp(data []byte, version, agentImage string) ([]byte, error)`: decodes to `map[string]any` (UseNumber), requires `version` present; `version != ""` must match `^\d+\.\d+\.\d+$` → sets `version` and `min_host_version = X.Y.0`; `agentImage != ""` → sets `config_schema.properties.agent_image.default` when that property exists; writes `json.MarshalIndent(…, "", "  ")` + newline. Empty version and empty agent image → the file is rewritten unchanged in content.

- [ ] Tests: stamps both fields; derives min_host; sets the agent default; leaves a manifest without `agent_image` alone; refuses `0.3` and `v0.3.1`; refuses a file without `version`. Implement; commit.

### Task 21: dev manifests and the plugins' `Version`

- `plugins/kubernetes/plugin.json`, `plugins/docker/plugin.json`: `"version": "0.0.0-dev"`.
- `plugins/kubernetes/plugin.go`, `plugins/docker/plugin.go`: `const Version = "0.1.0"` → `var Version = manifestVersion()` (decode `{"version"}` from `ManifestJSON`; `"unknown"` on error). `TestManifest` keeps `m.Version == Version`.
- Root tests that mention `0.1.0` refer to the fake (unchanged).
- [ ] `cd plugins/kubernetes && go test ./...`; same for docker; commit.

### Task 22: Dockerfiles stamp before `go build`

- `Dockerfile` stage 1b: `ARG RELEASE=` and `ARG AGENT_IMAGE=`; after `COPY . .`: `RUN go run ./plugins/manifeststamp -version "${RELEASE}" -agent-image "${AGENT_IMAGE}" plugins/kubernetes/plugin.json plugins/docker/plugin.json` then the builds (the `cp plugin.json` lines copy the stamped files).
- `plugins/kubernetes/Dockerfile`, `plugins/docker/Dockerfile`: the same two ARGs and stamp line before the build; `COPY plugins/<name>/plugin.json /plugin.json` → `COPY --from=build /src/plugins/<name>/plugin.json /plugin.json`.
- [ ] Local check: `docker build -f plugins/docker/Dockerfile --build-arg RELEASE=0.9.9 --build-arg AGENT_IMAGE=ghcr.io/loomux/agent:0.9.9@sha256:0000 -t lpd:stamp .` then `bash plugins/docker/smoke.sh lpd:stamp 0.9.9` (Task 23's smoke argument) and `docker run --rm --entrypoint /bin/sh lpd:stamp` isn't possible (distroless) — instead `docker create` + `docker cp <id>:/plugin.json -` and inspect. Commit.

### Task 23: workflows pass the args; smoke tests assert the version

- `image.yml` server build-args add `RELEASE=${{ needs.version.outputs.version }}` and `AGENT_IMAGE=${{ needs.agent.outputs.image }}`; the verify step also checks `jq -r .version /usr/local/lib/loomux/plugins/kubernetes/plugin.json` equals the release (when one; the alpine runtime has no jq: use `grep -q '"version": "X"'`).
- `plugins.yml` plugin builds add the same args from `inputs.version` / `inputs.agent_image`; smoke scripts take an optional second argument `<expected version>` and grep `"version":"<expected>"` in the describe answer; the workflow passes `${{ inputs.version }}`.
- [ ] actionlint; commit.

### Task 24: fragment `changes/loom-194-manifest-stamping.md` (`### Changed`: bundled and sidecar plugin manifests carry the release version and `min_host_version`; the default `agent_image` is the pinned `tag@digest` the release built; `plugin.json` in the tree says `0.0.0-dev`).

### Task 25: Verify, PR (base PR 3), handoff.

---

# PR 5 — Docs (branch `orski/loom-194-5-docs`, base PR 4)

### Task 26: `docs/release/versioning.md` — "Images" section: the tag table (from the report §3.1 with D5–D7 applied), the release run (version → agent → image ∥ plugins → publish → release), immutability and re-runs, runs without a version, attestation identities (`image.yml@refs/heads/main` for server/plugins, `agent.yml@…` for the agent; `refs/tags/v0.Y.0` for a hand-pushed MINOR) and the `gh attestation verify oci://… --owner loomux` command plus "the provenance commit is the `vX.Y.Z` tag's commit", how a deployment pins (`X.Y.Z@sha256:…`, server and sidecar the same X.Y.Z), D7/D8 stated.
### Task 27: `docs/design/target-providers.md` — §1.4 manifest example notes stamping (`0.0.0-dev` in the tree); §1.6 attestation identity rows corrected; §3 the pin sentence; §11 PR 3 row gains the release-run note. Send a `handoff` saying the design copy changed (CLAUDE.md rule).
### Task 28: `docs/deploy/agent-image.md` (sparse rebuilds, tags, labels, how the plugin default pin is produced), `docs/deploy/plugins.md` (compatibility rules, adopt), `docs/deploy/container.md` (tags; the legacy bare `:<short>`).
### Task 29: PR (base PR 4), CI, handoff with the whole stack listed in merge order.
