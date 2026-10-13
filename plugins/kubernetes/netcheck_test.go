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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
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
