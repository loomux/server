package kubernetes

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/Loomux/server/plugins/protocol"
)

// fakeProbes puts stub timeout and sleep commands first on PATH for the
// canary script: the API server answers its first apiAnswers probes and
// the internet its first netAnswers, then both are blocked, the way a
// policy engine confines a new pod once it has learned its address.
// kube-dns (port 53) always answers. It returns the PATH and a function
// reading how many times a class was probed or the script slept.
func fakeProbes(t *testing.T, apiAnswers, netAnswers int) (path string, count func(string) int) {
	t.Helper()
	dir := t.TempDir()
	stub := `#!/bin/bash
# timeout 3 bash -c "exec 3<>/dev/tcp/<host>/<port>"
dest=${4##*/dev/tcp/}
case $dest in
  */53) exit 0 ;;
  1.1.1.1/*) class=net limit=` + strconv.Itoa(netAnswers) + ` ;;
  *) class=api limit=` + strconv.Itoa(apiAnswers) + ` ;;
esac
n=$(cat "` + dir + `/$class" 2>/dev/null || echo 0)
echo $((n + 1)) > "` + dir + `/$class"
[ "$n" -lt "$limit" ]
`
	if err := os.WriteFile(filepath.Join(dir, "timeout"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	sleep := "#!/bin/bash\nn=$(cat \"" + dir + "/sleeps\" 2>/dev/null || echo 0)\necho $((n + 1)) > \"" + dir + "/sleeps\"\n"
	if err := os.WriteFile(filepath.Join(dir, "sleep"), []byte(sleep), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir + ":" + os.Getenv("PATH"), func(class string) int {
		b, _ := os.ReadFile(filepath.Join(dir, class))
		n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		return n
	}
}

// TestCanaryScriptSettles runs the canary script itself: a new pod can
// be unconfined for a moment while the policy engine learns its address
// (about 2 s with kube-network-policies on TEST, 2026-10-13), so a
// target that answers is tried again, and only one still answering
// after canaryAttempts decides "not enforced".
func TestCanaryScriptSettles(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	if _, err := os.Stat("/etc/resolv.conf"); err != nil {
		t.Skip("no /etc/resolv.conf for the kube-dns control")
	}
	for _, tc := range []struct {
		name                   string
		apiAnswers, netAnswers int
		exit, sleeps           int
	}{
		{"enforced from the start", 0, 0, exitBlocked, 0},
		{"enforced once the engine caught up", 2, 0, exitBlocked, 2},
		{"both answer at first, then neither", 1, 1, exitBlocked, 2},
		{"API server never confined", 1 << 20, 0, exitAPIReachable, canaryAttempts - 1},
		{"internet never confined", 0, 1 << 20, exitNetReachable, canaryAttempts - 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, count := fakeProbes(t, tc.apiAnswers, tc.netAnswers)
			cmd := exec.Command(bash, "-c", canaryScript("10.96.0.1:443"))
			cmd.Env = []string{"PATH=" + path}
			err := cmd.Run()
			code := 0
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				code = ee.ExitCode()
			} else if err != nil {
				t.Fatal(err)
			}
			if code != tc.exit {
				t.Errorf("exit = %d, want %d", code, tc.exit)
			}
			if got := count("sleeps"); got != tc.sleeps {
				t.Errorf("slept %d times, want %d", got, tc.sleeps)
			}
		})
	}
}

// A transient API error while the canary runs (etcd "leader changed",
// seen on TEST at install) is retried, not taken as the verdict.
func TestCanaryTransientReadError(t *testing.T) {
	cs := fake.NewClientset(&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "loomux-agents", Namespace: testNS}})
	canaryExits(cs, exitBlocked)
	failed := false
	cs.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if !failed {
			failed = true
			return true, nil, errors.New("etcdserver: leader changed")
		}
		return false, nil, nil
	})
	p := configured(t, cs, nil)
	res, _ := p.Check(ctx)
	if !failed || !res.OK || len(res.Problems) != 0 {
		t.Fatalf("check after one failed read = %+v (failed read injected: %v)", res, failed)
	}
}

// A read error the canary recovers from isn't reported when the canary
// later times out for another reason (a slow pull): the timeout is.
func TestCanaryStaleReadErrorNotReported(t *testing.T) {
	cs := fake.NewClientset()
	failed := false
	cs.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if !failed {
			failed = true
			return true, nil, errors.New("etcdserver: leader changed")
		}
		return false, nil, nil
	})
	p := configured(t, cs, nil) // the canary never finishes: no kubelet
	p.CanaryTimeout = 50 * time.Millisecond
	res, _ := p.Check(ctx)
	msg := failedMessage(res)
	if !failed || !strings.Contains(msg, "didn't finish within") || strings.Contains(msg, "leader changed") {
		t.Fatalf("check = %+v", res)
	}
}

// Forbidden or Unauthorized won't heal by waiting: reported at once.
func TestCanaryForbiddenReadFailsFast(t *testing.T) {
	cs := fake.NewClientset()
	cs.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, kerrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "lx-netcheck-abcd", errors.New("no"))
	})
	p := configured(t, cs, nil)
	p.CanaryTimeout = time.Minute
	start := time.Now()
	res, _ := p.Check(ctx)
	if !strings.Contains(failedMessage(res), "forbidden") || time.Since(start) > 10*time.Second {
		t.Fatalf("check = %+v after %v", res, time.Since(start))
	}
}

// failedMessage is the network_policy_check_failed problem's message.
func failedMessage(res protocol.CheckResult) string {
	for _, x := range res.Problems {
		if x.Code == ProblemNetworkCheckFailed {
			return x.Message
		}
	}
	return ""
}

// One that persists until the canary's time is up is reported, with the
// last error.
func TestCanaryPersistentReadError(t *testing.T) {
	cs := fake.NewClientset()
	cs.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("etcdserver: leader changed")
	})
	p := configured(t, cs, nil)
	p.CanaryTimeout = 50 * time.Millisecond
	res, _ := p.Check(ctx)
	var pr string
	for _, x := range res.Problems {
		if x.Code == ProblemNetworkCheckFailed {
			pr = x.Message
		}
	}
	if !strings.Contains(pr, "leader changed") {
		t.Fatalf("check = %+v", res)
	}
}

// A canary left behind (its delete failed while the API server was
// unavailable, seen on TEST) is removed by the next check once it has
// terminated or outlived any check; a live one, which may be another
// plugin process's (an old and a new sidecar overlapping in a rollout),
// is not. Another server's canaries and this server's machines are
// never touched.
func TestCanarySweepsStale(t *testing.T) {
	now := time.Date(2026, 10, 13, 8, 0, 0, 0, time.UTC)
	canary := func(name, instance string, phase corev1.PodPhase, age time.Duration) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS, CreationTimestamp: metav1.NewTime(now.Add(-age)), Labels: map[string]string{
			labelManagedBy: managedBy, protocol.LabelInstance: instance, labelRole: roleAgent, labelCanary: "true",
		}}, Status: corev1.PodStatus{Phase: phase}}
	}
	machine := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "lx-keep", Namespace: testNS, Labels: map[string]string{
		labelManagedBy: managedBy, protocol.LabelInstance: testInstance, labelRole: roleAgent,
	}}}
	cs := fake.NewClientset(
		canary("lx-netcheck-failed", testInstance, corev1.PodFailed, time.Minute),
		canary("lx-netcheck-succeeded", testInstance, corev1.PodSucceeded, time.Minute),
		canary("lx-netcheck-stuck", testInstance, corev1.PodRunning, time.Hour),
		canary("lx-netcheck-live", testInstance, corev1.PodRunning, 30*time.Second),
		canary("lx-netcheck-pulling", testInstance, corev1.PodPending, 2*time.Minute),
		canary("lx-netcheck-other", "another-instance", corev1.PodFailed, time.Hour),
		machine,
	)
	canaryExits(cs, exitBlocked)
	p := configured(t, cs, nil)
	p.Now = func() time.Time { return now }
	p.CanaryTimeout = 3 * time.Minute
	p.Check(ctx)
	pods, _ := cs.CoreV1().Pods(testNS).List(ctx, metav1.ListOptions{})
	var left []string
	for _, pod := range pods.Items {
		left = append(left, pod.Name)
	}
	if got, want := strings.Join(left, ","), "lx-keep,lx-netcheck-live,lx-netcheck-other,lx-netcheck-pulling"; got != want {
		t.Errorf("pods left = %s, want %s", got, want)
	}
}

// The kubelet's deadline must not end the canary before the plugin stops
// waiting for it: it counts the image pull (3 min on a cold node on TEST).
func TestCanaryDeadlineCoversTimeout(t *testing.T) {
	p := newTestPlugin(fake.NewClientset())
	p.CanaryTimeout = 3 * time.Minute
	pod := canaryPod("lx-netcheck-abcd", Config{AgentImage: "img"}, testInstance, "", p.CanaryTimeout)
	if d := pod.Spec.ActiveDeadlineSeconds; d == nil || time.Duration(*d)*time.Second <= p.CanaryTimeout {
		t.Errorf("activeDeadlineSeconds = %v, want more than the canary timeout %v", d, p.CanaryTimeout)
	}
}
