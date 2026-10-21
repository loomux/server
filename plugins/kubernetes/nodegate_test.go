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
