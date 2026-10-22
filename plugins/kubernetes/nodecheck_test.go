package kubernetes

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/Loomux/server/plugins/protocol"
	"github.com/Loomux/server/plugins/rpc"
)

// canaryNodes plays the scheduler and the kubelet for canary pods: one
// the plugin pins to a node (spec.nodeName) runs there, any other lands
// on scheduled. A canary on a node with a code exits with it; on a node
// without one it keeps running until set gives the node a code.
type canaryNodes struct {
	t         *testing.T
	cs        *fake.Clientset
	mu        sync.Mutex
	scheduled string
	codes     map[string]int32
	pinned    []string
}

// failToRun is a code for a canary the kubelet rejects (no room on the
// node): its pod fails without running.
const failToRun = -1

func nodeCanaries(t *testing.T, cs *fake.Clientset, scheduled string, codes map[string]int32) *canaryNodes {
	n := &canaryNodes{t: t, cs: cs, scheduled: scheduled, codes: codes}
	cs.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		pod, ok := action.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		if !ok || pod.Labels[labelCanary] != "true" {
			return false, nil, nil
		}
		// Stored here, under n.mu, so a set never misses a canary that
		// is made but not yet stored.
		n.mu.Lock()
		defer n.mu.Unlock()
		pinned := pod.Spec.NodeName != ""
		if !pinned {
			pod.Spec.NodeName = n.scheduled
		}
		if code, ok := n.codes[pod.Spec.NodeName]; ok {
			exitWith(pod, code)
		}
		pod.Namespace = action.GetNamespace()
		if err := cs.Tracker().Create(corev1.SchemeGroupVersion.WithResource("pods"), pod, pod.Namespace); err != nil {
			return true, nil, err
		}
		if pinned {
			n.pinned = append(n.pinned, pod.Spec.NodeName)
		}
		return true, pod, nil
	})
	return n
}

func exitWith(pod *corev1.Pod, code int32) {
	if code == failToRun {
		pod.Status.Phase, pod.Status.Reason, pod.Status.Message = corev1.PodFailed, "OutOfcpu", "Node didn't have enough resource: cpu"
		return
	}
	pod.Status.Phase = corev1.PodSucceeded
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "netcheck", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: code}}}}
}

// set gives node a code: canaries made on it from now on exit with it,
// and those running there now exit with it at once.
func (n *canaryNodes) set(node string, code int32) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.codes[node] = code
	list, err := n.cs.Tracker().List(corev1.SchemeGroupVersion.WithResource("pods"), corev1.SchemeGroupVersion.WithKind("Pod"), testNS)
	if err != nil {
		n.t.Fatal(err)
	}
	for _, pod := range list.(*corev1.PodList).Items {
		if pod.Labels[labelCanary] == "true" && pod.Spec.NodeName == node && pod.Status.Phase == "" {
			exitWith(&pod, code)
			if err := n.cs.Tracker().Update(corev1.SchemeGroupVersion.WithResource("pods"), &pod, testNS); err != nil {
				n.t.Fatal(err)
			}
		}
	}
}

// pinnedTo is how many canaries the plugin has pinned to node.
func (n *canaryNodes) pinnedTo(node string) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	c := 0
	for _, p := range n.pinned {
		if p == node {
			c++
		}
	}
	return c
}

// pinnedSoon waits up to 5 s for want canaries pinned to node: the
// plugin makes them on its own goroutines.
func (n *canaryNodes) pinnedSoon(node string, want int) bool {
	deadline := time.Now().Add(5 * time.Second)
	for n.pinnedTo(node) != want {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
	return true
}

// testClock is a settable clock safe to read from the plugin's
// goroutines.
type testClock struct{ ns atomic.Int64 }

func newClock(p *Plugin) *testClock {
	c := &testClock{}
	c.ns.Store(time.Date(2026, 10, 22, 1, 0, 0, 0, time.UTC).UnixNano())
	p.Now = func() time.Time { return time.Unix(0, c.ns.Load()).UTC() }
	return c
}

func (c *testClock) advance(d time.Duration) { c.ns.Add(int64(d)) }

// scheduleOn binds a machine's pod to node and marks it running, as the
// scheduler and the kubelet would; node "" leaves it unbound.
func scheduleOn(t *testing.T, cs *fake.Clientset, id, node string) {
	t.Helper()
	pod, err := cs.CoreV1().Pods(testNS).Get(ctx, podName(id), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pod.Spec.NodeName = node
	if err := cs.Tracker().Update(corev1.SchemeGroupVersion.WithResource("pods"), pod, testNS); err != nil {
		t.Fatal(err)
	}
	markRunning(t, cs, podName(id))
}

// statusSoon polls get until the machine is want, within 5 s.
func statusSoon(t *testing.T, p *Plugin, id, want string) protocol.Environment {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		env, err := p.GetTarget(ctx, id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if env.Status == want {
			return env
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s is %s (%s), never %s", id, env.Status, env.Reason, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// nodeSetup is a checked plugin over the kind manifests' policies whose
// canaries run on the given nodes; the check's own lands on scheduled.
func nodeSetup(t *testing.T, scheduled string, codes map[string]int32) (*fake.Clientset, *canaryNodes, *Plugin, *testClock) {
	t.Helper()
	cs := fake.NewClientset(append(kindPolicies(t), agentsService())...)
	nodes := nodeCanaries(t, cs, scheduled, codes)
	p := configured(t, cs, nil)
	var n atomic.Int64
	p.Rand = func() string { return strconv.FormatInt(n.Add(1), 10) }
	clock := newClock(p)
	if res, _ := p.Check(ctx); !res.OK || len(res.Problems) != 0 {
		t.Fatalf("check = %+v", res)
	}
	return cs, nodes, p, clock
}

func noneOffered(t *testing.T, p *Plugin) bool {
	t.Helper()
	info, err := p.DescribeTargets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return slices.Contains(info.EgressOptions, protocol.EgressNone)
}

// TestNoneRunsOnlyOnceItsNodeIsChecked (LOOM-208): the canary that
// certifies egress "none" runs on whichever node the scheduler picks, and
// a policy engine missing from or broken on another node left machines
// there unconfined, reported running. A "none" machine is running only
// once a canary pinned to its own node (spec.nodeName) says "enforced";
// one that says otherwise makes the machine an error and withdraws
// "none" until a check finds the node enforced.
func TestNoneRunsOnlyOnceItsNodeIsChecked(t *testing.T) {
	t.Run("confirmed on its node", func(t *testing.T) {
		cs, nodes, p, _ := nodeSetup(t, "n0", map[string]int32{"n0": exitBlocked})
		if _, err := p.CreateTarget(ctx, confinedSpec("conf1")); err != nil {
			t.Fatal(err)
		}
		scheduleOn(t, cs, "conf1", "n1")
		env, err := p.GetTarget(ctx, "conf1")
		if err != nil || env.Status != protocol.EnvStarting || !strings.Contains(env.Reason, "n1") {
			t.Fatalf("ready on an unchecked node: %s (%s), %v; want starting, naming n1", env.Status, env.Reason, err)
		}
		if h, err := p.TargetHealth(ctx, "conf1"); err != nil || h.Status != protocol.EnvStarting {
			t.Errorf("health on an unchecked node = %+v, %v", h, err)
		}
		if list, err := p.ListTargets(ctx); err != nil || len(list) != 1 || list[0].Status != protocol.EnvStarting {
			t.Errorf("list on an unchecked node = %+v, %v", list, err)
		}
		if !nodes.pinnedSoon("n1", 1) {
			t.Fatalf("canaries pinned to n1 = %d, want 1", nodes.pinnedTo("n1"))
		}
		nodes.set("n1", exitBlocked)
		statusSoon(t, p, "conf1", protocol.EnvRunning)
		if h, err := p.TargetHealth(ctx, "conf1"); err != nil || h.Status != protocol.EnvRunning {
			t.Errorf("health once n1 is checked = %+v, %v", h, err)
		}
		if nodes.pinnedTo("n1") != 1 {
			t.Errorf("canaries pinned to n1 = %d after the machine is confirmed, want still 1", nodes.pinnedTo("n1"))
		}
		if !noneOffered(t, p) {
			t.Error("none withdrawn though every node checked is enforced")
		}
	})
	t.Run("not enforced on its node", func(t *testing.T) {
		cs, nodes, p, clock := nodeSetup(t, "n0", map[string]int32{"n0": exitBlocked, "n1": exitNetReachable})
		if _, err := p.CreateTarget(ctx, confinedSpec("conf1")); err != nil {
			t.Fatal(err)
		}
		scheduleOn(t, cs, "conf1", "n1")
		env := statusSoon(t, p, "conf1", protocol.EnvError)
		if !strings.Contains(env.Reason, "n1") || !strings.Contains(env.Reason, "internet") {
			t.Errorf("reason = %q, want n1 and what it reached", env.Reason)
		}
		if noneOffered(t, p) {
			t.Error("none still offered after n1 let a confined pod out")
		}
		if _, err := p.CreateTarget(ctx, confinedSpec("conf2")); rpcCode(err) != rpc.CodeInvalidParams || !strings.Contains(err.Error(), "n1") {
			t.Errorf("create with egress none = %v, want invalid_params naming n1", err)
		}
		res, _ := p.Check(ctx)
		if !hasCode(res, ProblemNetworkPolicyNotEnforced) || !strings.Contains(problemText(res), "n1") {
			t.Errorf("check = %+v, want network_policy_not_enforced naming n1", res)
		}
		// The engine is mended on n1. Until a check, n1 stays open:
		// neither the renewer nor a get tries it again.
		nodes.set("n1", exitBlocked)
		clock.advance(2 * time.Minute)
		time.Sleep(50 * time.Millisecond) // renewer rounds
		if env, _ := p.GetTarget(ctx, "conf1"); env.Status != protocol.EnvError || nodes.pinnedTo("n1") != 1 || noneOffered(t, p) {
			t.Errorf("before a check: conf1 %s, canaries pinned to n1 %d, none offered %v; want error, 1, false", env.Status, nodes.pinnedTo("n1"), noneOffered(t, p))
		}
		// A check a minute later tries n1 again, and none is offered
		// again.
		if res, _ := p.Check(ctx); !res.OK || len(res.Problems) != 0 {
			t.Errorf("check after n1 is mended = %+v", res)
		}
		if nodes.pinnedTo("n1") < 2 {
			t.Errorf("canaries pinned to n1 = %d, want the check's own again", nodes.pinnedTo("n1"))
		}
		if !noneOffered(t, p) {
			t.Error("none not offered after the check found n1 enforced")
		}
	})
	t.Run("a node each", func(t *testing.T) {
		cs, _, p, _ := nodeSetup(t, "n0", map[string]int32{"n0": exitBlocked, "n1": exitBlocked, "n2": exitLinkLocalReach})
		for _, id := range []string{"conf1", "conf2"} {
			if _, err := p.CreateTarget(ctx, confinedSpec(id)); err != nil {
				t.Fatal(err)
			}
		}
		scheduleOn(t, cs, "conf1", "n1")
		scheduleOn(t, cs, "conf2", "n2")
		if _, err := p.ListTargets(ctx); err != nil {
			t.Fatal(err)
		}
		statusSoon(t, p, "conf1", protocol.EnvRunning)
		if env := statusSoon(t, p, "conf2", protocol.EnvError); !strings.Contains(env.Reason, "n2") || !strings.Contains(env.Reason, metadataAddr) {
			t.Errorf("conf2's reason = %q, want n2 and the metadata address", env.Reason)
		}
	})
	t.Run("the check's own node", func(t *testing.T) {
		cs, nodes, p, _ := nodeSetup(t, "n1", map[string]int32{"n1": exitBlocked})
		if _, err := p.CreateTarget(ctx, confinedSpec("conf1")); err != nil {
			t.Fatal(err)
		}
		scheduleOn(t, cs, "conf1", "n1")
		if env, err := p.GetTarget(ctx, "conf1"); err != nil || env.Status != protocol.EnvRunning {
			t.Errorf("on the node the check's canary ran on: %+v, %v; want running", env, err)
		}
		if nodes.pinnedTo("n1") != 0 {
			t.Errorf("canaries pinned to n1 = %d, want 0: the check's ran there", nodes.pinnedTo("n1"))
		}
	})
	t.Run("internet machines", func(t *testing.T) {
		cs, nodes, p, _ := nodeSetup(t, "n0", map[string]int32{"n0": exitBlocked})
		if _, err := p.CreateTarget(ctx, spec("open1")); err != nil {
			t.Fatal(err)
		}
		scheduleOn(t, cs, "open1", "n1")
		if env, err := p.GetTarget(ctx, "open1"); err != nil || env.Status != protocol.EnvRunning {
			t.Errorf("egress internet machine: %+v, %v; want running", env, err)
		}
		if nodes.pinnedTo("n1") != 0 {
			t.Error("a canary was pinned for an egress internet machine")
		}
	})
	t.Run("no node", func(t *testing.T) {
		cs, _, p, _ := nodeSetup(t, "n0", map[string]int32{"n0": exitBlocked})
		if _, err := p.CreateTarget(ctx, confinedSpec("conf1")); err != nil {
			t.Fatal(err)
		}
		scheduleOn(t, cs, "conf1", "")
		if env, err := p.GetTarget(ctx, "conf1"); err != nil || env.Status != protocol.EnvStarting {
			t.Errorf("ready with no node: %+v, %v; want starting", env, err)
		}
	})
	t.Run("a canary that can't run is tried again", func(t *testing.T) {
		cs, nodes, p, clock := nodeSetup(t, "n0", map[string]int32{"n0": exitBlocked, "n1": failToRun})
		if _, err := p.CreateTarget(ctx, confinedSpec("conf1")); err != nil {
			t.Fatal(err)
		}
		scheduleOn(t, cs, "conf1", "n1")
		deadline := time.Now().Add(5 * time.Second)
		for {
			env, _ := p.GetTarget(ctx, "conf1")
			if env.Status != protocol.EnvStarting {
				t.Fatalf("while n1 can't run a canary: %s (%s), want starting", env.Status, env.Reason)
			}
			if strings.Contains(env.Reason, "OutOfcpu") {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("reason = %q, never why n1 couldn't be checked", env.Reason)
			}
			time.Sleep(10 * time.Millisecond)
		}
		if !noneOffered(t, p) {
			t.Error("none withdrawn because a canary couldn't run")
		}
		nodes.set("n1", exitBlocked)
		clock.advance(2 * time.Minute)
		statusSoon(t, p, "conf1", protocol.EnvRunning)
		if nodes.pinnedTo("n1") != 2 {
			t.Errorf("canaries pinned to n1 = %d, want 2", nodes.pinnedTo("n1"))
		}
	})
}

// TestNoneNodeRenewed: a machine's node is checked again in the
// background past half of verdictTTL, whether or not the host asks
// about it, so a machine in use keeps running; one whose node has no
// "enforced" from the last verdictTTL is starting, and a renewal that
// finds the node no longer enforcing makes it an error.
func TestNoneNodeRenewed(t *testing.T) {
	t.Run("in the background", func(t *testing.T) {
		cs, nodes, p, clock := nodeSetup(t, "n0", map[string]int32{"n0": exitBlocked, "n1": exitBlocked})
		if _, err := p.CreateTarget(ctx, confinedSpec("conf1")); err != nil {
			t.Fatal(err)
		}
		scheduleOn(t, cs, "conf1", "n1")
		statusSoon(t, p, "conf1", protocol.EnvRunning)
		clock.advance(6 * time.Minute) // nobody asks about conf1
		if !nodes.pinnedSoon("n1", 2) {
			t.Fatalf("canaries pinned to n1 = %d 6 minutes on, nobody asking; want 2: the renewal", nodes.pinnedTo("n1"))
		}
		clock.advance(6 * time.Minute)
		if env, _ := p.GetTarget(ctx, "conf1"); env.Status != protocol.EnvRunning {
			t.Errorf("12 minutes on, renewed at 6: %s (%s), want running", env.Status, env.Reason)
		}
	})
	t.Run("stuck", func(t *testing.T) {
		cs, nodes, p, clock := nodeSetup(t, "n0", map[string]int32{"n0": exitBlocked, "n1": exitBlocked})
		p.CanaryTimeout = 10 * time.Second
		if _, err := p.CreateTarget(ctx, confinedSpec("conf1")); err != nil {
			t.Fatal(err)
		}
		scheduleOn(t, cs, "conf1", "n1")
		statusSoon(t, p, "conf1", protocol.EnvRunning)
		nodes.mu.Lock()
		delete(nodes.codes, "n1") // the renewal won't finish
		nodes.mu.Unlock()
		clock.advance(6 * time.Minute)
		if !nodes.pinnedSoon("n1", 2) {
			t.Fatalf("canaries pinned to n1 = %d, want 2: the renewal", nodes.pinnedTo("n1"))
		}
		if env, _ := p.GetTarget(ctx, "conf1"); env.Status != protocol.EnvRunning {
			t.Errorf("6 minutes on, the renewal running: %s (%s), want running", env.Status, env.Reason)
		}
		clock.advance(6 * time.Minute)
		if env, _ := p.GetTarget(ctx, "conf1"); env.Status != protocol.EnvStarting || !strings.Contains(env.Reason, "n1") {
			t.Errorf("12 minutes on, no enforced since: %s (%s), want starting, naming n1", env.Status, env.Reason)
		}
		nodes.set("n1", exitAPIReachable)
		if env := statusSoon(t, p, "conf1", protocol.EnvError); !strings.Contains(env.Reason, "n1") {
			t.Errorf("reason = %q, want n1", env.Reason)
		}
		if noneOffered(t, p) {
			t.Error("none still offered after the renewal found n1 open")
		}
	})
	t.Run("a renewal that can't run", func(t *testing.T) {
		cs, nodes, p, clock := nodeSetup(t, "n0", map[string]int32{"n0": exitBlocked, "n1": exitBlocked})
		if _, err := p.CreateTarget(ctx, confinedSpec("conf1")); err != nil {
			t.Fatal(err)
		}
		scheduleOn(t, cs, "conf1", "n1")
		statusSoon(t, p, "conf1", protocol.EnvRunning)
		nodes.set("n1", failToRun)
		clock.advance(6 * time.Minute)
		if !nodes.pinnedSoon("n1", 2) {
			t.Fatalf("canaries pinned to n1 = %d, want 2: the renewal", nodes.pinnedTo("n1"))
		}
		time.Sleep(50 * time.Millisecond) // it fails at once
		if env, _ := p.GetTarget(ctx, "conf1"); env.Status != protocol.EnvRunning {
			t.Errorf("6 minutes on, the renewal couldn't run: %s (%s), want running on the verdict of 6 minutes ago", env.Status, env.Reason)
		}
		clock.advance(5 * time.Minute)
		if env, _ := p.GetTarget(ctx, "conf1"); env.Status != protocol.EnvStarting {
			t.Errorf("11 minutes on, no enforced since: %s (%s), want starting", env.Status, env.Reason)
		}
	})
}

// TestNoneNodeVerdictLapses: a node's "enforced" lets a machine run
// within verdictTTL only, a machine new on that node or one already
// there.
func TestNoneNodeVerdictLapses(t *testing.T) {
	cs, nodes, p, clock := nodeSetup(t, "n0", map[string]int32{"n0": exitBlocked, "n1": exitBlocked})
	p.CanaryTimeout = 10 * time.Second
	if _, err := p.CreateTarget(ctx, confinedSpec("conf1")); err != nil {
		t.Fatal(err)
	}
	scheduleOn(t, cs, "conf1", "n1")
	statusSoon(t, p, "conf1", protocol.EnvRunning)
	nodes.mu.Lock()
	delete(nodes.codes, "n1") // the renewal won't finish
	nodes.mu.Unlock()
	clock.advance(11 * time.Minute)
	if _, err := p.CreateTarget(ctx, confinedSpec("conf2")); err != nil {
		t.Fatal(err)
	}
	scheduleOn(t, cs, "conf2", "n1")
	for _, id := range []string{"conf1", "conf2"} {
		if env, _ := p.GetTarget(ctx, id); env.Status != protocol.EnvStarting {
			t.Errorf("%s on n1, its verdict 11 minutes old: %s (%s), want starting", id, env.Status, env.Reason)
		}
	}
	nodes.set("n1", exitBlocked)
	statusSoon(t, p, "conf1", protocol.EnvRunning)
	statusSoon(t, p, "conf2", protocol.EnvRunning)
}

func hasCode(res protocol.CheckResult, code string) bool {
	for _, pr := range res.Problems {
		if pr.Code == code {
			return true
		}
	}
	return false
}

func problemText(res protocol.CheckResult) string {
	var b strings.Builder
	for _, pr := range res.Problems {
		b.WriteString(pr.Code + ": " + pr.Message + "\n")
	}
	return b.String()
}

// TestNoneNodeFailureSticks: a node found open stays so until a canary
// there says "enforced"; a retry that can't run proves nothing and
// mustn't clear it, nor let an older "enforced" run the machine again
// (LOOM-208 review).
func TestNoneNodeFailureSticks(t *testing.T) {
	cs, nodes, p, clock := nodeSetup(t, "n0", map[string]int32{"n0": exitBlocked, "n1": exitBlocked})
	if _, err := p.CreateTarget(ctx, confinedSpec("conf1")); err != nil {
		t.Fatal(err)
	}
	scheduleOn(t, cs, "conf1", "n1")
	statusSoon(t, p, "conf1", protocol.EnvRunning)
	nodes.set("n1", exitNetReachable)
	clock.advance(6 * time.Minute) // the renewal finds n1 open
	statusSoon(t, p, "conf1", protocol.EnvError)
	nodes.set("n1", failToRun)
	clock.advance(2 * time.Minute) // the check's retry can't run
	res, _ := p.Check(ctx)
	if nodes.pinnedTo("n1") != 3 {
		t.Fatalf("canaries pinned to n1 = %d, want 3: the first, the renewal, the check's retry", nodes.pinnedTo("n1"))
	}
	if !hasCode(res, ProblemNetworkPolicyNotEnforced) || !strings.Contains(problemText(res), "n1") {
		t.Errorf("check after a retry that couldn't run = %+v, want n1 still not enforced", res)
	}
	if env, _ := p.GetTarget(ctx, "conf1"); env.Status != protocol.EnvError {
		t.Errorf("conf1 after a retry that couldn't run: %s (%s), want error", env.Status, env.Reason)
	}
	if noneOffered(t, p) {
		t.Error("none offered again after a retry that couldn't run")
	}
	if _, err := p.CreateTarget(ctx, confinedSpec("conf2")); rpcCode(err) != rpc.CodeInvalidParams || !strings.Contains(err.Error(), "n1") {
		t.Errorf("create with egress none = %v, want invalid_params naming n1", err)
	}
}

// TestNoneStoppedNotRenewed: a stopped machine's node isn't checked for
// it any more, nor one whose pod is gone, nor one recreated with egress
// internet (LOOM-208 review).
func TestNoneStoppedNotRenewed(t *testing.T) {
	for _, how := range []string{"stop", "pod gone, health asked", "recreated with egress internet"} {
		t.Run(how, func(t *testing.T) {
			cs, nodes, p, clock := nodeSetup(t, "n0", map[string]int32{"n0": exitBlocked, "n1": exitBlocked})
			if _, err := p.CreateTarget(ctx, confinedSpec("conf1")); err != nil {
				t.Fatal(err)
			}
			scheduleOn(t, cs, "conf1", "n1")
			statusSoon(t, p, "conf1", protocol.EnvRunning)
			switch how {
			case "stop":
				if err := p.StopTarget(ctx, "conf1"); err != nil {
					t.Fatal(err)
				}
			case "recreated with egress internet":
				if _, err := p.RecreateTarget(ctx, "conf1", spec("conf1")); err != nil {
					t.Fatal(err)
				}
				scheduleOn(t, cs, "conf1", "n1")
				statusSoon(t, p, "conf1", protocol.EnvRunning)
			default:
				if err := cs.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("pods"), testNS, podName("conf1")); err != nil {
					t.Fatal(err)
				}
				if h, err := p.TargetHealth(ctx, "conf1"); err != nil || h.Status != protocol.EnvStopped {
					t.Fatalf("health = %+v, %v", h, err)
				}
			}
			clock.advance(6 * time.Minute)
			time.Sleep(100 * time.Millisecond) // many renewer rounds
			if n := nodes.pinnedTo("n1"); n != 1 {
				t.Errorf("canaries pinned to n1 = %d after the machine went, want still 1", n)
			}
		})
	}
}

// TestNoneOpenNodeOutranksTheCheck: check reports a node known open,
// naming it, whatever its own canary says: still running (the image
// pulling), unable to run, or out somewhere else; and its own canary out
// names the node it ran on (LOOM-208 review).
func TestNoneOpenNodeOutranksTheCheck(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pending bool
		n0      int32
	}{
		{name: "the check's canary still running", pending: true},
		{name: "the check's canary can't run", n0: failToRun},
		{name: "the check's canary out too", n0: exitLinkLocalReach},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs, nodes, p, clock := nodeSetup(t, "n0", map[string]int32{"n0": exitBlocked, "n1": exitNetReachable})
			if _, err := p.CreateTarget(ctx, confinedSpec("conf1")); err != nil {
				t.Fatal(err)
			}
			scheduleOn(t, cs, "conf1", "n1")
			statusSoon(t, p, "conf1", protocol.EnvError)
			nodes.mu.Lock()
			if tc.pending {
				delete(nodes.codes, "n0")
			} else {
				nodes.codes["n0"] = tc.n0
			}
			nodes.mu.Unlock()
			p.CanaryWait = 50 * time.Millisecond
			clock.advance(2 * time.Minute) // the check runs its canary again
			res, _ := p.Check(ctx)
			if !hasCode(res, ProblemNetworkPolicyNotEnforced) || !strings.Contains(problemText(res), "n1") {
				t.Errorf("check = %s; want network_policy_not_enforced naming n1", problemText(res))
			}
		})
	}
	t.Run("the check's canary out on its node", func(t *testing.T) {
		_, nodes, p, clock := nodeSetup(t, "n0", map[string]int32{"n0": exitBlocked})
		nodes.set("n0", exitLinkLocalReach)
		clock.advance(2 * time.Minute)
		res, _ := p.Check(ctx)
		if !hasCode(res, ProblemNetworkPolicyNotEnforced) || !strings.Contains(problemText(res), "n0") {
			t.Errorf("check = %s; want network_policy_not_enforced naming n0", problemText(res))
		}
	})
}

// TestNoneOpenNodeOutlivesAPolicyError: a check whose policy read
// fails, or finds the policies open, still reports a node known open,
// besides that (LOOM-208 review).
func TestNoneOpenNodeOutlivesAPolicyError(t *testing.T) {
	for _, tc := range []struct {
		name string
		list func() (runtime.Object, error)
		code string
	}{
		{"forbidden", func() (runtime.Object, error) {
			return nil, kerrors.NewForbidden(schema.GroupResource{Group: "networking.k8s.io", Resource: "networkpolicies"}, "", errors.New("no"))
		}, ProblemNetworkCheckFailed},
		{"transient", func() (runtime.Object, error) {
			return nil, kerrors.NewServerTimeout(schema.GroupResource{Group: "networking.k8s.io", Resource: "networkpolicies"}, "list", 1)
		}, ProblemNetworkCheckFailed},
		{"policies open", func() (runtime.Object, error) {
			return &networkingv1.NetworkPolicyList{}, nil
		}, ProblemNetworkPolicyAllowsEgress},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs, _, p, _ := nodeSetup(t, "n0", map[string]int32{"n0": exitBlocked, "n1": exitNetReachable})
			if _, err := p.CreateTarget(ctx, confinedSpec("conf1")); err != nil {
				t.Fatal(err)
			}
			scheduleOn(t, cs, "conf1", "n1")
			statusSoon(t, p, "conf1", protocol.EnvError)
			cs.PrependReactor("list", "networkpolicies", func(k8stesting.Action) (bool, runtime.Object, error) {
				obj, err := tc.list()
				return true, obj, err
			})
			res, _ := p.Check(ctx)
			if !hasCode(res, tc.code) || !hasCode(res, ProblemNetworkPolicyNotEnforced) || !strings.Contains(problemText(res), "n1") {
				t.Errorf("check = %s; want %s and n1 not enforced", problemText(res), tc.code)
			}
		})
	}
}
