# Kubernetes plugin (LOOM-179, LOOM-178 PR 4) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** the first real target provider: `plugins/kubernetes`, a pod per machine in one namespace of the
cluster Loomux runs in (design §8), passing the conformance suite and an end-to-end run on kind in CI, bundled
in the server image and published as its own sidecar image.

**Architecture:** its own Go module (client-go stays out of the server's `go.mod`; `replace ../..`), `sdk`'s
`Plugin` + `TargetProvider`; the Secret is the machine's record (spec as an annotation, key material as data);
the pod spec of §8 verbatim; the enforcement canary run in the background with a bounded wait so `check` stays
under the host's 30 s call deadline.

**Tech Stack:** Go 1.27, k8s.io/client-go v0.37.1 (+ its fake clientset), kind in CI, distroless static.

**Spec:** `docs/design/target-providers.md` §8, §2.3, §10, §12 "Kubernetes plugin".

## Global Constraints

- Pod Security `restricted` (asserted field by field and enforced by the kind namespace); no `LOOMUX_*` env,
  no ServiceAccount token, no host namespaces; images pinned by digest; labels from user text never reach
  object names (annotations only, cleaned).
- Every targets.* method idempotent on its id; another instance's objects never listed or touched.
- Errors never repeat the kubeconfig; the host's instance id (from configure) is the authority.

## Review Focus

1. A stop then an immediate start (pod still terminating): `ensurePod` waits for it to be gone, bounded by
   `DeleteWait` under the call deadline — unit: fake deletes at once; kind: real termination.
2. The first `check` on a cold cluster (image pull > 15 s): answers pending, the canary finishes in the
   background, the next check has the verdict — `TestCheck/pending`.
3. A spec labelled for another instance: refused (`TestCreateRefusesBadSpecs/instance`); their Secret never
   listed, got or destroyed (`TestOtherInstanceIsInvisible`).
4. The Role's verbs suffice: CI runs the plugin as the ServiceAccount (`kubeconfig-for-sa.sh`).
5. The record's spec never carries key material (`TestSecretAndClaim`).

---

### Task 1: Module, config, client — done
- [x] `go.mod` (client-go v0.37.1), `plugin.json`, `config.go`, `client.go`, `plugin.go`.

### Task 2: Objects, status, targets.*, the canary — done
- [x] `objects.go`, `status.go`, `targets.go`, `netcheck.go`, `cmd/loomux-plugin-kubernetes`.
- [x] `plugin_test.go`: manifest, config, pod spec, lifecycle (fake clientset), status, check, conformance.

### Task 3: kind, images, workflow, docs
- [x] `deploy/test/kind/` (§10 test copy + `kubeconfig-for-sa.sh`), `kind_test.go` (`-tags kind`).
- [x] `plugins/kubernetes/Dockerfile` + `smoke.sh`; server `Dockerfile` stage 1b bundles the plugin.
- [x] `plugins.yml`: `plugin-kubernetes` job (unit, kind conformance + e2e as the SA, image, scan) and a
  publish matrix; README, `docs/deploy/plugins.md`, design §8/§11 alignment, changelog.
- [ ] Local run of the kind job's steps green; PR; review; merge after PR 2 and PR 3.
