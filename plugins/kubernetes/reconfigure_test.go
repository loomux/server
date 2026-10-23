package kubernetes

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/Loomux/server/plugins/protocol"
	"github.com/Loomux/server/plugins/rpc"
)

// acrossConfigure pauses the first request of the old configuration
// that does verb on resource (named name, or any), configures the
// plugin anew onto next, and then lets the request go on. It counts the
// canary pods made in the old cluster after that.
type acrossConfigure struct {
	paused   atomic.Bool
	canaries atomic.Int32
}

// newCluster is a cluster with the kind manifests' policies whose
// canaries never finish.
func newCluster(t *testing.T) *fake.Clientset {
	return fake.NewClientset(append(kindPolicies(t), agentsService())...)
}

func pauseAcross(t *testing.T, cs, next *fake.Clientset, p *Plugin, verb, resource, name string, then func()) *acrossConfigure {
	t.Helper()
	a := &acrossConfigure{}
	prependReactor(cs, "create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if pod, ok := action.(k8stesting.CreateAction).GetObject().(*corev1.Pod); ok && a.paused.Load() && strings.HasPrefix(pod.Name, "lx-netcheck-") {
			a.canaries.Add(1)
		}
		return false, nil, nil
	})
	prependReactor(cs, verb, resource, func(action k8stesting.Action) (bool, runtime.Object, error) {
		if named, ok := action.(interface{ GetName() string }); ok && name != "" && named.GetName() != name {
			return false, nil, nil
		}
		if a.paused.CompareAndSwap(false, true) {
			reconfigure(t, p, next)
			if then != nil {
				then()
			}
		}
		return false, nil, nil
	})
	return a
}

// reconfigure configures p anew, onto cs and another API server.
func reconfigure(t *testing.T, p *Plugin, cs kubernetes.Interface) {
	t.Helper()
	p.NewClient = func(Config) (kubernetes.Interface, string, error) { return cs, "10.96.0.2:443", nil }
	if err := p.Configure(ctx, protocol.ConfigureParams{Config: baseConfig(t, nil), Host: protocol.HostInfo{Version: "test", InstanceID: testInstance}}); err != nil {
		t.Fatal(err)
	}
}

// copyMachine puts id's record and pod, as they are in from, into to.
func copyMachine(t *testing.T, from, to *fake.Clientset, id string) {
	t.Helper()
	sec, err := from.CoreV1().Secrets(testNS).Get(ctx, secretName(id), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pod, err := from.CoreV1().Pods(testNS).Get(ctx, podName(id), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, obj := range []runtime.Object{sec, pod} {
		if err := to.Tracker().Add(obj); err != nil {
			t.Fatal(err)
		}
	}
}

// A request that took the configuration before a configure neither
// reads nor writes the new one's verdicts, nor starts canaries, so a
// verdict from the old cluster (or namespace, or API server) can't
// certify a node, or "none", for the new one (LOOM-208 review round 6).
func TestStaleConfigurationCertifiesNothing(t *testing.T) {
	noCanary := func(t *testing.T, a *acrossConfigure) {
		t.Helper()
		if !a.paused.Load() {
			t.Fatal("the request was never paused")
		}
		time.Sleep(200 * time.Millisecond) // one it started would have run
		if n := a.canaries.Load(); n != 0 {
			t.Errorf("the old request made %d canaries in the old cluster after the configure", n)
		}
	}

	t.Run("get", func(t *testing.T) {
		cs, _, p, _ := nodeSetup(t, "n0", map[string]int32{"n0": exitBlocked, "n1": exitBlocked})
		if _, err := p.CreateTarget(ctx, confinedSpec("conf1")); err != nil {
			t.Fatal(err)
		}
		scheduleOn(t, cs, "conf1", "n1")
		next := newCluster(t)
		copyMachine(t, cs, next, "conf1")
		a := pauseAcross(t, cs, next, p, "get", "pods", podName("conf1"), nil)
		env, err := p.GetTarget(ctx, "conf1")
		if err != nil || env.Status == protocol.EnvRunning {
			t.Errorf("the old get = %s (%s), %v; want not running", env.Status, env.Reason, err)
		}
		noCanary(t, a)
		// The new cluster's canaries never finish: only an old verdict
		// could make its machine on n1 running.
		if env, _ := p.GetTarget(ctx, "conf1"); env.Status == protocol.EnvRunning {
			t.Error("the new configuration's machine on n1 is running on the old cluster's verdict")
		}
	})

	t.Run("check", func(t *testing.T) {
		cs, _, p, _ := nodeSetup(t, "n0", map[string]int32{"n0": exitBlocked})
		a := pauseAcross(t, cs, newCluster(t), p, "list", "networkpolicies", "", nil)
		res, _ := p.Check(ctx)
		if !hasCode(res, ProblemNetworkCheckPending) {
			t.Errorf("the old check = %s; want it pending, not the old cluster's verdict", problemText(res))
		}
		noCanary(t, a)
		if noneOffered(t, p) {
			t.Error("none offered in the new configuration on the old cluster's canary")
		}
	})

	t.Run("create", func(t *testing.T) {
		cs, _, p, _ := nodeSetup(t, "n0", map[string]int32{"n0": exitBlocked})
		a := pauseAcross(t, cs, newCluster(t), p, "list", "networkpolicies", "", nil)
		if _, err := p.CreateTarget(ctx, confinedSpec("conf1")); rpcCode(err) != rpc.CodeUnavailable {
			t.Errorf("the old create = %v; want unavailable", err)
		}
		noCanary(t, a)
	})

	t.Run("create, configured after its check", func(t *testing.T) {
		cs, _, p, _ := nodeSetup(t, "n0", map[string]int32{"n0": exitBlocked})
		a := pauseAcross(t, cs, newCluster(t), p, "get", "pods", podName("conf1"), nil)
		if _, err := p.CreateTarget(ctx, confinedSpec("conf1")); rpcCode(err) != rpc.CodeUnavailable {
			t.Errorf("the old create = %v; want unavailable", err)
		}
		noCanary(t, a)
		if _, err := cs.CoreV1().Pods(testNS).Get(ctx, podName("conf1"), metav1.GetOptions{}); err == nil {
			t.Error("a pod labelled egress none was made by the old configuration")
		}
	})

	t.Run("start", func(t *testing.T) {
		cs, _, p, _ := nodeSetup(t, "n0", map[string]int32{"n0": exitBlocked})
		if _, err := p.CreateTarget(ctx, confinedSpec("conf1")); err != nil {
			t.Fatal(err)
		}
		scheduleOn(t, cs, "conf1", "n0")
		a := pauseAcross(t, cs, newCluster(t), p, "get", "pods", podName("conf1"), nil)
		if err := p.StartTarget(ctx, "conf1"); rpcCode(err) != rpc.CodeUnavailable {
			t.Errorf("the old start of the machine still there = %v; want unavailable", err)
		}
		noCanary(t, a)
	})

	t.Run("stop", func(t *testing.T) {
		cs, _, p, _ := nodeSetup(t, "n0", map[string]int32{"n0": exitBlocked})
		if _, err := p.CreateTarget(ctx, confinedSpec("conf1")); err != nil {
			t.Fatal(err)
		}
		scheduleOn(t, cs, "conf1", "n0")
		next := newCluster(t)
		copyMachine(t, cs, next, "conf1")
		a := pauseAcross(t, cs, next, p, "get", "pods", podName("conf1"), func() {
			// The new configuration watches its machine before the old
			// stop is through.
			if env, err := p.GetTarget(ctx, "conf1"); err != nil || env.Status != protocol.EnvStarting {
				t.Errorf("the new get = %s, %v; want starting", env.Status, err)
			}
		})
		if err := p.StopTarget(ctx, "conf1"); err != nil {
			t.Fatal(err)
		}
		if !a.paused.Load() {
			t.Fatal("the stop was never paused")
		}
		p.mu.Lock()
		_, watched := p.watch[podName("conf1")]
		p.mu.Unlock()
		if !watched {
			t.Error("the old stop unwatched the new configuration's machine")
		}
	})

	t.Run("a canary of the old configuration that gets in", func(t *testing.T) {
		cs, _, p, _ := nodeSetup(t, "n0", map[string]int32{"n0": exitBlocked, "n1": exitBlocked})
		var script atomic.Value
		prependReactor(cs, "create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
			if pod, ok := action.(k8stesting.CreateAction).GetObject().(*corev1.Pod); ok && pod.Spec.NodeName == "n1" {
				script.Store(strings.Join(pod.Spec.Containers[0].Command, " "))
			}
			return false, nil, nil
		})
		p.mu.Lock()
		old := p.cfg
		p.mu.Unlock()
		reconfigure(t, p, newCluster(t))
		// Past the guards, as a path that forgot one would be.
		p.mu.Lock()
		nc := p.startNodeCanary(old, cs, testInstance, "n1")
		p.mu.Unlock()
		select {
		case <-nc.done:
		case <-time.After(5 * time.Second):
			t.Fatal("the canary never finished")
		}
		p.mu.Lock()
		n := p.nodes["n1"]
		p.mu.Unlock()
		if n.enforced != nil || n.settled != nil {
			t.Errorf("the old configuration's canary was recorded for n1: %+v", n)
		}
		if n.running != nil {
			t.Error("the old canary is still n1's running one: the node would never be checked again")
		}
		// It probed its own configuration's API server, not the new one.
		if got, _ := script.Load().(string); !strings.Contains(got, "10.96.0.1") || strings.Contains(got, "10.96.0.2") {
			t.Errorf("the old canary's script probes the wrong API server: %q", got)
		}
	})
}
