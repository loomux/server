# LOOM-178: Kubernetes plugin end to end on the test instance (2026-10-20)

**Result: passed on the second run.** A machine created through the API was
ready 13.8 s after the request. An agent command ran on it through the
router; from inside the pod the API server was blocked and the internet was
reachable. Deleting the target removed its workspace, task, pod, PVC, Secret
and PV within 20 s. The first run, an hour earlier, never got a running pod:
the instance's uplink was down to about 0.1 MB/s and the 542 MB agent image
arrived corrupted five times. That run is kept below because it surfaced two
small host bugs, fixed in this PR in their own commits (§6 A and B), and one
operational gap (§6 C).

Server `0.3.82 (web 0.3.11)`, API `v1`, plugin `kubernetes 0.3.82` as a
sidecar (`source: socket`), plugin configuration
`require_network_policy: false`, `storage_class: ceph-rbd-sc-delete`,
namespace and subdomain `loomux-agents`, agent image
`ghcr.io/loomux/agent:sha-2ea73bd@sha256:da7bbec7…`. All times UTC. The flow
was the documented API (`POST /api/v1/targets` with a `plugin` object,
`GET /api/v1/targets/{id}`, `POST /api/v1/dispatch`,
`GET /api/v1/tasks/{id}/attach-info`, `DELETE /api/v1/targets/{id}`,
`POST /api/v1/plugins/{id}/check`) as an admin; the cluster was only read.

## 1. Deployment checks (read-only, 19:26–19:30)

| Check | Expected (design) | Seen |
|---|---|---|
| loomuxd pod | sidecar `plugin-kubernetes` beside `loomuxd` | 3/3 Running, 0 restarts, 7d20h; sidecar ready, image `plugin-kubernetes:0.3.82@sha256:814595e0…` |
| sidecar container | §10 item 3: uid 10003, gid 10001, read-only root, caps dropped, no escalation, `--listen /run/loomux/plugins/kubernetes.sock` | as expected |
| ServiceAccount | §10 item 2: `loomux-plugin-kubernetes`, pod `automountServiceAccountToken: false`, projected token in the plugin container only | as expected; `loomuxd` mounts only `data` and `plugins-sock` |
| Role in `loomux-agents` | §8: pods c/g/l/w/d, pods/log g, PVCs c/g/l/d, secrets c/g/l/u/d, events l, services g | rule for rule; one RoleBinding to the SA, no ClusterRoleBinding |
| `can-i` as the SA | nothing more | `pods --subresource=exec` no, `services` create no, `deployments.apps` no, `networkpolicies` create no, pods in loomuxd's namespace no, cluster-wide list no |
| namespace | §10 item 1: PSA restricted (enforce/warn/audit) | as expected |
| policies, Service, quota | §10 items 4, 6, 7 | headless Service `loomux-agents`, 4 NetworkPolicies, ResourceQuota, LimitRange present |
| baseline | | no pods, PVCs or machine Secrets |

## 2. Run 2: the machine end to end (21:47–21:50)

`POST /api/v1/targets`
`{name: "e2e-loom178-k8s-2", plugin: {size: small, persistent: true, egress: internet}}`
answered `201` in 1.2 s: target `f9452d44-…`, environment `yuaxgx3ymf`, host
`lx-yuaxgx3ymf.loomux-agents.loomux-agents.svc.cluster.local:2222`, user
`agent`, workspace root `/data/work`, a managed ed25519 key, and the host key
pinned before the machine existed.

| Time | API (`GET /targets/{id}`, polled every second) | Cluster |
|---|---|---|
| 21:47:51.7 | `201`, `plugin.status: creating` | |
| 21:47:54.7 | reason `unschedulable: … unbound immediate PersistentVolumeClaims` (for one second, while the PVC bound) | |
| 21:47:55 | reason `ContainerCreating` | PV bound, pod scheduled, volume attached |
| 21:47:58–59 | | image already on the node; container created and started |
| 21:48:00 | | container ready (tcp 2222) |
| 21:48:02.9 | `plugin.status: running`, `image_digest` recorded | loomuxd logs `machine running` |
| 21:48:04.3 | **`ready: true`**, health `healthy`, `tmux 3.3a`, ~10.4 GB free, `next_step: null` | |

**The agent command.** `POST /api/v1/dispatch` asked the router to run, on
`e2e-loom178-k8s-2`:

```sh
echo loom178-ok; id -u; hostname
(timeout 3 bash -c 'exec 3<>/dev/tcp/10.96.0.1/443' && echo apiserver-REACHABLE || echo apiserver-blocked)
(timeout 5 bash -c 'exec 3<>/dev/tcp/1.1.1.1/443' && echo internet-ok || echo internet-blocked)
```

The router quoted it and asked for confirmation (21:48:29); `yes`
(21:48:37.6) ran it, and at 21:48:44.2 the dispatch succeeded with exit 0:

```
loom178-ok
10002
lx-yuaxgx3ymf
apiserver-blocked
internet-ok
```

So the command ran in the machine as uid 10002, and under the `internet`
egress policy the API server's ClusterIP was blocked while the internet was
reachable. That is §10 item 6(d), measured about 40 s after the pod started, long
after the policy engine's start-up gap. The router made a workspace
`shell@e2e-loom178-k8s-2` and a task; `GET /tasks/{id}/attach-info` returned
`attach_command` plus `attach_commands` with `via: kubectl`
(`kubectl -n loomux-agents exec -it lx-yuaxgx3ymf -- tmux -L loomux attach -t …`)
and `via: ssh`.

**Deleting the target.** `DELETE /api/v1/targets/f9452d44-…` at 21:49:26.8
answered `204` in 1.8 s. Then `GET` on the target answered `404`, the
workspace was gone from the list, and the task's attach-info answered
`404`. loomuxd logged `machine destroyed`. The pod, PVC and Secret were
gone by 21:49:35.8 (9.0 s after the request) and the PV by 21:49:46.9
(20.2 s). The namespace was left with only the headless Service, and quota
use was back to zero.

**The plugin's own network check** read `network_policy_not_enforced` at
21:47:37 (a canary on a node that had the image, answered in 6 s). This is
the start-up race measured on 2026-10-13: a new pod is unconfined for about
2 s, and 0.3.82 tries once. The retries that fix it (#374) are on main but
not yet on the test instance. The command above, run later in the pod's
life, is the observation that counts.

## 3. Run 1: an image that wouldn't pull (19:36–21:45)

**Check.** `POST /plugins/{id}/check` at 19:36:16 made canary
`lx-netcheck-9ad0b494`. The image wasn't on its node, the pull outlasted the
canary's 3 minutes, and the plugin deleted the canary at 19:39:17
(`the canary didn't finish within 3m0s`). The API said
`network_policy_check_pending`, as documented. A later canary, at 21:45, got
`etcdserver: request timed out` reading its pod. That was transient:
`/readyz` and `/livez/etcd` were fine a minute later.

**Create.** `e2e-loom178-k8s` (target `90cc91c1-…`, environment
`pe5lbbas5c`), same body as run 2, answered `201` at 19:39:50. The Secret,
the PVC, a 10Gi PV and the pod followed within 5 s. The pull started at
19:39:58. At 19:49:52 the host's 10-minute create deadline passed and the
target went to `plugin.status: error`, with the reason
`sqlite: update environment: context deadline exceeded` (§6 A). During
those ten minutes the API showed `next_step: test_connection` (§6 B).

**The pull.** It failed five times, at 19:53, 20:42, 21:12, 21:24 and
21:38. The first failure was on layer `f1f5860b…` (54 MB) and the rest on
`440bd28e…` (406 MB); each was a digest mismatch with a different wrong
digest. The registry wasn't at fault: from a workstation on the same
uplink, the 54 MB layer took 349 s and arrived truncated (42 MB, wrong
digest). Unrelated hosts (a speed test, a GitHub release) measured
0.09–0.13 MB/s, and GitHub's status page showed Packages operational. By
21:45 the uplink was back to about 1.6 MB/s.

**Delete.** `DELETE` at 21:39:17 answered `204` in 2.4 s, and loomuxd
logged `machine destroyed`. The Secret went at once. The pod got its
deletion timestamp (5 s grace, no finalizers) but stayed while a pull that
had started at 21:38:26 was in progress. That pull succeeded at 21:44:17
(5m50s); the container started and was stopped straight away. The pod and
PVC were gone at 21:44:40 and the PV at 21:44:46. Nothing needed doing by
hand.

## 4. Pod spec as made

It matched §8: `runAsNonRoot`, uid/gid/fsGroup 10002, seccomp
`RuntimeDefault`, read-only root, all capabilities dropped, no service
account token, no service links, requests equal to limits (1 CPU, 2Gi
memory, 2Gi ephemeral) for `small`. Labels: `loomux.io/role=agent`,
`loomux.io/egress=internet`, `loomux.io/target`, `loomux.io/environment`,
`loomux.io/instance`, `app.kubernetes.io/managed-by=loomux-plugin-kubernetes`.

## 5. Timings

| | Run 2 |
|---|---|
| request → `201` | 1.2 s |
| request → pod ready | 9.5 s (image cached) |
| request → target `ready` | 13.8 s |
| dispatch confirmed → output | 6.6 s |
| `DELETE` → `204` | 1.8 s |
| `DELETE` → pod, PVC, Secret gone | 9.0 s |
| `DELETE` → PV gone | 20.2 s |

## 6. Findings

**A. A deadline that passed during a write was reported as the write's
error.** Fixed, in `providers`. The create job's 10-minute deadline passed
while `follow` was writing the plugin's latest status. The write failed
with the expired context, and `failJob` recorded that error as the reason:
`sqlite: update environment: context deadline exceeded` instead of
`the machine didn't become ready within 10m0s`. `failJob` now names the
deadline whenever the job's context has passed it. Test:
`TestDeadlineDuringWriteRecordsTheDeadline`, which uses a store that holds
the write until the deadline, as sqlite does.

**B. `next_step` was `test_connection` while a machine came up.** Fixed, in
`api`. Design §5 says "`next_step` stays null while the machine is coming
up, and `plugin.status` says why". The target responses computed it from
the probe alone, so for the ten minutes the image pulled, the API pointed
at a connection test that could only fail. It is now null while the
machine is `creating`, `starting` or `recreating`. Test:
`TestMachines_NoNextStepWhileComingUp`.

**C. A cold image pull can outlast both deadlines.** Not fixed; for
discussion. The canary has 3 minutes and a machine 10. A first pull of the
542 MB image on a slow link takes longer than either, and the machine then
stays `error` until someone presses Start, even after the pull finishes
(reconcile leaves `error` alone, by design). The cheapest remedy is
deployment work: pre-pull the agent image on the nodes, for example with a
DaemonSet whose container runs `true` from the image, pinned like the
plugin's `agent_image`. Other options are not counting an image pull
against the deadline, or a smaller image.

**D. A transient scheduling message shows as `status_reason`.** Cosmetic.
For about a second while the PVC binds, the reason reads
`unschedulable: 0/4 nodes are available: pod has unbound immediate
PersistentVolumeClaims…`. That is normal for an immediate-binding storage
class, but it reads like a failure. The plugin could leave out
`FailedScheduling` for unbound PVCs while the PVC is still pending.
Not changed here.
