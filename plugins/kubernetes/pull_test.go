package kubernetes

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/Loomux/server/plugins/protocol"
)

// LOOM-203: a cold image pull is progress, not a failure.

// podWaiting puts pod name on node, Pending with its containers waiting
// for reason (the kubelet's ContainerCreating while it pulls).
func podWaiting(t *testing.T, cs *fake.Clientset, name, node, reason, message string) {
	t.Helper()
	pod, err := cs.CoreV1().Pods(testNS).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pod.Spec.NodeName = node
	pod.Status.Phase = corev1.PodPending
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionTrue}}
	var statuses []corev1.ContainerStatus
	for _, c := range pod.Spec.Containers {
		statuses = append(statuses, corev1.ContainerStatus{Name: c.Name, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: message}}})
	}
	pod.Status.ContainerStatuses = statuses
	if err := cs.Tracker().Update(corev1.SchemeGroupVersion.WithResource("pods"), pod, testNS); err != nil {
		t.Fatal(err)
	}
}

// podEvent records the kubelet's word on pod, at.
func podEvent(t *testing.T, cs *fake.Clientset, pod, name, reason, message string, at time.Time) {
	t.Helper()
	ev := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: name, Namespace: testNS},
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: pod, Namespace: testNS},
		Reason:         reason, Message: message,
		FirstTimestamp: metav1.NewTime(at), LastTimestamp: metav1.NewTime(at),
	}
	if _, err := cs.CoreV1().Events(testNS).Create(ctx, ev, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestPullInProgressIsProgress(t *testing.T) {
	const image = "ghcr.io/loomux/agent:test"
	pulling := func(t *testing.T) (*fake.Clientset, *Plugin) {
		t.Helper()
		cs := fake.NewClientset()
		p := configured(t, cs, nil)
		if _, err := p.CreateTarget(ctx, spec("pull1")); err != nil {
			t.Fatal(err)
		}
		podWaiting(t, cs, podName("pull1"), "n1", "ContainerCreating", "")
		podEvent(t, cs, podName("pull1"), "e1", "Scheduled", "assigned to n1", time.Now().Add(-3*time.Minute))
		podEvent(t, cs, podName("pull1"), "e2", "Pulling", `Pulling image "`+image+`"`, time.Now().Add(-2*time.Minute))
		return cs, p
	}

	t.Run("pulling", func(t *testing.T) {
		_, p := pulling(t)
		env, err := p.GetTarget(ctx, "pull1")
		if err != nil {
			t.Fatal(err)
		}
		if env.Status != protocol.EnvCreating || env.Progress != protocol.ProgressImagePull || !strings.Contains(env.Reason, "pulling the image on node n1 (for 2m") || !strings.Contains(env.Reason, image) {
			t.Errorf("get = %s, progress %q (%s); want creating, image_pull, naming the node, the time and the image", env.Status, env.Progress, env.Reason)
		}
		list, _ := p.ListTargets(ctx)
		if len(list) != 1 || list[0].Progress != protocol.ProgressImagePull {
			t.Errorf("list = %+v", list)
		}
		h, _ := p.TargetHealth(ctx, "pull1")
		if h.Status != protocol.EnvCreating || !strings.Contains(h.Reason, "pulling the image") {
			t.Errorf("health = %+v", h)
		}
	})
	t.Run("pulled", func(t *testing.T) {
		cs, p := pulling(t)
		podEvent(t, cs, podName("pull1"), "e3", "Pulled", "Successfully pulled image", time.Now().Add(-time.Second))
		if env, _ := p.GetTarget(ctx, "pull1"); env.Progress != "" {
			t.Errorf("after Pulled: progress %q (%s), want none", env.Progress, env.Reason)
		}
	})
	for _, fail := range []struct{ event, waiting string }{{"Failed", "ErrImagePull"}, {"BackOff", "ImagePullBackOff"}} {
		t.Run(fail.waiting, func(t *testing.T) {
			cs, p := pulling(t)
			podEvent(t, cs, podName("pull1"), "e3", fail.event, "failed to pull: digest mismatch", time.Now().Add(-time.Second))
			podWaiting(t, cs, podName("pull1"), "n1", fail.waiting, "failed to pull and unpack image: digest mismatch")
			env, _ := p.GetTarget(ctx, "pull1")
			if env.Status != protocol.EnvError || env.Progress != "" || !strings.Contains(env.Reason, fail.waiting+": failed to pull") {
				t.Errorf("get = %s, progress %q (%s); want error naming %s", env.Status, env.Progress, env.Reason, fail.waiting)
			}
		})
	}
	t.Run("another pod's pull", func(t *testing.T) {
		cs := fake.NewClientset()
		p := configured(t, cs, nil)
		if _, err := p.CreateTarget(ctx, spec("pull1")); err != nil {
			t.Fatal(err)
		}
		podWaiting(t, cs, podName("pull1"), "n1", "ContainerCreating", "")
		podEvent(t, cs, "lx-someone-else", "e1", "Pulling", "Pulling image", time.Now())
		if env, _ := p.GetTarget(ctx, "pull1"); env.Progress != "" {
			t.Errorf("another pod's Pulling counted: %q (%s)", env.Progress, env.Reason)
		}
	})
	t.Run("not scheduled", func(t *testing.T) {
		cs := fake.NewClientset()
		p := configured(t, cs, nil)
		if _, err := p.CreateTarget(ctx, spec("pull1")); err != nil {
			t.Fatal(err)
		}
		podEvent(t, cs, podName("pull1"), "e1", "Pulling", "Pulling image", time.Now())
		if env, _ := p.GetTarget(ctx, "pull1"); env.Progress != "" {
			t.Errorf("an unscheduled pod is pulling: %q (%s)", env.Progress, env.Reason)
		}
	})
}

// A canary that times out while its image pulls still proves nothing
// (fail closed, LOOM-208), and says it was pulling.
func TestCanaryTimeoutWhilePulling(t *testing.T) {
	cs := fake.NewClientset(kindPolicies(t)...)
	cs.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		pod, ok := action.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		if ok && pod.Labels[labelCanary] == "true" {
			pod.Spec.NodeName = "n1"
			pod.Status.Phase = corev1.PodPending
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "netcheck", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}}}
		}
		return false, nil, nil
	})
	podEvent(t, cs, "lx-netcheck-abcd", "e1", "Pulling", `Pulling image "ghcr.io/loomux/agent:test"`, time.Now())
	p := configured(t, cs, nil, func(p *Plugin) { p.CanaryTimeout = 300 * time.Millisecond })
	cctx, cancel := context.WithTimeout(ctx, p.CanaryTimeout) // as runCanary bounds it
	defer cancel()
	state, detail, node, _, _ := p.canary(cctx, p.cfg, p.client, p.cfg.apiHost, testInstance, "")
	if state != enforcementFailed || node != "n1" || !strings.Contains(detail, "didn't finish within 300ms, still pulling the image on node n1") {
		t.Errorf("canary = %s on %q (%s), want failed, naming the pull", state, node, detail)
	}
}
