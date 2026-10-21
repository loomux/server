package kubernetes

import (
	"strings"
	"sync/atomic"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

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

// A node's verdicts go by when their canaries started, not when they
// were read: one started earlier and read later (its pod was slow, or
// the check's canary landed there meanwhile) never undoes a newer one
// (LOOM-208 review).
func TestNodeVerdictsByStart(t *testing.T) {
	check := func(seq uint64, state enforcementState) *netcheck {
		return &netcheck{seq: seq, state: state, detail: state.String()}
	}
	n := &nodeCheck{}
	n.record(check(2, enforcementNotEnforced))
	n.record(check(1, enforcementEnforced)) // older, read later
	if n.open == nil || n.open.seq != 2 {
		t.Fatalf("an older enforced cleared a newer not enforced: open = %+v", n.open)
	}
	if n.settled.seq != 2 {
		t.Errorf("settled = %d, want the newer 2", n.settled.seq)
	}
	n.record(check(4, enforcementFailed))
	if n.open == nil {
		t.Error("a canary that couldn't run cleared not enforced")
	}
	n.record(check(3, enforcementEnforced)) // older than the failed one, newer than the open
	if n.open != nil {
		t.Errorf("an enforced started after the open one left it open: %+v", n.open)
	}
	if n.settled.seq != 4 || n.enforced.seq != 3 {
		t.Errorf("settled %d, enforced %d; want 4, 3", n.settled.seq, n.enforced.seq)
	}
}
