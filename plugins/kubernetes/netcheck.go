package kubernetes

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"

	"github.com/Loomux/server/plugins/protocol"
)

// The network-isolation check (design §8). NetworkPolicy is only as
// real as the cluster's policy engine, so the plugin tests enforcement
// rather than assuming it: a short-lived canary pod, confined like an
// egress "none" machine by the §10 policies, tries the API server,
// kube-dns beyond port 53, the link-local metadata address and the
// internet and exits 10, 14, 13, 11 or 0. A new pod can be unconfined for a moment while the engine learns
// its address (about 2 s with kube-network-policies on theWyseKube,
// measured 2026-10-13), so the canary tries a target that answers
// again, canaryAttempts times a second apart, before it calls it
// reachable. The canary's verdict stands only with the policies'
// (policy.go): the namespace's NetworkPolicies deny a confined machine
// all but kube-dns. Not enforced: egress "none" is withdrawn from
// describe and refused at create, and check reports it; with
// require_network_policy, check fails. The canary samples one node; a
// "none" machine runs only once one has said "enforced" on its own
// node (nodecheck.go, LOOM-208).

type enforcementState int

const (
	enforcementUnknown enforcementState = iota
	enforcementPending
	enforcementEnforced
	enforcementNotEnforced
	// enforcementPolicyOpen: the namespace's NetworkPolicies let a
	// confined machine reach more than kube-dns (policy.go).
	enforcementPolicyOpen
	enforcementFailed
)

func (s enforcementState) String() string {
	switch s {
	case enforcementPending:
		return "pending"
	case enforcementEnforced:
		return "enforced"
	case enforcementNotEnforced:
		return "not enforced"
	case enforcementPolicyOpen:
		return "policies open"
	case enforcementFailed:
		return "failed"
	}
	return "unknown"
}

// Canary exit codes. The control is kube-dns, which §10's policy (c)
// allows a confined pod: when even that is unreachable, nothing is
// known about enforcement (pod networking is broken, or the DNS policy
// is missing) and "enforced" must not be claimed.
const (
	exitBlocked        = 0
	exitAPIReachable   = 10
	exitNetReachable   = 11
	exitNoNetwork      = 12
	exitLinkLocalReach = 13
	exitDNSBeyond53    = 14
)

// canaryAttempts bounds how long a target that answers is tried again
// (a second apart) before the canary decides it is reachable: well over
// the engine's start-up gap, well under the canary's deadline.
const canaryAttempts = 15

// verdictTTL is how long a canary's "enforced" serves describe and
// create (and start and recreate): a policy engine removed after a
// check must not leave "none" offered and made. Past half of it they
// start the canary again in the background, so a cluster in use renews
// it before it lapses. Check runs the canary again after a minute,
// whatever it said.
const verdictTTL = 10 * time.Minute

// netcheck is one run of the canary and its verdict.
type netcheck struct {
	done   chan struct{}
	state  enforcementState
	detail string
	at     time.Time
	// node is where it ran: the node it was pinned to, or the one the
	// scheduler gave it ("" if it never got one).
	node string
	// seq and doneSeq number when it started and finished, on one
	// count: a node's verdicts go by them, not by when a canary was read.
	seq, doneSeq uint64
}

// enforcement is the verdict check reports. The namespace's
// NetworkPolicies are read every time; the canary is started when none
// ran in the last minute, and waited for up to wait; one still running
// after that answers pending and finishes in the background for the
// next call. A node a canary found not enforcing is tried again with
// one pinned to it, as often, and stands until one says "enforced"; it
// is the verdict while it stands.
func (p *Plugin) enforcement(ctx context.Context, cfg Config, client kubernetes.Interface, instance string, wait time.Duration) (enforcementState, string) {
	if state, detail := policyVerdict(ctx, client, cfg.Namespace, instance); state != enforcementEnforced {
		return state, detail
	}
	deadline := time.Now().Add(wait)
	p.mu.Lock()
	nc := p.netcheck
	if nc == nil || (nc.state != enforcementPending && p.Now().Sub(nc.at) > time.Minute) {
		nc = p.startCanary(cfg, client, instance)
	}
	retests := p.retestNodes(cfg, client, instance)
	p.mu.Unlock()
	state, detail := p.await(ctx, nc, wait)
	for _, r := range retests {
		p.await(ctx, r, time.Until(deadline))
	}
	// A node known open is the answer, named, whatever this canary says
	// (still running, unable to run, or out: then on its own node).
	if s, d := p.nodeFailure(); s != enforcementEnforced {
		return s, d
	}
	return state, detail
}

// confinement is the verdict describe (wait 0: it never blocks) and
// create (wait CanaryWait) act on: the policies, read now, no node a
// canary found not enforcing, and the last finished canary's verdict,
// "enforced" only within verdictTTL. One older than half that starts
// the canary in the background; with no usable verdict, a running
// canary is waited for up to wait. A verdict other than "enforced"
// stands until a check. For create the policies are read again after
// the canary (and ensurePod reads them last).
func (p *Plugin) confinement(ctx context.Context, cfg Config, client kubernetes.Interface, instance string, wait time.Duration) (enforcementState, string) {
	if state, detail := policyVerdict(ctx, client, cfg.Namespace, instance); state != enforcementEnforced {
		return state, detail
	}
	if state, detail := p.nodeFailure(); state != enforcementEnforced {
		return state, detail
	}
	state, detail := p.lastCanary(ctx, cfg, client, instance, wait)
	if state != enforcementEnforced || wait == 0 {
		return state, detail
	}
	return policyVerdict(ctx, client, cfg.Namespace, instance)
}

// lastCanary is confinement's canary half.
func (p *Plugin) lastCanary(ctx context.Context, cfg Config, client kubernetes.Interface, instance string, wait time.Duration) (enforcementState, string) {
	p.mu.Lock()
	last, now := p.settled, p.Now()
	renew := last == nil || (last.state == enforcementEnforced && now.Sub(last.at) > verdictTTL/2)
	if renew && (p.netcheck == nil || p.netcheck.state != enforcementPending) {
		p.startCanary(cfg, client, instance)
	}
	nc := p.netcheck
	p.mu.Unlock()
	switch {
	case last != nil && last.state == enforcementEnforced && now.Sub(last.at) <= verdictTTL:
		return last.state, last.detail
	case last != nil && last.state != enforcementEnforced:
		return last.state, last.detail
	}
	return p.await(ctx, nc, wait)
}

// startCanary starts a canary as the current check; p.mu is held.
func (p *Plugin) startCanary(cfg Config, client kubernetes.Interface, instance string) *netcheck {
	p.seq++
	nc := &netcheck{done: make(chan struct{}), state: enforcementPending, seq: p.seq}
	p.netcheck = nc
	go p.runCanary(nc, cfg, client, p.apiHost, instance)
	return nc
}

// await is a check's verdict once it has one, waiting up to wait:
// pending while it runs.
func (p *Plugin) await(ctx context.Context, nc *netcheck, wait time.Duration) (enforcementState, string) {
	select {
	case <-nc.done:
	case <-time.After(wait):
	case <-ctx.Done():
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return nc.state, nc.detail
}

func (p *Plugin) runCanary(nc *netcheck, cfg Config, client kubernetes.Interface, apiHost, instance string) {
	ctx, cancel := context.WithTimeout(context.Background(), p.CanaryTimeout)
	defer cancel()
	state, detail, node := p.canary(ctx, cfg, client, apiHost, instance, nc.node)
	p.mu.Lock()
	p.seq++
	nc.state, nc.detail, nc.at, nc.node, nc.doneSeq = state, detail, p.Now(), node, p.seq
	current := p.netcheck == nc // not overtaken by a configure
	if current {
		p.settled = nc
	}
	// Its node's verdict too: a check's canary says as much about the
	// node it landed on as one pinned there.
	if n := p.nodes[node]; n != nil && n.running == nc {
		n.running = nil
		n.record(nc)
	} else if current && node != "" {
		p.nodeOf(node).record(nc)
	}
	p.mu.Unlock()
	close(nc.done)
	p.Logger.Printf("network isolation in %s on node %s: %s (%s)", cfg.Namespace, node, state, detail)
}

// canary runs the pod, pinned to node unless that is "", and reads its
// exit code; ranOn is the node it ran on, as far as it got one.
func (p *Plugin) canary(ctx context.Context, cfg Config, client kubernetes.Interface, apiHost, instance, node string) (state enforcementState, detail, ranOn string) {
	name := "lx-netcheck-" + p.Rand()
	pods := client.CoreV1().Pods(cfg.Namespace)
	p.sweepCanaries(ctx, pods, instance)
	p.mu.Lock()
	p.inflight[name] = true
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.inflight, name)
		p.mu.Unlock()
	}()
	ranOn = node
	if _, err := pods.Create(ctx, canaryPod(name, cfg, instance, apiHost, p.CanaryTimeout, node), metav1.CreateOptions{}); err != nil {
		return enforcementFailed, "couldn't create the canary pod: " + safeStatus(err), ranOn
	}
	defer func() {
		dctx, dcancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer dcancel()
		_ = pods.Delete(dctx, name, metav1.DeleteOptions{GracePeriodSeconds: ptr(int64(0))})
	}()
	var readErr error
	for {
		pod, err := pods.Get(ctx, name, metav1.GetOptions{})
		switch {
		case kerrors.IsNotFound(err):
			return enforcementFailed, "the canary pod disappeared", ranOn
		case kerrors.IsForbidden(err) || kerrors.IsUnauthorized(err):
			return enforcementFailed, "couldn't read the canary pod: " + safeStatus(err), ranOn
		case err != nil:
			// Transient (etcd "leader changed" was seen on TEST): poll
			// again, and report it only if it outlasts the canary. The
			// context's own end is the timeout below, not a read error.
			if ctx.Err() == nil {
				readErr = err
			}
		default:
			readErr = nil
			if ranOn == "" {
				ranOn = pod.Spec.NodeName
			}
			if state, detail, done := canaryVerdict(pod); done {
				return state, detail, ranOn
			}
		}
		select {
		case <-ctx.Done():
			if readErr != nil {
				return enforcementFailed, "couldn't read the canary pod: " + safeStatus(readErr), ranOn
			}
			return enforcementFailed, "the canary didn't finish within " + p.CanaryTimeout.String(), ranOn
		case <-time.After(p.Poll):
		}
	}
}

// sweepCanaries deletes this server's canaries left by an earlier check
// whose own delete failed (the API server was unavailable): those that
// have terminated, or have outlived any check's wait for them. A live
// one may be another plugin process's (an old and a new sidecar
// overlapping in a rollout) and is left alone, as is one this process
// is still reading (canaries run at once, one per node). Best effort,
// like that delete: the next check tries again.
func (p *Plugin) sweepCanaries(ctx context.Context, pods corev1client.PodInterface, instance string) {
	list, err := pods.List(ctx, metav1.ListOptions{LabelSelector: instanceSelector(instance) + "," + labelCanary + "=true"})
	if err != nil {
		return
	}
	for _, pod := range list.Items {
		p.mu.Lock()
		reading := p.inflight[pod.Name]
		p.mu.Unlock()
		if reading {
			continue
		}
		terminated := pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed
		if terminated || p.Now().Sub(pod.CreationTimestamp.Time) > p.CanaryTimeout+time.Minute {
			_ = pods.Delete(ctx, pod.Name, metav1.DeleteOptions{GracePeriodSeconds: ptr(int64(0))})
		}
	}
}

// canaryVerdict is the verdict a read of the canary pod gives, once it
// gives one: its exit code, or an image it can't run.
func canaryVerdict(pod *corev1.Pod) (enforcementState, string, bool) {
	if code, reason, done := canaryExit(pod); done {
		switch code {
		case exitBlocked:
			return enforcementEnforced, "a confined pod reached neither the API server, kube-dns beyond port 53, the metadata address nor the internet", true
		case exitAPIReachable:
			return enforcementNotEnforced, "a confined pod reached the API server", true
		case exitNetReachable:
			return enforcementNotEnforced, "a confined pod reached the internet", true
		case exitLinkLocalReach:
			return enforcementNotEnforced, "a confined pod reached the link-local metadata address " + metadataAddr, true
		case exitDNSBeyond53:
			return enforcementNotEnforced, "a confined pod reached kube-dns beyond port 53 (its metrics port, 9153)", true
		case exitNoNetwork:
			return enforcementFailed, "a confined pod couldn't reach kube-dns either: pod networking is broken, or the DNS policy of the agents namespace is missing", true
		default:
			return enforcementFailed, fmt.Sprintf("the canary exited %d: %s", code, reason), true
		}
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Waiting != nil && imageErrors[cs.State.Waiting.Reason] {
			return enforcementFailed, "the canary can't run the agent image: " + cs.State.Waiting.Reason + " " + oneLine(cs.State.Waiting.Message), true
		}
	}
	return enforcementUnknown, "", false
}

// canaryExit is the canary's exit code once it has one.
func canaryExit(pod *corev1.Pod) (code int, reason string, done bool) {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Terminated != nil {
			return int(cs.State.Terminated.ExitCode), oneLine(cs.State.Terminated.Reason + " " + cs.State.Terminated.Message), true
		}
	}
	if pod.Status.Phase == corev1.PodFailed {
		return -1, oneLine(pod.Status.Reason + " " + pod.Status.Message), true
	}
	return 0, "", false
}

// metadataAddr is the link-local address clouds serve instance
// metadata (and credentials) at.
const metadataAddr = "169.254.169.254"

// canaryScript probes the API server (by the client's address and by
// its cluster name), kube-dns (the pod's resolver) on its metrics port,
// the metadata address and the internet (an address and a name), all at
// once; one that still answers after canaryAttempts tries decides, the
// earliest listed first. kube-dns on 9153 is the in-cluster sample: no
// firewall outside the cluster stands between it and the pod, and §10's
// (c) allows port 53 only, so only an engine enforcing that keeps it
// out. When none answers, kube-dns on port 53 is the control: a
// confined pod may reach it, so reaching it says the pod has a network
// and the policies hold (exit 0), and not reaching it says nothing can
// be concluded (exit 12). These are samples: what decides that nothing
// else is reachable either is the policies (policy.go).
func canaryScript(apiHost string) string {
	type target struct {
		host, port string
		code       int
	}
	var targets []target
	if h, port, err := net.SplitHostPort(apiHost); err == nil && h != "" {
		targets = append(targets, target{h, port, exitAPIReachable})
	}
	targets = append(targets,
		target{"kubernetes.default.svc.cluster.local", "443", exitAPIReachable},
		target{`"$dns"`, "9153", exitDNSBeyond53},
		target{metadataAddr, "80", exitLinkLocalReach},
		target{"1.1.1.1", "443", exitNetReachable},
		target{"github.com", "443", exitNetReachable},
	)
	script := "dns=$(awk '/^nameserver/ { print $2; exit }' /etc/resolv.conf)\n"
	script += "probe() { timeout 3 bash -c \"exec 3<>/dev/tcp/$1/$2\" 2>/dev/null; }\n"
	script += "reached() {\n"
	for i, t := range targets {
		script += fmt.Sprintf("probe %s %s & p%d=$!\n", t.host, t.port, i)
	}
	script += "code=0\n"
	for i := len(targets) - 1; i >= 0; i-- {
		script += fmt.Sprintf("wait $p%d && code=%d\n", i, targets[i].code)
	}
	script += "return $code\n}\n"
	script += fmt.Sprintf("for i in $(seq 1 %d); do\n", canaryAttempts)
	script += "reached; code=$?\n"
	script += "[ $code -eq 0 ] && break\n"
	script += fmt.Sprintf("[ $i -eq %d ] && exit $code\n", canaryAttempts)
	script += "sleep 1\ndone\n"
	script += "[ -n \"$dns\" ] && probe \"$dns\" 53 || exit 12\n"
	script += "exit 0\n"
	return script
}

// canaryPod is restricted-compliant like every pod the plugin makes,
// labelled as a confined machine (a target and an environment
// included) so the policies apply to it as to one, and marked as a
// canary so nothing lists it as a machine. Its deadline, which the
// kubelet counts from scheduling (the image pull included), outlasts the
// plugin's wait for it, so it only ends a canary nobody reads. With a
// node it is bound there (spec.nodeName), past the scheduler.
func canaryPod(name string, cfg Config, instance, apiHost string, timeout time.Duration, node string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				labelManagedBy: managedBy, protocol.LabelInstance: instance,
				protocol.LabelTarget: "netcheck", protocol.LabelEnvironment: strings.TrimPrefix(name, "lx-"),
				labelRole: roleAgent, labelEgress: protocol.EgressNone, labelCanary: "true",
			},
		},
		Spec: corev1.PodSpec{
			NodeName:                     node,
			AutomountServiceAccountToken: ptr(false),
			EnableServiceLinks:           ptr(false),
			RestartPolicy:                corev1.RestartPolicyNever,
			ActiveDeadlineSeconds:        ptr(int64((timeout + time.Minute) / time.Second)),
			SecurityContext:              podSecurity(),
			Containers: []corev1.Container{{
				Name:    "netcheck",
				Image:   cfg.AgentImage,
				Command: []string{"bash", "-c", canaryScript(apiHost)},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("64Mi")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
				},
				SecurityContext: containerSecurity(),
			}},
		},
	}
}
