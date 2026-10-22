package kubernetes

import (
	"strings"
	"sync/atomic"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
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

// A node found open stays open until a canary started after that
// finding (after the open one finished) says "enforced": one that got
// out is evidence of an escape during its run, and a canary that ran
// alongside it, started after it, only sampled the same time. One that
// got out but started before the latest "enforced" did is older news
// and opens nothing, however late it is read. One that couldn't run
// clears nothing (LOOM-208 review).
func TestNodeVerdictsByStart(t *testing.T) {
	check := func(seq, done uint64, state enforcementState) *netcheck {
		return &netcheck{seq: seq, doneSeq: done, state: state, detail: state.String()}
	}
	n := &nodeCheck{}
	n.record(check(2, 3, enforcementNotEnforced))
	n.record(check(1, 4, enforcementEnforced)) // started before, read after
	if n.open == nil || n.open.seq != 2 {
		t.Fatalf("an enforced started before the open one cleared it: open = %+v", n.open)
	}
	if n.settled.seq != 2 {
		t.Errorf("settled = %d, want the newer 2", n.settled.seq)
	}

	n = &nodeCheck{}
	n.record(check(2, 3, enforcementEnforced))
	n.record(check(1, 4, enforcementNotEnforced)) // started before, out, read after
	if n.open != nil {
		t.Fatalf("an escape started before a newer enforced reopened the node: open = %+v", n.open)
	}
	if n.enforced.seq != 2 || n.settled.seq != 2 {
		t.Errorf("enforced %d, settled %d; want 2, 2", n.enforced.seq, n.settled.seq)
	}
	n.record(check(5, 6, enforcementNotEnforced)) // started after the enforced one
	if n.open == nil || n.open.seq != 5 {
		t.Fatalf("an escape started after the last enforced was dropped: open = %+v", n.open)
	}
	n.record(check(4, 7, enforcementNotEnforced)) // older start, but out and done later
	if n.open == nil || n.open.doneSeq != 7 {
		t.Errorf("open = %+v; want the later-finished escape, 7", n.open)
	}
	n.record(check(6, 8, enforcementEnforced)) // started while the open one ran
	if n.open == nil {
		t.Error("an enforced started before the open one finished cleared it")
	}
	n.record(check(9, 10, enforcementFailed))
	if n.open == nil {
		t.Error("a canary that couldn't run cleared not enforced")
	}
	n.record(check(8, 11, enforcementEnforced)) // started after the open one finished (7)
	if n.open != nil {
		t.Errorf("an enforced started after the open one finished left it open: %+v", n.open)
	}
	if n.settled.seq != 9 || n.enforced.seq != 8 {
		t.Errorf("settled %d, enforced %d; want 9, 8", n.settled.seq, n.enforced.seq)
	}
}
