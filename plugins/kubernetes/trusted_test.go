package kubernetes

import (
	"bytes"
	"log"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/Loomux/server/plugins/protocol"
	"github.com/Loomux/server/plugins/rpc"
)

// trustedSpec is a machine named name asking for egress trusted.
func trustedSpec(id, name string) protocol.EnvironmentSpec {
	sp := spec(id)
	sp.Name, sp.Egress = name, protocol.EgressTrusted
	return sp
}

// syncBuffer is a log the plugin's goroutines may write while a test
// reads it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func reconfigureWith(t *testing.T, p *Plugin, extra map[string]any) {
	t.Helper()
	if err := p.Configure(ctx, protocol.ConfigureParams{Config: baseConfig(t, extra), Host: protocol.HostInfo{Version: "test", InstanceID: testInstance}}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
}

func TestTrustedMachinesConfig(t *testing.T) {
	cfg, err := parseConfig(baseConfig(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.TrustedMachines) != 0 || cfg.trusted("cc") || cfg.trusted("") {
		t.Errorf("by default no machine is trusted: %+v", cfg.TrustedMachines)
	}
	cfg, err = parseConfig(baseConfig(t, map[string]any{"trusted_machines": " cc, command center ,,cc"}))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.TrustedMachines, []string{"cc", "command center"}) || !cfg.trusted("cc") || !cfg.trusted(" command center ") || cfg.trusted("c") {
		t.Errorf("trusted machines = %q", cfg.TrustedMachines)
	}
	for name, v := range map[string]any{"a list": []any{"cc"}, "a newline": "cc\nhub", "a boolean": true} {
		c := baseConfig(t, nil)
		c["trusted_machines"] = v
		if _, err := parseConfig(c); rpcCode(err) != rpc.CodeInvalidConfig {
			t.Errorf("%s: want invalid_config, got %v", name, err)
		}
	}
}

// TestTrustedOnlyForNamedMachines (LOOM-226): egress trusted is offered
// only while trusted_machines names a machine, last, and made only for
// a name it lists; the pod carries the label (e) selects, and the
// plugin's log says so.
func TestTrustedOnlyForNamedMachines(t *testing.T) {
	cs := fake.NewClientset(append(kindPolicies(t), agentsService())...)
	canaryExits(cs, exitBlocked)
	// Set before configure, as a host would: the canary the first
	// describe starts logs from its own goroutine. And each canary its
	// own name: that one may outlive the reconfigure below, and a fixed
	// name made the next configuration's check canary "already exist".
	logs := &syncBuffer{}
	var n atomic.Int64
	p := configured(t, cs, nil, func(p *Plugin) {
		p.Logger = log.New(logs, "", 0)
		p.Rand = func() string { return strconv.FormatInt(n.Add(1), 10) }
	})
	info, err := p.DescribeTargets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(info.EgressOptions, protocol.EgressTrusted) {
		t.Errorf("egress options = %v: trusted offered with no trusted_machines", info.EgressOptions)
	}
	if _, err := p.CreateTarget(ctx, trustedSpec("tr1", "cc")); rpcCode(err) != rpc.CodeInvalidParams {
		t.Errorf("trusted create with no trusted_machines = %v, want invalid_params", err)
	}

	reconfigureWith(t, p, map[string]any{"trusted_machines": "cc"})
	if res, _ := p.Check(ctx); !res.OK || len(res.Problems) != 0 {
		t.Errorf("check = %+v", res)
	}
	info, err = p.DescribeTargets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(info.EgressOptions); n < 2 || info.EgressOptions[0] != protocol.EgressInternet || info.EgressOptions[n-1] != protocol.EgressTrusted {
		t.Errorf("egress options = %v, want internet first and trusted last", info.EgressOptions)
	}

	// Asked for a name (LOOM-216), trusted is offered only to one listed.
	for name, want := range map[string]bool{"cc": true, "builds": false} {
		info, err := p.DescribeTargetsFor(ctx, name)
		if err != nil || slices.Contains(info.EgressOptions, protocol.EgressTrusted) != want || info.EgressOptions[0] != protocol.EgressInternet {
			t.Errorf("describe for %s = %v, %v; want trusted %v", name, info.EgressOptions, err, want)
		}
	}

	if _, err := p.CreateTarget(ctx, trustedSpec("tr2", "builds")); rpcCode(err) != rpc.CodeInvalidParams || !strings.Contains(err.Error(), "trusted_machines") {
		t.Errorf("trusted create for a name not listed = %v, want invalid_params naming trusted_machines", err)
	}
	for _, get := range []func() error{
		func() error {
			_, err := cs.CoreV1().Secrets(testNS).Get(ctx, secretName("tr2"), metav1.GetOptions{})
			return err
		},
		func() error {
			_, err := cs.CoreV1().Pods(testNS).Get(ctx, podName("tr2"), metav1.GetOptions{})
			return err
		},
	} {
		if get() == nil {
			t.Error("a refused trusted create left an object behind")
		}
	}

	env, err := p.CreateTarget(ctx, trustedSpec("tr3", "cc"))
	if err != nil {
		t.Fatalf("trusted create for cc: %v", err)
	}
	if env.Egress != protocol.EgressTrusted {
		t.Errorf("environment egress = %q", env.Egress)
	}
	pod, err := cs.CoreV1().Pods(testNS).Get(ctx, podName("tr3"), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if pod.Labels[labelEgress] != protocol.EgressTrusted || pod.Labels[labelRole] != roleAgent {
		t.Errorf("pod labels = %v", pod.Labels)
	}
	if !strings.Contains(logs.String(), "egress trusted: making pod lx-tr3") {
		t.Errorf("the plugin's log doesn't record the trusted pod: %q", logs.String())
	}
	// No node canary gates it: that is "none"'s alone.
	markRunning(t, cs, podName("tr3"))
	if got, err := p.GetTarget(ctx, "tr3"); err != nil || got.Status != protocol.EnvRunning {
		t.Errorf("get = %+v, %v; want running", got, err)
	}
}

// TestTrustedWithdrawn: a name taken out of trusted_machines turns its
// trusted machine to error and refuses its start and recreate; the pod
// is left as it is. Named again, it runs.
func TestTrustedWithdrawn(t *testing.T) {
	cs := fake.NewClientset(append(kindPolicies(t), agentsService())...)
	canaryExits(cs, exitBlocked)
	p := configured(t, cs, map[string]any{"trusted_machines": "cc"})
	if _, err := p.CreateTarget(ctx, trustedSpec("tr1", "cc")); err != nil {
		t.Fatal(err)
	}
	markRunning(t, cs, podName("tr1"))
	plain, err := p.CreateTarget(ctx, spec("in1"))
	if err != nil {
		t.Fatal(err)
	}
	markRunning(t, cs, podName(plain.ID))
	ids := hostFor(t, p)

	reconfigureWith(t, p, map[string]any{"trusted_machines": "hub"})
	notified(t, ids, "tr1") // at once, not at the host's next reconcile
	got, err := p.GetTarget(ctx, "tr1")
	if err != nil || got.Status != protocol.EnvError || !strings.Contains(got.Reason, "trusted_machines") {
		t.Errorf("get = %+v, %v; want error naming trusted_machines", got, err)
	}
	if h, err := p.TargetHealth(ctx, "tr1"); err != nil || h.Status != protocol.EnvError {
		t.Errorf("health = %+v, %v; want error", h, err)
	}
	list, err := p.ListTargets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range list {
		want := protocol.EnvRunning
		if e.ID == "tr1" {
			want = protocol.EnvError
		}
		if e.Status != want {
			t.Errorf("list: %s = %s, want %s", e.ID, e.Status, want)
		}
	}
	if _, err := cs.CoreV1().Pods(testNS).Get(ctx, podName("tr1"), metav1.GetOptions{}); err != nil {
		t.Errorf("the withdrawn machine's pod should be left: %v", err)
	}
	if err := p.StartTarget(ctx, "tr1"); rpcCode(err) != rpc.CodeInvalidParams {
		t.Errorf("start of the running withdrawn machine = %v, want invalid_params", err)
	}
	if _, err := p.RecreateTarget(ctx, "tr1", trustedSpec("tr1", "cc")); rpcCode(err) != rpc.CodeInvalidParams {
		t.Errorf("recreate of the withdrawn machine = %v, want invalid_params", err)
	}
	if err := p.StopTarget(ctx, "tr1"); err != nil {
		t.Fatal(err)
	}
	if err := p.StartTarget(ctx, "tr1"); rpcCode(err) != rpc.CodeInvalidParams {
		t.Errorf("start of the stopped withdrawn machine = %v, want invalid_params", err)
	}
	if _, err := cs.CoreV1().Pods(testNS).Get(ctx, podName("tr1"), metav1.GetOptions{}); err == nil {
		t.Error("a refused start made the pod")
	}

	reconfigureWith(t, p, map[string]any{"trusted_machines": "hub, cc"})
	notified(t, ids, "tr1")
	if err := p.StartTarget(ctx, "tr1"); err != nil {
		t.Fatalf("start once named again: %v", err)
	}
	markRunning(t, cs, podName("tr1"))
	if got, err := p.GetTarget(ctx, "tr1"); err != nil || got.Status != protocol.EnvRunning {
		t.Errorf("get = %+v, %v; want running", got, err)
	}
	if err := p.DestroyTarget(ctx, "tr1"); err != nil {
		t.Fatal(err)
	}
}

// TestTrustedPolicyLeavesNoneAlone: a policy for trusted machines,
// however wide, selects no "none" machine, so "none" stays offered and
// made; and check says when no policy lets trusted machines out.
func TestTrustedPolicyLeavesNoneAlone(t *testing.T) {
	trusted := map[string]string{labelRole: roleAgent, labelEgress: protocol.EgressTrusted}
	for _, tc := range []struct {
		name    string
		drop    []string
		extra   []runtime.Object
		named   string
		missing bool // check reports trusted_egress_policy_missing
	}{
		{name: "the §10 policies", named: "cc"},
		{name: "a policy letting trusted machines anywhere", named: "cc",
			extra: []runtime.Object{egressPolicy("trusted-anywhere", trusted, nil)}},
		{name: "no policy (e)", drop: []string{"agents-allow-trusted"}, named: "cc", missing: true},
		{name: "no policy (e), nothing named", drop: []string{"agents-allow-trusted"}},
		{name: "a policy (e) for one target only", drop: []string{"agents-allow-trusted"}, named: "cc", missing: true,
			extra: []runtime.Object{egressPolicy("one", map[string]string{labelEgress: protocol.EgressTrusted, protocol.LabelTarget: "t-x"}, cidr("192.168.0.0/16"))}},
		{name: "a policy (e) that only allows DNS", drop: []string{"agents-allow-trusted"}, named: "cc", missing: true,
			extra: []runtime.Object{egressPolicy("dns-only", trusted, kubeDNS(map[string]string{nsNameLabel: "kube-system"}), port(corev1.ProtocolUDP, 53))}},
		{name: "an ingress-only policy (e)", drop: []string{"agents-allow-trusted"}, named: "cc", missing: true,
			extra: []runtime.Object{&networkingv1.NetworkPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: "ingress-only", Namespace: testNS},
				Spec: networkingv1.NetworkPolicySpec{
					PodSelector: metav1.LabelSelector{MatchLabels: trusted},
					PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
					Egress:      []networkingv1.NetworkPolicyEgressRule{{To: cidr("0.0.0.0/0")}},
				},
			}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs := fake.NewClientset(append(append(kindPolicies(t, tc.drop...), agentsService()), tc.extra...)...)
			canaryExits(cs, exitBlocked)
			p := configured(t, cs, map[string]any{"trusted_machines": tc.named})
			res, err := p.Check(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var codes []string
			for _, pr := range res.Problems {
				codes = append(codes, pr.Code)
			}
			if tc.missing {
				if !res.OK || !slices.Equal(codes, []string{ProblemTrustedPolicyMissing}) || res.Problems[0].Severity != protocol.SeverityWarning {
					t.Errorf("check = %+v, want OK with trusted_egress_policy_missing only", res)
				}
			} else if !res.OK || len(codes) != 0 {
				t.Errorf("check = %+v, want no problems", res)
			}
			info, err := p.DescribeTargets(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(info.EgressOptions, protocol.EgressNone) {
				t.Errorf("egress options = %v: none withdrawn", info.EgressOptions)
			}
			if _, err := p.CreateTarget(ctx, confinedSpec("conf1")); err != nil {
				t.Errorf("create with egress none: %v", err)
			}
		})
	}
}

// notified waits for the host to be told of id (targets.changed), and
// of nothing else meanwhile.
func notified(t *testing.T, ids <-chan string, id string) {
	t.Helper()
	select {
	case got := <-ids:
		if got != id {
			t.Errorf("targets.changed for %q, want %q", got, id)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("the host wasn't told of %s", id)
	}
}
