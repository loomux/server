package kubernetes

import (
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/Loomux/server/plugins/protocol"
)

// LOOM-212: a canary is deleted once its verdict is recorded, so none
// is left behind Completed; one still running for another check is
// left to it; one whose delete fails is deleted again by the sweep,
// and its verdict stands.

// canaryNames is the canary pods in namespace ns, sorted.
func canaryNames(t *testing.T, cs *fake.Clientset, ns string) []string {
	t.Helper()
	list, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: labelCanary + "=true"})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, pod := range list.Items {
		names = append(names, pod.Name)
	}
	sort.Strings(names)
	return names
}

// canaryGone waits up to 5 s for the canary pod name in ns to be gone.
func canaryGone(t *testing.T, cs *fake.Clientset, ns, name, why string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := cs.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{}); kerrors.IsNotFound(err) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the canary %s was never deleted: %s", name, why)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// numbered names each canary pod apart: lx-netcheck-c1, -c2, …
func numbered(p *Plugin) {
	var n atomic.Int32
	p.Rand = func() string { return "c" + strconv.Itoa(int(n.Add(1))) }
}

// A finished canary is deleted after its verdict is recorded, and by
// the time a check has its answer: none is left Completed.
func TestCanaryDeletedOnceItsVerdictIsRecorded(t *testing.T) {
	cs := fake.NewClientset(kindPolicies(t)...)
	canaryExits(cs, exitBlocked)
	var p *Plugin
	var mu sync.Mutex
	var deleted []string
	var recordedFirst []bool
	cs.PrependReactor("delete", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		p.mu.Lock()
		recorded := p.settled != nil && p.settled.state == enforcementEnforced
		p.mu.Unlock()
		mu.Lock()
		deleted = append(deleted, action.(k8stesting.DeleteAction).GetName())
		recordedFirst = append(recordedFirst, recorded)
		mu.Unlock()
		return false, nil, nil
	})
	p = configured(t, cs, nil, func(pp *Plugin) { p = pp })
	res, err := p.Check(ctx)
	if err != nil || !res.OK {
		t.Fatalf("check = %+v, %v", res, err)
	}
	if left := canaryNames(t, cs, testNS); len(left) != 0 {
		t.Errorf("canaries left after the check answered: %v", left)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(deleted) != 1 || deleted[0] != "lx-netcheck-abcd" || !recordedFirst[0] {
		t.Errorf("deletes = %v, verdict recorded before each: %v; want lx-netcheck-abcd, after its verdict", deleted, recordedFirst)
	}
}

// A canary still running for another check (a node's, LOOM-208) isn't
// touched when the check's canary finishes and is deleted, nor by its
// sweep; it goes once its own reader is done with it, even if it never
// finished.
func TestCanaryStillRunningIsLeftAlone(t *testing.T) {
	cs := fake.NewClientset(kindPolicies(t)...)
	// The check's canary (unpinned) exits; the node's, pinned, runs on.
	cs.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		pod, ok := action.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		if ok && pod.Labels[labelCanary] == "true" && pod.Spec.NodeName == "" {
			pod.Status.Phase = corev1.PodSucceeded
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "netcheck", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: exitBlocked}}}}
		}
		return false, nil, nil
	})
	p := configured(t, cs, nil, numbered)
	p.mu.Lock()
	node := p.startNodeCanary(p.cfg, p.client, testInstance, "node-a")
	p.mu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	for len(canaryNames(t, cs, testNS)) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the node's canary was never made")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if res, err := p.Check(ctx); err != nil || !res.OK {
		t.Fatalf("check = %+v, %v", res, err)
	}
	if left := canaryNames(t, cs, testNS); len(left) != 1 || left[0] != "lx-netcheck-c1" {
		t.Fatalf("canaries after the check = %v, want only the node's, still running: lx-netcheck-c1", left)
	}
	p.sweepLeft()
	if left := canaryNames(t, cs, testNS); len(left) != 1 {
		t.Fatalf("the sweep took the running canary: left %v", left)
	}
	select {
	case <-node.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the node's canary never ended")
	}
	if left := canaryNames(t, cs, testNS); len(left) != 0 {
		t.Errorf("a canary that outlived its wait was left: %v", left)
	}
}

// A canary whose delete fails (the API server unavailable) is deleted
// again by the sweep, whatever its phase (here the kubelet hasn't yet
// marked it Succeeded, so the label sweep alone would leave it) and in
// its own configuration's namespace after a configure moved on; its
// verdict stands meanwhile.
func TestCanaryDeleteFailureRetriedBySweep(t *testing.T) {
	cs := fake.NewClientset(kindPolicies(t)...)
	cs.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		pod, ok := action.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		if ok && pod.Labels[labelCanary] == "true" {
			pod.Status.Phase = corev1.PodRunning
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "netcheck", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: exitBlocked}}}}
		}
		return false, nil, nil
	})
	var failing atomic.Bool
	failing.Store(true)
	var attempts atomic.Int32
	cs.PrependReactor("delete", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		attempts.Add(1)
		if failing.Load() {
			return true, nil, kerrors.NewServiceUnavailable("etcdserver: request timed out")
		}
		return false, nil, nil
	})
	var clock *testClock
	p := configured(t, cs, nil, func(p *Plugin) { clock = newClock(p) })
	enforced := func(when string) {
		t.Helper()
		if res, err := p.Check(ctx); err != nil || !res.OK {
			t.Errorf("%s: check = %+v, %v", when, res, err)
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.settled == nil || p.settled.state != enforcementEnforced {
			t.Errorf("%s: the verdict = %+v, want enforced", when, p.settled)
		}
	}
	enforced("the delete failed")
	if left := canaryNames(t, cs, testNS); len(left) != 1 || left[0] != "lx-netcheck-abcd" {
		t.Fatalf("canaries = %v, want the undeleted lx-netcheck-abcd", left)
	}
	before := attempts.Load()
	p.sweepLeft()
	if attempts.Load() == before {
		t.Error("the sweep didn't try the failed delete again")
	}
	enforced("the sweep's delete failed too")
	p.mu.Lock()
	_, kept := p.undeleted["lx-netcheck-abcd"]
	p.mu.Unlock()
	if !kept {
		t.Fatal("a canary still undeleted was forgotten")
	}

	// A configure moves to another namespace; the API server is back.
	failing.Store(false)
	if err := p.Configure(ctx, protocol.ConfigureParams{Config: baseConfig(t, map[string]any{"namespace": "loomux-elsewhere"}), Host: protocol.HostInfo{Version: "test", InstanceID: testInstance}}); err != nil {
		t.Fatal(err)
	}
	clock.advance(2 * time.Minute)
	canaryGone(t, cs, testNS, "lx-netcheck-abcd", "the sweep of the next configuration should have deleted it")
	deadline := time.Now().Add(5 * time.Second)
	for {
		p.mu.Lock()
		n := len(p.undeleted)
		p.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a deleted canary is still on the list to retry")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A delete failing for longer than undeletedFor (the Role lost delete)
// is given up, so the list stays bounded.
func TestCanaryDeleteGivenUp(t *testing.T) {
	cs := fake.NewClientset(kindPolicies(t)...)
	canaryExits(cs, exitBlocked)
	cs.PrependReactor("delete", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, kerrors.NewForbidden(corev1.Resource("pods"), action.(k8stesting.DeleteAction).GetName(), nil)
	})
	var clock *testClock
	p := configured(t, cs, nil, func(p *Plugin) { clock = newClock(p) })
	if res, err := p.Check(ctx); err != nil || !res.OK {
		t.Fatalf("check = %+v, %v", res, err)
	}
	p.sweepLeft()
	p.mu.Lock()
	n := len(p.undeleted)
	p.mu.Unlock()
	if n != 1 {
		t.Fatalf("undeleted = %d, want the one canary", n)
	}
	clock.advance(undeletedFor + time.Minute)
	p.sweepLeft()
	p.mu.Lock()
	n = len(p.undeleted)
	p.mu.Unlock()
	if n != 0 {
		t.Errorf("undeleted = %d after %s, want it given up", n, undeletedFor)
	}
}
