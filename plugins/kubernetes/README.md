# The Kubernetes plugin (LOOM-179)

A target-provider plugin (`docs/design/target-providers.md` §8): one
pod per machine in one namespace, a Secret carrying its sshd files, a
PersistentVolumeClaim for its data, reached by loomuxd over SSH through
a headless Service. Its own Go module, so `client-go` never enters the
server's `go.mod`; it replaces `github.com/Loomux/server` with `../..`.

```sh
cd plugins/kubernetes
go test ./...                       # unit tests and the conformance suite, against a fake cluster
go build ./cmd/loomux-plugin-kubernetes
```

## Forms

- **Bundled** in the server image under `/usr/local/lib/loomux/plugins/kubernetes/`
  (`Dockerfile` stage 1b): installed from the UI as source `bundled`,
  run as a subprocess, configured with a `kubeconfig` (a secret).
- **Sidecar**: `ghcr.io/loomux/plugin-kubernetes` (`plugins/kubernetes/Dockerfile`,
  distroless), started with `--listen /run/loomux/plugins/kubernetes.sock`
  beside loomuxd; the mounted ServiceAccount is its credential and
  `kubeconfig` stays empty. What the cluster needs is design §10;
  `deploy/test/kind/` is the test copy of it.

## Configuration

`plugin.json`'s schema: `namespace`, `storage_class` (default
`ceph-rbd-sc-delete`, a Delete-reclaim class; a Retain class leaves
volumes behind after destroy and the quota allows none of it), `subdomain`,
`agent_image` (pin it by digest in production), `kubeconfig` (secret,
empty in-cluster), `sizes` (`{name: {cpu, memory, disk}}`; small/medium/
large by default), `max_environments`, `require_network_policy`,
`timezone`.

The Role it runs under grants exactly design §8's verbs (pods c/g/l/w/d,
pods/log get, PVCs c/g/l/d, secrets c/g/l/u/d, events list, services
get), rule for rule what theWyseKube's manifests grant; a unit test
checks every verb the plugin uses is among them. An ephemeral machine's
`ephemeral-storage` limit covers its data emptyDir, `/tmp` and headroom,
since the kubelet counts disk-backed emptyDirs against it.

## What it makes per machine

| Object | Name | |
|---|---|---|
| Secret | `lx-<id>-ssh` | `host_ed25519`, `authorized_keys`; the spec (without key material) as the `loomux.io/spec` annotation, so start and recreate need only the id. While it exists the machine exists: stopped when its pod is gone |
| PersistentVolumeClaim | `lx-<id>-data` | persistent machines only; RWO, the configured class, the size's disk |
| Pod | `lx-<id>` | the spec of design §8, Pod Security `restricted`; `hostname`/`subdomain` give it `lx-<id>.<subdomain>.<namespace>.svc.cluster.local` |

All labelled `app.kubernetes.io/managed-by=loomux-plugin-kubernetes`,
`loomux.io/instance`, `loomux.io/target`, `loomux.io/environment`,
`loomux.io/role=agent`, `loomux.io/egress`. Another server's machines
(another instance label) are invisible: never listed, never touched.

## The network-isolation check

`plugin.check` runs a canary pod from the agent image, labelled as a
confined (`egress: none`) agent, which tries the API server and the
internet, then kube-dns as the control (the one thing a confined pod
may reach). Exit 0: enforced, and `egress: none` is offered. 10 or 11:
not enforced; `network_policy_not_enforced` is reported (an error with
`require_network_policy`, which then refuses machines) and only
`egress: internet` is offered. 12: not even kube-dns answered, so pod
networking is broken or the DNS policy is missing;
`network_policy_check_failed`, and no verdict is claimed. A canary
still running after 15 s (the image pulling) answers
`network_policy_check_pending`; the next check has the verdict.

## Tests

`plugin_test.go`: the manifest and schema, the pod spec field by field,
the lifecycle against client-go's fake clientset, status mapping, the
canary's exit codes, and the host's conformance suite served in-process.
`kind_test.go` (`-tags kind`, CI's `plugin-kubernetes` job): the
conformance suite and an end-to-end run against a kind cluster with
`deploy/test/kind/` applied, as the plugin's ServiceAccount: create,
wait for running, ssh in through a port-forward with the pinned host
key, stop, start, destroy.
