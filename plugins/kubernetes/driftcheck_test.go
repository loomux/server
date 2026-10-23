package kubernetes

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/Loomux/server/plugins/protocol"
	"github.com/Loomux/server/plugins/rpc"
)

// LOOM-211: a running egress "none" machine stays checked after it
// starts. Its node is renewed by LOOM-208's renewer; these cover the
// NetworkPolicies drifting under it, and the re-check a change in them
// brings forward.

// reasonSoon polls get until the machine is want with a reason holding
// part, within 5 s.
func reasonSoon(t *testing.T, p *Plugin, id, want, part string) protocol.Environment {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		env, err := p.GetTarget(ctx, id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if env.Status == want && strings.Contains(env.Reason, part) {
			return env
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s is %s (%s), never %s with %q", id, env.Status, env.Reason, want, part)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// touchPolicies changes the policies without opening them: one more
// egress port for egress "internet" machines, a policy no "none" one is
// selected by.
func touchPolicies(t *testing.T, cs *fake.Clientset, port int32) {
	t.Helper()
	nps := cs.NetworkingV1().NetworkPolicies(testNS)
	np, err := nps.Get(ctx, "agents-allow-internet", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	np.Spec.Egress = append(np.Spec.Egress, networkingv1.NetworkPolicyEgressRule{Ports: []networkingv1.NetworkPolicyPort{{Port: ptr(intstr.FromInt32(port))}}})
	if _, err := nps.Update(ctx, np, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

// runningOn is a checked plugin with a "none" machine, conf1, running on
// n1, whose canaries say "enforced" (as n0's, the check's, do).
func runningOn(t *testing.T, setup ...func(*Plugin)) (*fake.Clientset, *canaryNodes, *Plugin, *testClock) {
	t.Helper()
	cs, nodes, p, clock := nodeSetup(t, "n0", map[string]int32{"n0": exitBlocked, "n1": exitBlocked}, setup...)
	if _, err := p.CreateTarget(ctx, confinedSpec("conf1")); err != nil {
		t.Fatal(err)
	}
	scheduleOn(t, cs, "conf1", "n1")
	statusSoon(t, p, "conf1", protocol.EnvRunning)
	return cs, nodes, p, clock
}

// The policies isolating a running machine for egress removed (the
// default deny, and the DNS one, which isolates it too): the next
// re-read makes it an error, withdraws "none" and fails the check;
// restored, the change re-checks its node and it runs again.
func TestNonePolicyRemovedWithdraws(t *testing.T) {
	cs, nodes, p, clock := runningOn(t)
	nps := cs.NetworkingV1().NetworkPolicies(testNS)
	var removed []*networkingv1.NetworkPolicy
	for _, name := range []string{"agents-default-deny", "agents-allow-dns"} {
		np, err := nps.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if err := nps.Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}
		removed = append(removed, np)
	}
	clock.advance(time.Minute + time.Second) // the renewer's next read
	reasonSoon(t, p, "conf1", protocol.EnvError, "NetworkPolicies no longer confine")
	if noneOffered(t, p) {
		t.Error("none still offered with the deny policy gone")
	}
	if _, err := p.CreateTarget(ctx, confinedSpec("conf2")); rpcCode(err) != rpc.CodeInvalidParams {
		t.Errorf("create with egress none = %v, want invalid_params", err)
	}
	if res, _ := p.Check(ctx); !hasCode(res, ProblemNetworkPolicyAllowsEgress) {
		t.Errorf("check = %+v, want %s", res, ProblemNetworkPolicyAllowsEgress)
	}
	// The change (to open) already re-checked n1: wait for it to settle.
	if !nodes.pinnedSoon("n1", 2) {
		t.Fatalf("canaries pinned to n1 = %d, want 2: the first, the change's", nodes.pinnedTo("n1"))
	}
	for _, np := range removed {
		np.ResourceVersion = ""
		if _, err := nps.Create(ctx, np, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	clock.advance(time.Minute + time.Second)
	if !nodes.pinnedSoon("n1", 3) {
		t.Fatalf("canaries pinned to n1 = %d after the policy came back, want 3: it re-checks the node", nodes.pinnedTo("n1"))
	}
	statusSoon(t, p, "conf1", protocol.EnvRunning)
	if !noneOffered(t, p) {
		t.Error("none not offered again with the policies restored")
	}
}

// A change in the policies re-checks the node at once, not at the
// renewal 5 minutes on: an engine broken there meanwhile is found
// within the minute.
func TestNonePolicyChangeRechecksNode(t *testing.T) {
	cs, nodes, p, clock := runningOn(t)
	nodes.set("n1", exitNetReachable)
	touchPolicies(t, cs, 8443)
	clock.advance(time.Minute + time.Second)
	reasonSoon(t, p, "conf1", protocol.EnvError, "n1")
	if got := nodes.pinnedTo("n1"); got != 2 {
		t.Errorf("canaries pinned to n1 = %d, want 2: the first, the change's", got)
	}
}

// A re-check whose canary can't run proves nothing: it is tried again a
// minute on, the machine runs on the earlier "enforced" only within
// verdictTTL, then is starting, never passed.
func TestNoneRecheckThatCantRunIsNoPass(t *testing.T) {
	cs, nodes, p, clock := runningOn(t)
	nodes.set("n1", failToRun)
	touchPolicies(t, cs, 8443)
	clock.advance(time.Minute + time.Second)
	if !nodes.pinnedSoon("n1", 2) {
		t.Fatalf("canaries pinned to n1 = %d, want 2: the change's re-check", nodes.pinnedTo("n1"))
	}
	time.Sleep(50 * time.Millisecond) // it fails at once
	if env, _ := p.GetTarget(ctx, "conf1"); env.Status != protocol.EnvRunning {
		t.Errorf("a minute on, the re-check couldn't run: %s (%s), want running on the verdict of a minute ago", env.Status, env.Reason)
	}
	clock.advance(time.Minute + time.Second)
	if !nodes.pinnedSoon("n1", 3) {
		t.Fatalf("canaries pinned to n1 = %d, want 3: the re-check tried again, still pending", nodes.pinnedTo("n1"))
	}
	clock.advance(9 * time.Minute) // past verdictTTL since the last "enforced"
	reasonSoon(t, p, "conf1", protocol.EnvStarting, "n1")
	nodes.set("n1", exitBlocked)
	clock.advance(time.Minute + time.Second)
	statusSoon(t, p, "conf1", protocol.EnvRunning)
}

// Reads of the policies that fail prove nothing: the last that found
// them confining stands for verdictTTL, then a running machine is
// starting, and runs again once one succeeds.
func TestNonePolicyReadFailureIsNoPass(t *testing.T) {
	var failing atomic.Bool
	cs, _, p, clock := runningOn(t)
	settleCanaries(t, p) // before changing the fake's reactors
	cs.PrependReactor("list", "networkpolicies", func(k8stesting.Action) (bool, runtime.Object, error) {
		if failing.Load() {
			return true, nil, errors.New("etcdserver: request timed out")
		}
		return false, nil, nil
	})
	failing.Store(true)
	clock.advance(time.Minute + time.Second)
	time.Sleep(50 * time.Millisecond) // the renewer's read fails
	if env, _ := p.GetTarget(ctx, "conf1"); env.Status != protocol.EnvRunning {
		t.Errorf("a read failed a minute on: %s (%s), want running on the read before", env.Status, env.Reason)
	}
	clock.advance(10 * time.Minute)
	reasonSoon(t, p, "conf1", protocol.EnvStarting, "NetworkPolicies")
	failing.Store(false)
	clock.advance(time.Minute + time.Second)
	statusSoon(t, p, "conf1", protocol.EnvRunning)
}

// No storm: the policies are read once a minute, a node has one canary
// at a time however often they change, and a re-check waits a minute
// after the node's last.
func TestNoneRecheckNoStorm(t *testing.T) {
	var reads atomic.Int32
	cs, nodes, p, clock := runningOn(t, func(p *Plugin) { p.CanaryTimeout = time.Minute })
	settleCanaries(t, p) // before changing the fake's reactors
	cs.PrependReactor("list", "networkpolicies", func(k8stesting.Action) (bool, runtime.Object, error) {
		reads.Add(1)
		return false, nil, nil
	})
	if _, err := p.CreateTarget(ctx, confinedSpec("conf2")); err != nil {
		t.Fatal(err)
	}
	scheduleOn(t, cs, "conf2", "n1") // a second machine on the same node
	statusSoon(t, p, "conf2", protocol.EnvRunning)
	nodes.mu.Lock()
	delete(nodes.codes, "n1") // the re-check runs on
	nodes.mu.Unlock()
	reads.Store(0)

	touchPolicies(t, cs, 1)
	clock.advance(time.Minute + time.Second)
	if !nodes.pinnedSoon("n1", 2) {
		t.Fatalf("canaries pinned to n1 = %d, want 2: one re-check for both machines", nodes.pinnedTo("n1"))
	}
	for i := int32(2); i <= 4; i++ {
		touchPolicies(t, cs, i)
		clock.advance(time.Minute + time.Second)
		time.Sleep(30 * time.Millisecond)
		if got := nodes.pinnedTo("n1"); got != 2 {
			t.Fatalf("change %d with the re-check still running: %d canaries pinned to n1, want 2", i, got)
		}
	}
	if got := reads.Load(); got > 4 {
		t.Errorf("policy reads over 4 minutes = %d, want one a minute", got)
	}
	nodes.set("n1", exitBlocked) // the running one ends, begun before the later changes
	deadline := time.Now().Add(5 * time.Second)
	for {
		p.mu.Lock()
		running := p.nodes["n1"].running
		p.mu.Unlock()
		if running == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the re-check never ended")
		}
		time.Sleep(5 * time.Millisecond)
	}
	clock.advance(30 * time.Second)
	time.Sleep(30 * time.Millisecond)
	if got := nodes.pinnedTo("n1"); got != 2 {
		t.Fatalf("within a minute of the last: %d canaries pinned to n1, want 2", got)
	}
	clock.advance(31 * time.Second)
	if !nodes.pinnedSoon("n1", 3) {
		t.Fatalf("canaries pinned to n1 = %d, want 3: the later changes still re-check n1", nodes.pinnedTo("n1"))
	}
	clock.advance(time.Minute + time.Second)
	clock.advance(time.Minute + time.Second)
	time.Sleep(30 * time.Millisecond)
	if got := nodes.pinnedTo("n1"); got != 3 {
		t.Errorf("no change since: %d canaries pinned to n1, want 3", got)
	}
}
