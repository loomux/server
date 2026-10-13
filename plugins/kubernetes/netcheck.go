package kubernetes

import (
	"context"
	"fmt"
	"net"
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
// egress "none" machine by the §10 policies, tries the API server and
// the internet and exits 10, 11 or 0. A new pod can be unconfined for a
// moment while the engine learns its address (about 2 s with
// kube-network-policies on theWyseKube, measured 2026-10-13), so the
// canary tries a target that answers again, canaryAttempts times a
// second apart, before it calls it reachable. Not enforced: egress "none" is
// withdrawn from describe and check reports it; with
// require_network_policy, check fails.

type enforcementState int

const (
	enforcementUnknown enforcementState = iota
	enforcementPending
	enforcementEnforced
	enforcementNotEnforced
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
	exitBlocked      = 0
	exitAPIReachable = 10
	exitNetReachable = 11
	exitNoNetwork    = 12
)

// canaryAttempts bounds how long a target that answers is tried again
// (a second apart) before the canary decides it is reachable: well over
// the engine's start-up gap, well under the canary's deadline.
const canaryAttempts = 15

// netcheck is one run of the canary and its verdict.
type netcheck struct {
	done   chan struct{}
	state  enforcementState
	detail string
	at     time.Time
}

// verdict is the last verdict, without starting a check.
func (p *Plugin) verdict() (enforcementState, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.netcheck == nil {
		return enforcementUnknown, ""
	}
	return p.netcheck.state, p.netcheck.detail
}

// enforcement is the verdict for this configuration: a check is started
// when none ran in the last minute, and waited for up to wait; a check
// still running after that answers pending and finishes in the
// background for the next call.
func (p *Plugin) enforcement(ctx context.Context, cfg Config, client kubernetes.Interface, wait time.Duration) (enforcementState, string) {
	p.mu.Lock()
	nc := p.netcheck
	if nc == nil || (nc.state != enforcementPending && p.Now().Sub(nc.at) > time.Minute) {
		nc = &netcheck{done: make(chan struct{}), state: enforcementPending}
		p.netcheck = nc
		apiHost := p.apiHost
		instance := p.host.InstanceID
		go p.runCanary(nc, cfg, client, apiHost, instance)
	}
	p.mu.Unlock()
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
	state, detail := p.canary(ctx, cfg, client, apiHost, instance)
	p.mu.Lock()
	nc.state, nc.detail, nc.at = state, detail, p.Now()
	p.mu.Unlock()
	close(nc.done)
	p.Logger.Printf("network isolation in %s: %s (%s)", cfg.Namespace, state, detail)
}

// canary runs the pod and reads its exit code.
func (p *Plugin) canary(ctx context.Context, cfg Config, client kubernetes.Interface, apiHost, instance string) (enforcementState, string) {
	name := "lx-netcheck-" + p.Rand()
	pods := client.CoreV1().Pods(cfg.Namespace)
	sweepCanaries(ctx, pods, instance)
	if _, err := pods.Create(ctx, canaryPod(name, cfg, instance, apiHost, p.CanaryTimeout), metav1.CreateOptions{}); err != nil {
		return enforcementFailed, "couldn't create the canary pod: " + safeStatus(err)
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
			return enforcementFailed, "the canary pod disappeared"
		case err != nil:
			// Transient (etcd "leader changed" was seen on TEST): poll
			// again, and report it only if it outlasts the canary.
			readErr = err
		default:
			if state, detail, done := canaryVerdict(pod); done {
				return state, detail
			}
		}
		select {
		case <-ctx.Done():
			if readErr != nil {
				return enforcementFailed, "couldn't read the canary pod: " + safeStatus(readErr)
			}
			return enforcementFailed, "the canary didn't finish within " + p.CanaryTimeout.String()
		case <-time.After(p.Poll):
		}
	}
}

// sweepCanaries deletes this server's canaries left by an earlier check
// whose own delete failed (the API server was unavailable). Best effort,
// like that delete: the next check tries again.
func sweepCanaries(ctx context.Context, pods corev1client.PodInterface, instance string) {
	list, err := pods.List(ctx, metav1.ListOptions{LabelSelector: instanceSelector(instance) + "," + labelCanary + "=true"})
	if err != nil {
		return
	}
	for _, pod := range list.Items {
		_ = pods.Delete(ctx, pod.Name, metav1.DeleteOptions{GracePeriodSeconds: ptr(int64(0))})
	}
}

// canaryVerdict is the verdict a read of the canary pod gives, once it
// gives one: its exit code, or an image it can't run.
func canaryVerdict(pod *corev1.Pod) (enforcementState, string, bool) {
	if code, reason, done := canaryExit(pod); done {
		switch code {
		case exitBlocked:
			return enforcementEnforced, "a confined pod reached neither the API server nor the internet", true
		case exitAPIReachable:
			return enforcementNotEnforced, "a confined pod reached the API server", true
		case exitNetReachable:
			return enforcementNotEnforced, "a confined pod reached the internet", true
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

// canaryScript probes the API server (by the client's address and by
// its cluster name) and the internet; the first that still answers
// after canaryAttempts tries decides. When neither does, kube-dns (the
// pod's resolver) is the control: a confined pod may reach it, so reaching it says the pod has a network
// and the policies hold (exit 0), and not reaching it says nothing can
// be concluded (exit 12).
func canaryScript(apiHost string) string {
	script := "probe() { timeout 3 bash -c \"exec 3<>/dev/tcp/$1/$2\" 2>/dev/null; }\n"
	script += "reached() {\n"
	if h, port, err := net.SplitHostPort(apiHost); err == nil && h != "" {
		script += "probe " + h + " " + port + " && return 10\n"
	}
	script += "probe kubernetes.default.svc.cluster.local 443 && return 10\n"
	script += "probe 1.1.1.1 443 && return 11\n"
	script += "return 0\n}\n"
	script += fmt.Sprintf("for i in $(seq 1 %d); do\n", canaryAttempts)
	script += "reached; code=$?\n"
	script += "[ $code -eq 0 ] && break\n"
	script += fmt.Sprintf("[ $i -eq %d ] && exit $code\n", canaryAttempts)
	script += "sleep 1\ndone\n"
	script += "dns=$(awk '/^nameserver/ { print $2; exit }' /etc/resolv.conf)\n"
	script += "[ -n \"$dns\" ] && probe \"$dns\" 53 || exit 12\n"
	script += "exit 0\n"
	return script
}

// canaryPod is restricted-compliant like every pod the plugin makes,
// labelled as a confined agent so the policies apply to it, and marked
// as a canary so nothing lists it as a machine. Its deadline, which the
// kubelet counts from scheduling (the image pull included), outlasts the
// plugin's wait for it, so it only ends a canary nobody reads.
func canaryPod(name string, cfg Config, instance, apiHost string, timeout time.Duration) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				labelManagedBy: managedBy, protocol.LabelInstance: instance,
				labelRole: roleAgent, labelEgress: protocol.EgressNone, labelCanary: "true",
			},
		},
		Spec: corev1.PodSpec{
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
