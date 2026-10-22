package kubernetes

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/Loomux/server/plugins/protocol"
	"github.com/Loomux/server/plugins/rpc"
)

// leakOn records what a canary that got out on node would: as if one
// finished while a create, start or recreate was under way.
func leakOn(p *Plugin, node string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.nodeOf(node).record(&netcheck{state: enforcementNotEnforced, detail: "a confined pod reached the internet", at: p.Now(), node: node})
}

// The last gate before a "none" pod is made counts a node found not
// enforcing while the create, start or recreate was under way, not only
// the policies (LOOM-208 review).
func TestNoneNodeFailureBeforeThePod(t *testing.T) {
	refused := func(t *testing.T, err error, what string) {
		t.Helper()
		if rpcCode(err) != rpc.CodeInvalidParams || !strings.Contains(err.Error(), "n1") {
			t.Errorf("%s, n1 found open on the way = %v; want invalid_params naming n1", what, err)
		}
	}
	t.Run("create", func(t *testing.T) {
		cs, _, p, _ := nodeSetup(t, "n0", map[string]int32{"n0": exitBlocked})
		cs.PrependReactor("create", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
			leakOn(p, "n1")
			return false, nil, nil
		})
		_, err := p.CreateTarget(ctx, confinedSpec("conf1"))
		refused(t, err, "create")
		if _, err := cs.CoreV1().Pods(testNS).Get(ctx, podName("conf1"), metav1.GetOptions{}); err == nil {
			t.Error("a pod labelled egress none was made")
		}
	})
	t.Run("start", func(t *testing.T) {
		cs, _, p, _ := nodeSetup(t, "n0", map[string]int32{"n0": exitBlocked})
		if _, err := p.CreateTarget(ctx, confinedSpec("conf1")); err != nil {
			t.Fatal(err)
		}
		if err := p.StopTarget(ctx, "conf1"); err != nil {
			t.Fatal(err)
		}
		var gets atomic.Int32
		cs.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
			// The start reads the pod first, before its own check, then
			// in ensurePod, after it.
			if action.(k8stesting.GetAction).GetName() == podName("conf1") && gets.Add(1) == 2 {
				leakOn(p, "n1")
			}
			return false, nil, nil
		})
		refused(t, p.StartTarget(ctx, "conf1"), "start")
		if _, err := cs.CoreV1().Pods(testNS).Get(ctx, podName("conf1"), metav1.GetOptions{}); err == nil {
			t.Error("a pod labelled egress none was made")
		}
	})
	t.Run("start, the pod still there", func(t *testing.T) {
		cs, _, p, _ := nodeSetup(t, "n0", map[string]int32{"n0": exitBlocked, "n1": exitNetReachable})
		for _, id := range []string{"conf1", "conf2"} {
			if _, err := p.CreateTarget(ctx, confinedSpec(id)); err != nil {
				t.Fatal(err)
			}
		}
		scheduleOn(t, cs, "conf2", "n0")
		statusSoon(t, p, "conf2", protocol.EnvRunning)
		scheduleOn(t, cs, "conf1", "n1")
		statusSoon(t, p, "conf1", protocol.EnvError)
		refused(t, p.StartTarget(ctx, "conf1"), "start")
		if err := p.StartTarget(ctx, "conf2"); err != nil {
			t.Errorf("start of a running machine on an enforced node = %v, want nil", err)
		}
	})
	t.Run("recreate", func(t *testing.T) {
		cs, _, p, _ := nodeSetup(t, "n0", map[string]int32{"n0": exitBlocked})
		if _, err := p.CreateTarget(ctx, confinedSpec("conf1")); err != nil {
			t.Fatal(err)
		}
		cs.PrependReactor("delete", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
			if action.(k8stesting.DeleteAction).GetName() == podName("conf1") {
				leakOn(p, "n1")
			}
			return false, nil, nil
		})
		_, err := p.RecreateTarget(ctx, "conf1", confinedSpec("conf1"))
		refused(t, err, "recreate")
		if _, err := cs.CoreV1().Pods(testNS).Get(ctx, podName("conf1"), metav1.GetOptions{}); err == nil {
			t.Error("a pod labelled egress none was made")
		}
	})
}

// A node's verdicts go by when their probes ran, as the kubelet says,
// not by when their canaries were started or read: a canary may wait on
// its image and run after one started later (LOOM-208 review round 10).
// An escape opens the node unless the latest "enforced" probe began
// after it began; without both times it opens it (fail closed). An open
// node clears only for an "enforced" probe that began after the escapes'
// probes ended, by the kubelet's times or because its canary started
// after the last escape was read; one that couldn't run clears nothing.
func TestNodeVerdictsByProbe(t *testing.T) {
	at := func(s int) time.Time { return time.Date(2026, 10, 23, 1, 0, s, 0, time.UTC) }
	// check is a canary started seq, read done, whose probe ran from..until
	// (seconds; -1 for no kubelet times).
	check := func(seq, done uint64, from, until int, state enforcementState) *netcheck {
		nc := &netcheck{seq: seq, doneSeq: done, state: state, detail: state.String()}
		if from >= 0 {
			nc.probe = probe{from: at(from), until: at(until)}
		}
		return nc
	}

	t.Run("started first, probed after an enforced: recorded", func(t *testing.T) {
		n := &nodeCheck{}
		n.record(check(2, 3, 5, 6, enforcementEnforced))
		n.record(check(1, 4, 10, 11, enforcementNotEnforced)) // its image pulled late
		if n.open == nil || n.open.seq != 1 {
			t.Fatalf("an escape probed after the latest enforced was dropped: open = %+v", n.open)
		}
	})

	t.Run("probed in the same second: recorded", func(t *testing.T) {
		n := &nodeCheck{}
		n.record(check(2, 3, 5, 6, enforcementEnforced))
		n.record(check(1, 4, 5, 7, enforcementNotEnforced)) // the kubelet's times are whole seconds
		if n.open == nil {
			t.Fatal("an escape that began with the latest enforced was dropped")
		}
	})

	t.Run("a stale escape is dropped", func(t *testing.T) {
		n := &nodeCheck{}
		n.record(check(2, 3, 5, 6, enforcementEnforced))
		n.record(check(1, 4, 1, 2, enforcementNotEnforced)) // read late
		if n.open != nil {
			t.Fatalf("an escape probed before the latest enforced opened the node: %+v", n.open)
		}
		if n.enforced.seq != 2 || n.settled.seq != 2 {
			t.Errorf("enforced %d, settled %d; want 2, 2", n.enforced.seq, n.settled.seq)
		}
	})

	t.Run("a missing time fails closed", func(t *testing.T) {
		for _, tc := range []struct {
			name           string
			enforced, open *netcheck
		}{
			{"the escape's", check(2, 3, 5, 6, enforcementEnforced), check(1, 4, -1, -1, enforcementNotEnforced)},
			{"the enforced one's", check(2, 3, -1, -1, enforcementEnforced), check(1, 4, 1, 2, enforcementNotEnforced)},
		} {
			n := &nodeCheck{}
			n.record(tc.enforced)
			n.record(tc.open)
			if n.open == nil {
				t.Errorf("without %s time, an escape read after an enforced was dropped", tc.name)
			}
		}
		// An escape without times clears only for a canary started after
		// it was read: none probed later by the kubelet's times does.
		n := &nodeCheck{}
		n.record(check(1, 4, -1, -1, enforcementNotEnforced))
		n.record(check(2, 5, 30, 31, enforcementEnforced))
		if n.open == nil {
			t.Fatal("an escape without times was cleared by kubelet times")
		}
		n.record(check(6, 7, -1, -1, enforcementEnforced))
		if n.open != nil {
			t.Error("an enforced started after the escape was read left it open")
		}
	})

	t.Run("cleared by probe times", func(t *testing.T) {
		n := &nodeCheck{}
		n.record(check(1, 4, 10, 11, enforcementNotEnforced))
		n.record(check(2, 5, 11, 12, enforcementEnforced)) // began as the escape ended
		if n.open == nil {
			t.Fatal("an enforced probe that began alongside the escape cleared it")
		}
		n.record(check(3, 6, 12, 13, enforcementEnforced)) // started before the escape was read, probed after it
		if n.open != nil {
			t.Errorf("an enforced probed after the escape left it open: %+v", n.open)
		}
	})

	t.Run("every escape must have ended", func(t *testing.T) {
		// The enforced canaries start before the escapes are read, so
		// only the kubelet's times can clear the node.
		n := &nodeCheck{}
		n.record(check(1, 5, 10, 20, enforcementNotEnforced))
		n.record(check(2, 6, 12, 15, enforcementNotEnforced)) // read later, ended sooner
		n.record(check(3, 7, 18, 19, enforcementEnforced))
		if n.open == nil {
			t.Fatal("an enforced probed before the first escape ended cleared the node")
		}
		n.record(check(4, 8, 21, 22, enforcementEnforced))
		if n.open != nil {
			t.Errorf("an enforced probed after both escapes left the node open: %+v", n.open)
		}
		n = &nodeCheck{}
		n.record(check(1, 4, 10, 20, enforcementNotEnforced))
		n.record(check(3, 6, -1, -1, enforcementNotEnforced))
		n.record(check(2, 7, 30, 31, enforcementEnforced))
		if n.open == nil {
			t.Error("with an escape without times, kubelet times cleared the node")
		}
	})

	t.Run("launch order without times", func(t *testing.T) {
		n := &nodeCheck{}
		n.record(check(2, 3, -1, -1, enforcementNotEnforced))
		n.record(check(1, 4, -1, -1, enforcementEnforced)) // started before, read after
		if n.open == nil || n.open.seq != 2 {
			t.Fatalf("an enforced started before the open one cleared it: open = %+v", n.open)
		}
		n.record(check(3, 5, -1, -1, enforcementEnforced)) // started while the open one ran
		if n.open == nil {
			t.Error("an enforced started before the open one was read cleared it")
		}
		n.record(check(6, 7, -1, -1, enforcementFailed))
		if n.open == nil {
			t.Error("a canary that couldn't run cleared not enforced")
		}
		n.record(check(4, 8, -1, -1, enforcementEnforced)) // started after it was read (3)
		if n.open != nil {
			t.Errorf("an enforced started after the open one was read left it open: %+v", n.open)
		}
		if n.settled.seq != 6 || n.enforced.seq != 4 {
			t.Errorf("settled %d, enforced %d; want 6, 4", n.settled.seq, n.enforced.seq)
		}
	})
}

// A canary's probe times are the kubelet's, from its container's end,
// and reach the node's verdict; missing or out of order, they are none.
func TestCanaryProbeTimes(t *testing.T) {
	from, until := time.Date(2026, 10, 23, 1, 0, 5, 0, time.UTC), time.Date(2026, 10, 23, 1, 0, 9, 0, time.UTC)
	ended := func(started, finished time.Time) *corev1.Pod {
		return &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "netcheck", State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{StartedAt: metav1.NewTime(started), FinishedAt: metav1.NewTime(finished)},
		}}}}}
	}
	if got := probeOf(ended(from, until)); got.from != from || got.until != until {
		t.Errorf("probe = %v, want %v..%v", got, from, until)
	}
	for name, pod := range map[string]*corev1.Pod{
		"no start":     ended(time.Time{}, until),
		"no finish":    ended(from, time.Time{}),
		"out of order": ended(until, from),
		"running":      {Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "netcheck"}}}},
	} {
		if got := probeOf(pod); got.known() {
			t.Errorf("%s: probe = %v, want none", name, got)
		}
	}

	cs := fake.NewClientset(append(kindPolicies(t), agentsService())...)
	cs.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		obj, err := cs.Tracker().Get(corev1.SchemeGroupVersion.WithResource("pods"), testNS, action.(k8stesting.GetAction).GetName())
		if err != nil {
			return true, nil, err
		}
		pod := obj.(*corev1.Pod).DeepCopy()
		if pod.Labels[labelCanary] == "true" {
			pod.Spec.NodeName = "n1"
			pod.Status = ended(from, until).Status
			pod.Status.ContainerStatuses[0].State.Terminated.ExitCode = exitBlocked
		}
		return true, pod, nil
	})
	p := configured(t, cs, nil)
	p.mu.Lock()
	nc := p.startNodeCanary(p.cfg, cs, testInstance, "n1")
	p.mu.Unlock()
	select {
	case <-nc.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the canary never finished")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if nc.state != enforcementEnforced || nc.probe.from != from || nc.probe.until != until {
		t.Errorf("canary = %s, probe %v; want enforced, %v..%v", nc.state, nc.probe, from, until)
	}
}

// A node found open after check has its "enforced" verdict, before it
// reports the nodes known open, is reported, and with the policy
// required the check isn't OK: the verdict alone decided OK before
// (LOOM-208 review round 8).
func TestCheckNotOKOnANodeFoundOpenMeanwhile(t *testing.T) {
	cs := fake.NewClientset(append(kindPolicies(t), agentsService())...)
	nodeCanaries(t, cs, "n0", map[string]int32{"n0": exitBlocked})
	p := configured(t, cs, map[string]any{"require_network_policy": true})
	p.verdictHook = func() { leakOn(p, "n1") }
	res, _ := p.Check(ctx)
	if !hasCode(res, ProblemNetworkPolicyNotEnforced) || !strings.Contains(problemText(res), "n1") {
		t.Fatalf("check = %s; want n1 not enforced", problemText(res))
	}
	if res.OK {
		t.Errorf("check OK with an error-severity problem: %s", problemText(res))
	}
}

// With the policy required, a check whose canary is still running isn't
// OK, though "pending" is only a warning.
func TestCheckNotOKWhilePendingIfRequired(t *testing.T) {
	cs := fake.NewClientset(append(kindPolicies(t), agentsService())...) // canaries never finish
	p := configured(t, cs, map[string]any{"require_network_policy": true})
	res, _ := p.Check(ctx)
	if !hasCode(res, ProblemNetworkCheckPending) || res.OK {
		t.Errorf("check = OK %v, %s; want not OK, pending", res.OK, problemText(res))
	}
}
