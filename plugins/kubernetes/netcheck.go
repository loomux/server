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

	"github.com/Loomux/server/plugins/protocol"
)

// The network-isolation check (design §8). NetworkPolicy is only as
// real as the cluster's policy engine, so the plugin tests enforcement
// rather than assuming it: a short-lived canary pod, confined like an
// egress "none" machine by the §10 policies, tries the API server and
// the internet and exits 10, 11 or 0. Not enforced: egress "none" is
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

// Canary exit codes.
const (
	exitBlocked      = 0
	exitAPIReachable = 10
	exitNetReachable = 11
)

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
	if _, err := pods.Create(ctx, canaryPod(name, cfg, instance, apiHost), metav1.CreateOptions{}); err != nil {
		return enforcementFailed, "couldn't create the canary pod: " + safeStatus(err)
	}
	defer func() {
		dctx, dcancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer dcancel()
		_ = pods.Delete(dctx, name, metav1.DeleteOptions{GracePeriodSeconds: ptr(int64(0))})
	}()
	for {
		pod, err := pods.Get(ctx, name, metav1.GetOptions{})
		switch {
		case kerrors.IsNotFound(err):
			return enforcementFailed, "the canary pod disappeared"
		case err != nil:
			return enforcementFailed, "couldn't read the canary pod: " + safeStatus(err)
		}
		if code, reason, done := canaryExit(pod); done {
			switch code {
			case exitBlocked:
				return enforcementEnforced, "a confined pod reached neither the API server nor the internet"
			case exitAPIReachable:
				return enforcementNotEnforced, "a confined pod reached the API server"
			case exitNetReachable:
				return enforcementNotEnforced, "a confined pod reached the internet"
			default:
				return enforcementFailed, fmt.Sprintf("the canary exited %d: %s", code, reason)
			}
		}
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.State.Waiting != nil && imageErrors[cs.State.Waiting.Reason] {
				return enforcementFailed, "the canary can't run the agent image: " + cs.State.Waiting.Reason + " " + oneLine(cs.State.Waiting.Message)
			}
		}
		select {
		case <-ctx.Done():
			return enforcementFailed, "the canary didn't finish within " + p.CanaryTimeout.String()
		case <-time.After(p.Poll):
		}
	}
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
// its cluster name) and the internet; the first that answers decides.
func canaryScript(apiHost string) string {
	script := "probe() { timeout 3 bash -c \"exec 3<>/dev/tcp/$1/$2\" 2>/dev/null; }\n"
	if h, port, err := net.SplitHostPort(apiHost); err == nil && h != "" {
		script += "probe " + h + " " + port + " && exit 10\n"
	}
	script += "probe kubernetes.default.svc.cluster.local 443 && exit 10\n"
	script += "probe 1.1.1.1 443 && exit 11\n"
	script += "exit 0\n"
	return script
}

// canaryPod is restricted-compliant like every pod the plugin makes,
// labelled as a confined agent so the policies apply to it, and marked
// as a canary so nothing lists it as a machine.
func canaryPod(name string, cfg Config, instance, apiHost string) *corev1.Pod {
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
			ActiveDeadlineSeconds:        ptr(int64(60)),
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
