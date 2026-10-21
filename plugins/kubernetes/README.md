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
get, networkpolicies list); a unit test checks every verb the plugin
uses is among them. theWyseKube's manifests grant all but
networkpolicies list, which the deny-all check needs (LOOM-201): until
its Role has it, the check reports `network_policy_check_failed` and
`egress: none` isn't offered. An ephemeral machine's
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

`egress: none` rests on two things, both required.

**The policies.** On every check, describe and create the plugin lists
the namespace's NetworkPolicies. Every one that may select a confined
machine (by `loomux.io/role`, `egress`, `instance`, or anything that
could match one machine's target or environment label) must allow it
§10's (c) and nothing else: pods labelled `k8s-app=kube-dns` in
`kube-system`, UDP or TCP 53. And one that selects every confined
machine must isolate it for egress (§10's default deny). NetworkPolicies
are additive, so one more policy that lets agents reach the metadata
address or a LAN service breaks the guarantee for every confined
machine. Then `network_policy_allows_egress` is reported, naming the
policy, and `egress: none` is withdrawn.

**The canary.** A canary pod from the agent image, labelled as a
confined machine, tries all of these at once: the API server (by the
client's address and by its cluster name), kube-dns on its metrics port
9153, the link-local metadata address `169.254.169.254:80`,
`1.1.1.1:443` and `github.com:443`. kube-dns on 9153 is in-cluster:
no firewall outside the cluster blocks it while letting port 53
through, so only an engine enforcing (c) keeps it out. Then the canary
tries kube-dns on port 53 as the control. A target that answers is
tried again, 15 times a second apart, before it counts: an engine
confines a new pod only once it has learned its address (about 2 s
with kube-network-policies). Exit 0: enforced. 10, 14, 13 or 11 (the
API server, kube-dns beyond 53, the metadata address, the internet):
not enforced, and `network_policy_not_enforced` is reported (an error
with `require_network_policy`, which then refuses machines). 12: not
even kube-dns answered, so pod networking is broken
or the DNS policy is missing; `network_policy_check_failed` is
reported and no verdict is claimed. A canary still running after 15 s
(the image pulling) answers `network_policy_check_pending`; the next
check has the verdict.

Check runs the canary again after a minute. Describe never waits for
it: it offers `none` on an "enforced" at most 10 minutes old, and past
5 minutes it starts the canary in the background, so a cluster in use
renews the verdict before it lapses. A pod labelled `none` (create,
start, recreate) is made only while the policies hold and such a
verdict exists. With none, the canary is run and waited for (15 s).
The policies are read before anything is made and once more, last,
just before the pod is.
Otherwise create refuses it: `invalid_params`, or `unavailable` while
the canary runs. A machine already running isn't watched: the label
confines nothing by itself.

What it trusts. The canary samples a few destinations; it can't prove
that none other answers. What proves that is the policies, so the
engine must implement NetworkPolicy as specified, on every node: the
canary runs on whichever node it is scheduled to. A policy engine
DaemonSet missing from or broken on one node leaves machines there
unconfined, and the canary only notices if it lands there. Policies are
live, and no list is atomic with making a pod: one an admin adds after
that last read, even in the same moment, applies to the machine at
once, and only the next check reports it. Holding policy edits back
while a pod is made would take admission control in the cluster, which
the plugin can't set up. Policy APIs the plugin's namespaced Role can't
read can allow what NetworkPolicy
denies: AdminNetworkPolicy, Calico `GlobalNetworkPolicy`, Cilium's
policies. None of those may let agent pods out. The canary is the only
check on them, and only for its own destinations. Whether a pod's
traffic to its own node is policed is up to the engine, so the node's
own listening services are outside the guarantee. Name lookups through
kube-dns remain allowed (§10's (c)), and the resolvers forward them.

## Tests

`plugin_test.go`: the manifest and schema, the pod spec field by field,
the lifecycle against client-go's fake clientset, status mapping, the
canary's exit codes, and the host's conformance suite served in-process.
`policy_test.go`: the deny-all check, against the kind manifests'
policies and against policies that let a confined machine out (a CIDR,
a LAN service, kube-dns on another port, one machine by its label);
the verdict's expiry for describe and create; the policies read again
at create, start and recreate.
`kind_test.go` (`-tags kind`, CI's `plugin-kubernetes` job): the
conformance suite and an end-to-end run against a kind cluster with
`deploy/test/kind/` applied, as the plugin's ServiceAccount: create,
wait for running, ssh in through a port-forward with the pinned host
key, stop, start, destroy.
