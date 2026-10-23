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

## A cold image pull (LOOM-203)

The first machine on a node pulls the agent image (542 MB), which on a
slow link takes longer than the host's 10-minute create deadline. While
it pulls, the machine is `creating`, its reason "pulling the image on
node … (for …)", and the plugin tells the host it is progress
(`progress: image_pull`): the host's deadline stands still meanwhile, up
to 30 minutes more. The plugin calls it pulling when the pod is
scheduled, its container `ContainerCreating`, and the kubelet's latest
image Event is `Pulling`, with no `Pulled`, `Failed` or `BackOff` since.
A pull that fails is `error` with its reason (`ErrImagePull: …`,
`ImagePullBackOff: …`); the kubelet retries it, and recreate or start
picks the machine up again. The canary keeps its 3 minutes and the
quota above: one that runs out mid-pull proves nothing and says it was
pulling; the image, once pulled, serves the next.

To take a first pull out of the way, pre-pull the pinned image on every
node. That is deployment work (it needs a DaemonSet, which the plugin's
Role can't make), for example, with `agent_image` pinned to the same
reference:

```yaml
apiVersion: apps/v1
kind: DaemonSet
metadata: {name: loomux-agent-prepull, namespace: loomux-agents}
spec:
  selector: {matchLabels: {app: loomux-agent-prepull}}
  template:
    metadata: {labels: {app: loomux-agent-prepull}}
    spec:
      automountServiceAccountToken: false
      securityContext: {runAsNonRoot: true, runAsUser: 10002, seccompProfile: {type: RuntimeDefault}}
      initContainers:
        - name: pull
          image: ghcr.io/loomux/agent:<the pinned version>
          command: ["true"]
          securityContext: {allowPrivilegeEscalation: false, readOnlyRootFilesystem: true, capabilities: {drop: [ALL]}}
          resources: {requests: {cpu: 10m, memory: 16Mi}, limits: {cpu: 50m, memory: 32Mi}}
      containers:
        - name: pause
          image: registry.k8s.io/pause:3.10
          securityContext: {allowPrivilegeEscalation: false, readOnlyRootFilesystem: true, capabilities: {drop: [ALL]}}
          resources: {requests: {cpu: 1m, memory: 8Mi}, limits: {cpu: 10m, memory: 16Mi}}
```

Its pods sit in the namespace's `pods` quota (one per node) and carry
none of the plugin's labels, so neither the policies nor the plugin
count them as machines; leave room for them, or run it in another
namespace.

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
the canary runs.

**Each machine's node** (LOOM-208). NetworkPolicy is enforced by an
engine on each node, and the check's canary runs on one. So a `none`
machine is reported `running` only while a canary bound to its own node
(`spec.nodeName`, past the scheduler) has said "enforced" there within
the last 10 minutes. The check's canary counts for the node it landed
on. Until then a machine whose pod is up is `starting` ("checking
network isolation on node …"), and one whose pod isn't up yet keeps its
own status, such as `creating`. The node's canary starts when a get,
list or health first sees the pod on its node, usually while sshd
comes up. The renewer only knows pods it has seen on a node, so one the
scheduler hadn't placed at the host's last ask isn't checked until the
host asks again (and can't be `running` before). A node where the
canary gets out makes each `none` machine there `error`, naming the node and what was reached, and
withdraws `none` from describe and create (a start of a `none` machine
still on that node is refused too); check reports
`network_policy_not_enforced` for that node, whatever its own canary
says, and tries it again with a
pinned canary a minute on, until one whose probe began after the
failure's ended says "enforced"; neither the renewer nor a get tries it before, and a retry
that can't run leaves it open. The pod is left
as it is, like any machine in error: destroy it. A canary that can't
run on the node (no room, the image) proves nothing either way: an
"enforced" from the last 10 minutes still stands, the machine is
`starting` without one, and the node is tried again a minute on. The
plugin keeps the verdicts current itself: every minute it looks at the
nodes of its `none` machines and runs a canary on each whose
"enforced" is more than 5 minutes old, whether or not the host asks,
so a machine in use stays `running`. A renewal that gets out turns the
machines there to `error` (and a verdict that changes their status
tells the host at once, `targets.changed`, rather than at its next
reconcile: LOOM-213); when none can say "enforced" for 10
minutes, they are `starting` until one does. The verdicts live in the
plugin process, so a restarted plugin checks each node again, and its
`none` machines are `starting` until it says "enforced": seconds when the
agent image is on the node, up to the canary's 3-minute timeout when it
is pulled, and a minute more for each try that can't run; so does a
reconfigured one, and a request still under way from before the
configure neither reads nor records the new configuration's verdicts
(a `none` create or start from it is refused: try again). A node's
verdicts go by when their probes ran, by the kubelet's container
times, not when their canaries were started or read (one may wait on
its image and run after one started later): an escape counts unless
the latest "enforced" probe began after it had ended, so one that
overlapped it counts, and without both times it counts. Each canary is a pod in the namespace's quota while
it runs, up to one per node being checked plus the check's own, for each
plugin configuration whose canaries are still live. A restart, a
rollout (old and new sidecar overlapping) or a configure starts a fresh
set while the last one's may still run: a canary lives at most 4
minutes (its 3-minute timeout plus a minute's active deadline, should
its plugin die first), and the new process doesn't adopt them. One that
never got a node (pinned to a node whose kubelet is gone, or
unschedulable) has no deadline, as the kubelet keeps it: a running
plugin sweeps those left behind past that age at configure and once a
minute, so it holds its place up to about a minute longer. Each canary
is deleted once its verdict is recorded, so none is left `Completed`; one
whose delete fails (the API server unavailable) is deleted again by that
sweep. A finished one is out of the quota meanwhile (it counts only pods
not yet `Succeeded` or `Failed`), and an unfinished one counts as above
until it is deleted or its deadline ends it. So leave
`pods` room for `max_environments` machines plus (nodes + 1) × the
configurations that can overlap: 2 for a restart or a rollout, one more
for each further restart within 4 minutes. A canary with no room can't
run, which proves nothing: the node is tried again a minute on, and 10
minutes on the machines are `starting`, never `running` unchecked.

**After it starts** (LOOM-211). A running `none` machine stays
checked: its node by the renewal above, and the agents namespace's
NetworkPolicies by every check, describe and create, and by the plugin
once a minute. Policies found open (one lets a confined machine out, or
none isolates it any more) make each `none` machine `error`, as an open
node does, until a read finds them confining again; `none` is withdrawn
and check reports `network_policy_allows_egress` meanwhile. A read that
fails proves nothing: the last good one stands 10 minutes, then the
machine is `starting`. A change in the policies runs each watched
node's canary again at once rather than at the renewal, until one
started after the change has run (still one at a time per node, and
not within a minute of the node's last, so the quota above holds). The
pod is left running: the host records the `error` at its next reconcile
(`LOOMUX_TARGET_PROBE_INTERVAL`, 5 minutes by default) and refuses to
dispatch to it; whether to destroy it is yours. A read that changes
what the policies make of those machines (open, confining, or
unconfirmed) tells the host at once (`targets.changed`, LOOM-223). The
Role can't watch policies or read nodes, so the minute's list is the
event source.

What it trusts. The canary samples a few destinations; it can't prove
that none other answers. What proves that is the policies, so the
engine must implement NetworkPolicy as specified on each node; the
canary on a machine's node shows it does there, for its destinations,
when the machine comes up and at each renewal. An engine that breaks on
that node between renewals leaves the machine unconfined until the
next one. Policies are
live, and no list is atomic with making a pod: one an admin adds after
that last read, even in the same moment, applies to the machine at
once, and only the next read (at most a minute on) reports it. Holding policy edits back
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
`nodecheck_test.go`: a `none` machine runs only once a canary pinned to
its node says "enforced"; a node that lets one out, or can't run one;
two nodes; renewal in the background, stuck, or unable to run; lapse.
`nodegate_test.go`: a node found open during a create, start or
recreate refuses the pod. `nodecheck_kind_test.go` (`-tags kind`): a
canary pinned to kind's node, and to a node that doesn't exist.
`kind_test.go` (`-tags kind`, CI's `plugin-kubernetes` job): the
conformance suite and an end-to-end run against a kind cluster with
`deploy/test/kind/` applied, as the plugin's ServiceAccount: create,
wait for running, ssh in through a port-forward with the pinned host
key, stop, start, destroy. It needs `KUBECONFIG` set to kind's own
kubeconfig (`kind create cluster --kubeconfig FILE`), current context
`kind-loomux`, the plugin's cluster; otherwise it fails before it
starts. Every kubectl it runs names that file and context, so none
falls back to the default context, which can be a real cluster
(`kindguard_test.go` tests the check).
