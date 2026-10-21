package kubernetes

import (
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/Loomux/server/plugins/protocol"
	"github.com/Loomux/server/plugins/rpc"
)

// kindPolicies are the §10 NetworkPolicies as CI applies them to kind
// (deploy/test/kind/30-networkpolicies.yaml), but those named in drop.
func kindPolicies(t *testing.T, drop ...string) []runtime.Object {
	t.Helper()
	f, err := os.Open("../../deploy/test/kind/30-networkpolicies.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	dec := yaml.NewYAMLOrJSONDecoder(f, 4096)
	var out []runtime.Object
	for {
		var np networkingv1.NetworkPolicy
		if err := dec.Decode(&np); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if np.Name == "" || slices.Contains(drop, np.Name) {
			continue
		}
		out = append(out, &np)
	}
	if len(out) == 0 {
		t.Fatal("no NetworkPolicies in the kind manifests")
	}
	return out
}

// egressPolicy allows the pods sel selects egress to peers on ports
// (any port when none).
func egressPolicy(name string, sel map[string]string, peers []networkingv1.NetworkPolicyPeer, ports ...networkingv1.NetworkPolicyPort) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: sel},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress:      []networkingv1.NetworkPolicyEgressRule{{To: peers, Ports: ports}},
		},
	}
}

func cidr(c string) []networkingv1.NetworkPolicyPeer {
	return []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: c}}}
}

func kubeDNS(ns map[string]string) []networkingv1.NetworkPolicyPeer {
	return []networkingv1.NetworkPolicyPeer{{
		NamespaceSelector: &metav1.LabelSelector{MatchLabels: ns},
		PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"k8s-app": "kube-dns"}},
	}}
}

func port(proto corev1.Protocol, p int) networkingv1.NetworkPolicyPort {
	return networkingv1.NetworkPolicyPort{Protocol: &proto, Port: ptr(intstr.FromInt32(int32(p)))}
}

// TestNoneRestsOnDenyAll (LOOM-201): the canary only tries a few
// destinations, so a policy letting a confined machine reach another
// one (the metadata address, a LAN service) used to pass the check, and
// egress "none" was offered and made while that destination stayed
// open. "none" now also rests on the policies themselves: every one
// that may select a confined machine allows it kube-dns on port 53 and
// nothing else, and one isolates every such machine for egress.
func TestNoneRestsOnDenyAll(t *testing.T) {
	agents := map[string]string{labelRole: roleAgent}
	confined := map[string]string{labelRole: roleAgent, labelEgress: protocol.EgressNone}
	kubeSystem := map[string]string{"kubernetes.io/metadata.name": "kube-system"}
	for _, tc := range []struct {
		name   string
		drop   []string
		extra  []runtime.Object
		detail string // the reason none isn't offered; empty: it is
	}{
		{name: "the §10 policies"},
		{name: "an additive policy lets agents reach the metadata address",
			extra:  []runtime.Object{egressPolicy("allow-metadata", agents, cidr("169.254.169.254/32"))},
			detail: "169.254.169.254/32"},
		{name: "one lets confined machines reach a LAN service",
			extra: []runtime.Object{egressPolicy("allow-db", confined, []networkingv1.NetworkPolicyPeer{{
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "lan"}},
				PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"app": "db"}},
			}}, port(corev1.ProtocolTCP, 5432))},
			detail: "allow-db"},
		{name: "one lets one machine out, by its environment label",
			extra:  []runtime.Object{egressPolicy("allow-one", map[string]string{protocol.LabelEnvironment: "abc123"}, cidr("0.0.0.0/0"))},
			detail: "0.0.0.0/0"},
		{name: "one allows every destination",
			extra:  []runtime.Object{egressPolicy("allow-all", agents, nil)},
			detail: "any destination"},
		{name: "kube-dns on any port",
			extra:  []runtime.Object{egressPolicy("dns-any-port", agents, kubeDNS(kubeSystem))},
			detail: "any port"},
		{name: "kube-dns on another port",
			extra:  []runtime.Object{egressPolicy("dns-metrics", agents, kubeDNS(kubeSystem), port(corev1.ProtocolTCP, 9153))},
			detail: "dns-metrics"},
		{name: "pods called kube-dns in any namespace",
			extra:  []runtime.Object{egressPolicy("dns-anywhere", agents, kubeDNS(map[string]string{}), port(corev1.ProtocolUDP, 53))},
			detail: "dns-anywhere"},
		{name: "egress rules with no policy types say Egress",
			extra: []runtime.Object{&networkingv1.NetworkPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: "implicit", Namespace: testNS},
				Spec: networkingv1.NetworkPolicySpec{
					PodSelector: metav1.LabelSelector{MatchLabels: agents},
					Egress:      []networkingv1.NetworkPolicyEgressRule{{To: cidr("10.0.0.0/8")}},
				},
			}},
			detail: "10.0.0.0/8"},
		{name: "nothing isolates confined machines for egress",
			drop:   []string{"agents-default-deny", "agents-allow-dns"},
			detail: "isolates"},
		{name: "the default deny alone is enough", drop: []string{"agents-allow-dns"}},
		{name: "another instance's machines may go anywhere",
			extra: []runtime.Object{egressPolicy("other", map[string]string{protocol.LabelInstance: "another-instance"}, cidr("0.0.0.0/0"))}},
		{name: "a second policy for internet machines",
			extra: []runtime.Object{egressPolicy("more-internet", map[string]string{labelEgress: protocol.EgressInternet}, cidr("0.0.0.0/0"))}},
		{name: "an ingress-only policy's egress rules don't apply",
			extra: []runtime.Object{&networkingv1.NetworkPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: "ingress-only", Namespace: testNS},
				Spec: networkingv1.NetworkPolicySpec{
					PodSelector: metav1.LabelSelector{MatchLabels: agents},
					PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
					Egress:      []networkingv1.NetworkPolicyEgressRule{{To: cidr("0.0.0.0/0")}},
				},
			}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			objs := append(kindPolicies(t, tc.drop...), tc.extra...)
			objs = append(objs, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "loomux-agents", Namespace: testNS}})
			cs := fake.NewClientset(objs...)
			canaryExits(cs, exitBlocked) // the canary's destinations are blocked either way
			p := configured(t, cs, nil)
			res, err := p.Check(ctx)
			if err != nil {
				t.Fatal(err)
			}
			info, err := p.DescribeTargets(ctx)
			if err != nil {
				t.Fatal(err)
			}
			sp := spec("conf1")
			sp.Egress = protocol.EgressNone
			_, createErr := p.CreateTarget(ctx, sp)
			if tc.detail == "" {
				if !res.OK || len(res.Problems) != 0 {
					t.Errorf("check = %+v", res)
				}
				if len(info.EgressOptions) != 2 {
					t.Errorf("egress options = %v, want internet and none", info.EgressOptions)
				}
				if createErr != nil {
					t.Errorf("create with egress none: %v", createErr)
				}
				return
			}
			var pr *protocol.Problem
			for i := range res.Problems {
				if res.Problems[i].Code == "network_policy_allows_egress" {
					pr = &res.Problems[i]
				}
			}
			if pr == nil || pr.Severity != protocol.SeverityWarning || !strings.Contains(pr.Message, tc.detail) {
				t.Errorf("check = %+v, want network_policy_allows_egress naming %q", res, tc.detail)
			}
			if len(info.EgressOptions) != 1 || info.EgressOptions[0] != protocol.EgressInternet {
				t.Errorf("egress options = %v, want internet only", info.EgressOptions)
			}
			if rpcCode(createErr) != rpc.CodeInvalidParams {
				t.Errorf("create with egress none = %v, want invalid_params", createErr)
			}
			if _, err := cs.CoreV1().Pods(testNS).Get(ctx, podName("conf1"), metav1.GetOptions{}); err == nil {
				t.Error("a pod labelled egress none was made")
			}
		})
	}
}

// agentsService is the headless Service, so a check has nothing else
// to say.
func agentsService() *corev1.Service {
	return &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "loomux-agents", Namespace: testNS}}
}

func confinedSpec(id string) protocol.EnvironmentSpec {
	sp := spec(id)
	sp.Egress = protocol.EgressNone
	return sp
}

// settle waits for the plugin's latest canary to finish.
func settle(t *testing.T, p *Plugin) {
	t.Helper()
	p.mu.Lock()
	nc := p.netcheck
	p.mu.Unlock()
	if nc == nil {
		return
	}
	select {
	case <-nc.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the canary didn't finish")
	}
}

// canaries is how many canary pods the plugin has made.
func canaries(cs *fake.Clientset) int {
	n := 0
	for _, a := range cs.Actions() {
		if c, ok := a.(k8stesting.CreateAction); ok && a.GetResource().Resource == "pods" {
			if pod, ok := c.GetObject().(*corev1.Pod); ok && pod.Labels[labelCanary] == "true" {
				n++
			}
		}
	}
	return n
}

// TestNoneVerdictExpires (LOOM-201): the canary's "enforced" served
// describe for as long as the process lived and create never asked, so
// a policy engine removed after a check left egress "none" offered and
// made, unconfined. Describe and create now take it for verdictTTL (10
// minutes); past half of that, describe renews it in the background.
func TestNoneVerdictExpires(t *testing.T) {
	setup := func(t *testing.T) (*fake.Clientset, *atomic.Int32, *Plugin, *time.Time) {
		cs := fake.NewClientset(append(kindPolicies(t), agentsService())...)
		exit := canaryExits(cs, exitBlocked)
		p := configured(t, cs, nil)
		now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
		p.Now = func() time.Time { return now }
		if res, _ := p.Check(ctx); !res.OK || len(res.Problems) != 0 {
			t.Fatalf("check = %+v", res)
		}
		return cs, exit, p, &now
	}
	offered := func(p *Plugin) bool {
		info, err := p.DescribeTargets(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return slices.Contains(info.EgressOptions, protocol.EgressNone)
	}
	t.Run("lapses", func(t *testing.T) {
		cs, exit, p, now := setup(t)
		*now = now.Add(4 * time.Minute)
		if !offered(p) || canaries(cs) != 1 {
			t.Errorf("4 minutes on: none offered %v, canaries %d; want true, the check's 1", offered(p), canaries(cs))
		}
		exit.Store(exitAPIReachable) // the engine is gone
		*now = now.Add(7 * time.Minute)
		if offered(p) {
			t.Error("none offered 11 minutes after the check")
		}
		settle(t, p)
		if offered(p) || canaries(cs) != 2 {
			t.Errorf("once the new canary is done: none offered %v, canaries %d; want false, 2", offered(p), canaries(cs))
		}
		if _, err := p.CreateTarget(ctx, confinedSpec("conf1")); rpcCode(err) != rpc.CodeInvalidParams {
			t.Errorf("create with egress none once not enforced = %v", err)
		}
	})
	t.Run("renewed while in use", func(t *testing.T) {
		cs, _, p, now := setup(t)
		*now = now.Add(6 * time.Minute)
		if !offered(p) {
			t.Error("none withdrawn while its renewal runs")
		}
		settle(t, p)
		*now = now.Add(6 * time.Minute) // 12 minutes after the check, 6 after the renewal
		if !offered(p) || canaries(cs) < 2 {
			t.Errorf("after a renewal: none offered %v, canaries %d", offered(p), canaries(cs))
		}
	})
	t.Run("renewal sees the engine gone", func(t *testing.T) {
		_, exit, p, now := setup(t)
		exit.Store(exitNetReachable)
		*now = now.Add(6 * time.Minute)
		offered(p)
		settle(t, p)
		if offered(p) {
			t.Error("none still offered after a renewal found the engine gone")
		}
	})
	t.Run("describe never waits", func(t *testing.T) {
		cs := fake.NewClientset(append(kindPolicies(t), agentsService())...) // the canary never finishes: no kubelet
		p := configured(t, cs, nil)
		p.CanaryWait = 5 * time.Second
		start := time.Now()
		if offered(p) || time.Since(start) > time.Second {
			t.Errorf("describe with no verdict yet: none offered %v after %v", offered(p), time.Since(start))
		}
	})
	t.Run("create", func(t *testing.T) {
		cs, exit, p, now := setup(t)
		if _, err := p.CreateTarget(ctx, confinedSpec("conf1")); err != nil {
			t.Fatalf("create with egress none, freshly enforced: %v", err)
		}
		*now = now.Add(11 * time.Minute)
		exit.Store(exitNetReachable)
		if _, err := p.CreateTarget(ctx, confinedSpec("conf2")); rpcCode(err) != rpc.CodeInvalidParams || !strings.Contains(err.Error(), "internet") {
			t.Errorf("create with egress none after the TTL, the engine gone = %v", err)
		}
		if n := canaries(cs); n != 2 {
			t.Errorf("canaries = %d, want 2: the check's and the create's", n)
		}
		if _, err := cs.CoreV1().Pods(testNS).Get(ctx, podName("conf2"), metav1.GetOptions{}); err == nil {
			t.Error("a pod labelled egress none was made")
		}
	})
	t.Run("pending", func(t *testing.T) {
		cs := fake.NewClientset(append(kindPolicies(t), agentsService())...) // the canary never finishes: no kubelet
		p := configured(t, cs, nil)
		p.CanaryWait = 20 * time.Millisecond
		if _, err := p.CreateTarget(ctx, confinedSpec("conf1")); rpcCode(err) != rpc.CodeUnavailable {
			t.Errorf("create with egress none, the check still running = %v", err)
		}
	})
}

// The policies are read at every describe, create, start and recreate,
// not once per canary: one added after the check counts at once.
func TestNonePoliciesReadEveryTime(t *testing.T) {
	cs := fake.NewClientset(append(kindPolicies(t), agentsService())...)
	canaryExits(cs, exitBlocked)
	p := configured(t, cs, nil)
	if res, _ := p.Check(ctx); !res.OK || len(res.Problems) != 0 {
		t.Fatalf("check = %+v", res)
	}
	if _, err := p.CreateTarget(ctx, confinedSpec("conf1")); err != nil {
		t.Fatalf("create with egress none: %v", err)
	}
	if err := p.StopTarget(ctx, "conf1"); err != nil {
		t.Fatal(err)
	}
	open := egressPolicy("allow-metadata", map[string]string{labelRole: roleAgent}, cidr("169.254.169.254/32"))
	if _, err := cs.NetworkingV1().NetworkPolicies(testNS).Create(ctx, open, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if info, _ := p.DescribeTargets(ctx); len(info.EgressOptions) != 1 {
		t.Errorf("egress options = %v, want internet only", info.EgressOptions)
	}
	if _, err := p.CreateTarget(ctx, confinedSpec("conf2")); rpcCode(err) != rpc.CodeInvalidParams || !strings.Contains(err.Error(), "allow-metadata") {
		t.Errorf("create with egress none = %v", err)
	}
	if err := p.StartTarget(ctx, "conf1"); rpcCode(err) != rpc.CodeInvalidParams {
		t.Errorf("start of an egress none machine = %v", err)
	}
	if _, err := cs.CoreV1().Pods(testNS).Get(ctx, podName("conf1"), metav1.GetOptions{}); err == nil {
		t.Error("start made the egress none machine's pod")
	}
	if _, err := p.RecreateTarget(ctx, "conf1", confinedSpec("conf1")); rpcCode(err) != rpc.CodeInvalidParams {
		t.Errorf("recreate with egress none = %v", err)
	}
	if _, err := p.CreateTarget(ctx, spec("open1")); err != nil {
		t.Errorf("create with egress internet: %v", err)
	}
}

// A Role without networkpolicies list can't verify "none": the check
// says so (an error when required) and "none" isn't offered.
func TestNonePoliciesForbidden(t *testing.T) {
	cs := fake.NewClientset(append(kindPolicies(t), agentsService())...)
	cs.PrependReactor("list", "networkpolicies", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, kerrors.NewForbidden(schema.GroupResource{Group: "networking.k8s.io", Resource: "networkpolicies"}, "", errors.New("no"))
	})
	canaryExits(cs, exitBlocked)
	p := configured(t, cs, map[string]any{"require_network_policy": true})
	res, _ := p.Check(ctx)
	if msg := failedMessage(res); res.OK || !strings.Contains(msg, "networkpolicies list") {
		t.Errorf("check = %+v", res)
	}
	if info, _ := p.DescribeTargets(ctx); len(info.EgressOptions) != 1 {
		t.Errorf("egress options = %v, want internet only", info.EgressOptions)
	}
}

// A policy added while create waits for the canary counts: the policies
// are read again after the wait, just before the pod is made.
func TestNonePoliciesReadAfterTheCanary(t *testing.T) {
	cs := fake.NewClientset(append(kindPolicies(t), agentsService())...)
	canaryExits(cs, exitBlocked)
	open := egressPolicy("allow-metadata", map[string]string{labelRole: roleAgent}, cidr("169.254.169.254/32"))
	cs.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if pod, ok := action.(k8stesting.CreateAction).GetObject().(*corev1.Pod); ok && pod.Labels[labelCanary] == "true" {
			if err := cs.Tracker().Add(open); err != nil {
				t.Error(err)
			}
		}
		return false, nil, nil
	})
	p := configured(t, cs, nil) // no verdict yet: create runs the canary
	if _, err := p.CreateTarget(ctx, confinedSpec("conf1")); rpcCode(err) != rpc.CodeInvalidParams || !strings.Contains(err.Error(), "allow-metadata") {
		t.Errorf("create with egress none, a policy added during its canary = %v", err)
	}
	if _, err := cs.CoreV1().Pods(testNS).Get(ctx, podName("conf1"), metav1.GetOptions{}); err == nil {
		t.Error("a pod labelled egress none was made")
	}
}
