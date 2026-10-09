package kubernetes

import (
	"context"
	"errors"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/Loomux/server/plugins"
	"github.com/Loomux/server/plugins/plugintest"
	"github.com/Loomux/server/plugins/protocol"
	"github.com/Loomux/server/plugins/rpc"
	"github.com/Loomux/server/plugins/sdk"
)

const (
	testInstance = "11111111-2222-3333-4444-555555555555"
	testNS       = "loomux-agents"
)

var ctx = context.Background()

// newTestPlugin is a plugin over cs, with short waits.
func newTestPlugin(cs kubernetes.Interface) *Plugin {
	p := New()
	p.NewClient = func(Config) (kubernetes.Interface, string, error) { return cs, "10.96.0.1:443", nil }
	p.Logger = log.New(io.Discard, "", 0)
	p.Poll = 5 * time.Millisecond
	p.DeleteWait = 200 * time.Millisecond
	p.CanaryWait = time.Second
	p.CanaryTimeout = 2 * time.Second
	p.Rand = func() string { return "abcd" }
	return p
}

// baseConfig is a valid configuration with the schema's defaults.
func baseConfig(t *testing.T, extra map[string]any) map[string]any {
	t.Helper()
	cfg := map[string]any{"namespace": testNS, "storage_class": "ceph-rbd-sc", "agent_image": "ghcr.io/loomux/agent:test"}
	for k, v := range extra {
		cfg[k] = v
	}
	m, err := plugins.ParseManifest(ManifestJSON)
	if err != nil {
		t.Fatal(err)
	}
	s, err := m.Schema()
	if err != nil {
		t.Fatal(err)
	}
	cfg = s.ApplyDefaults(cfg)
	if err := s.Validate(cfg); err != nil {
		t.Fatalf("base config isn't valid for the schema: %v", err)
	}
	return cfg
}

// configured is a plugin over a fresh fake clientset, configured.
func configured(t *testing.T, cs *fake.Clientset, extra map[string]any) *Plugin {
	t.Helper()
	p := newTestPlugin(cs)
	if err := p.Configure(ctx, protocol.ConfigureParams{Config: baseConfig(t, extra), Host: protocol.HostInfo{Version: "test", InstanceID: testInstance}}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	return p
}

func spec(id string) protocol.EnvironmentSpec {
	return protocol.EnvironmentSpec{
		ID: id, TargetID: "t-" + id, Name: "Builds  (big)\n", Size: "small", Persistent: true, Egress: protocol.EgressInternet,
		Image: "ghcr.io/loomux/agent:test",
		SSH: protocol.SSHBootstrap{
			AuthorizedKey:  "no-port-forwarding,no-agent-forwarding,no-X11-forwarding ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIKey loomux",
			HostPrivateKey: "-----BEGIN OPENSSH PRIVATE KEY-----\nSECRETMATERIAL\n-----END OPENSSH PRIVATE KEY-----\n",
			HostPublicKey:  "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIHost", Port: 2222,
		},
		Labels: map[string]string{protocol.LabelInstance: testInstance, protocol.LabelTarget: "t-" + id, protocol.LabelEnvironment: id},
	}
}

// canaryExits makes every canary pod the fake creates exit with code.
func canaryExits(cs *fake.Clientset, code int32) {
	cs.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		pod, ok := action.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		if ok && pod.Labels[labelCanary] == "true" {
			pod.Status.Phase = corev1.PodSucceeded
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "netcheck", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: code}}}}
		}
		return false, nil, nil
	})
}

func rpcCode(err error) string {
	var re *rpc.Error
	if errors.As(err, &re) {
		return re.Code
	}
	if err == nil {
		return "<nil>"
	}
	return "<" + err.Error() + ">"
}

func TestManifest(t *testing.T) {
	m, err := plugins.ParseManifest(ManifestJSON)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	if m.Name != "kubernetes" || m.Version != Version || len(m.Capabilities) != 7 || len(m.Permissions) != 2 {
		t.Errorf("manifest = %+v", m)
	}
	s, err := m.Schema()
	if err != nil {
		t.Fatal(err)
	}
	if !s.Properties["kubeconfig"].Secret || s.Properties["agent_image"].Format != "image-reference" {
		t.Error("kubeconfig must be x-secret and agent_image an image-reference")
	}
	if s.Properties["storage_class"].Default != "ceph-rbd-sc-delete" {
		t.Errorf("storage_class default = %v, want the Delete-reclaim class (a Retain class is refused by the quota)", s.Properties["storage_class"].Default)
	}
	if cfg := s.ApplyDefaults(map[string]any{}); cfg["namespace"] != "loomux-agents" || cfg["max_environments"] != 5.0 && cfg["max_environments"] != 5 {
		t.Errorf("defaults = %v", cfg)
	}
}

func TestParseConfig(t *testing.T) {
	cfg, err := parseConfig(baseConfig(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Namespace != testNS || cfg.Subdomain != "loomux-agents" || cfg.MaxEnvironments != 5 || cfg.Timezone != "UTC" || cfg.RequireNetworkPolicy || len(cfg.Sizes) != 3 || cfg.Sizes[0].Name != "small" {
		t.Errorf("config = %+v", cfg)
	}
	if cfg.addressTemplate() != "lx-{id}.loomux-agents.loomux-agents.svc.cluster.local" {
		t.Errorf("address template = %q", cfg.addressTemplate())
	}
	custom, err := parseConfig(baseConfig(t, map[string]any{
		"sizes":            map[string]any{"big": map[string]any{"cpu": "8", "memory": "32Gi", "disk": "100Gi"}, "tiny": map[string]any{"cpu": "250m", "memory": "512Mi", "disk": "1Gi"}},
		"max_environments": 2.0, "require_network_policy": true,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(custom.Sizes) != 2 || custom.Sizes[0].Name != "tiny" || custom.Sizes[1].Name != "big" || custom.MaxEnvironments != 2 || !custom.RequireNetworkPolicy {
		t.Errorf("custom = %+v", custom)
	}
	bad := map[string]map[string]any{
		"namespace":  {"namespace": "Loomux_Agents"},
		"image":      {"agent_image": "ghcr.io/x; rm -rf /"},
		"size":       {"sizes": map[string]any{"s": map[string]any{"cpu": "lots", "memory": "1Gi", "disk": "1Gi"}}},
		"size-name":  {"sizes": map[string]any{"Big One": map[string]any{"cpu": "1", "memory": "1Gi", "disk": "1Gi"}}},
		"max":        {"max_environments": 0.0},
		"require":    {"require_network_policy": "yes"},
		"no-storage": {"storage_class": ""},
		"timezone":   {"timezone": "Europe/Warsaw; echo"},
	}
	for name, extra := range bad {
		cfg := map[string]any{"namespace": testNS, "storage_class": "sc", "agent_image": "img"}
		for k, v := range extra {
			cfg[k] = v
		}
		if _, err := parseConfig(cfg); rpcCode(err) != rpc.CodeInvalidConfig {
			t.Errorf("%s: want invalid_config, got %v", name, err)
		}
	}
}

func TestPodSpecIsRestricted(t *testing.T) {
	cfg, _ := parseConfig(baseConfig(t, map[string]any{"timezone": "Europe/Warsaw"}))
	sp := spec("abc123")
	size, _ := cfg.size("small")
	pod := podFor(sp, size, cfg, testInstance)

	if pod.Name != "lx-abc123" || pod.Spec.Hostname != "lx-abc123" || pod.Spec.Subdomain != "loomux-agents" {
		t.Errorf("name/hostname/subdomain = %q %q %q", pod.Name, pod.Spec.Hostname, pod.Spec.Subdomain)
	}
	want := map[string]string{labelManagedBy: managedBy, protocol.LabelInstance: testInstance, protocol.LabelTarget: "t-abc123", protocol.LabelEnvironment: "abc123", labelRole: "agent", labelEgress: "internet"}
	for k, v := range want {
		if pod.Labels[k] != v {
			t.Errorf("label %s = %q, want %q", k, pod.Labels[k], v)
		}
	}
	if len(pod.Labels) != len(want) {
		t.Errorf("unexpected labels: %v", pod.Labels)
	}
	if pod.Annotations[annotationName] != "Builds (big)" {
		t.Errorf("target-name annotation = %q", pod.Annotations[annotationName])
	}
	ps := pod.Spec
	if ps.AutomountServiceAccountToken == nil || *ps.AutomountServiceAccountToken || ps.EnableServiceLinks == nil || *ps.EnableServiceLinks || ps.RestartPolicy != corev1.RestartPolicyAlways {
		t.Error("automountServiceAccountToken and enableServiceLinks must be false, restartPolicy Always")
	}
	if ps.HostNetwork || ps.HostPID || ps.HostIPC || ps.ServiceAccountName != "" {
		t.Error("no host namespaces, no service account")
	}
	sc := ps.SecurityContext
	if sc == nil || !*sc.RunAsNonRoot || *sc.RunAsUser != 10002 || *sc.RunAsGroup != 10002 || *sc.FSGroup != 10002 || sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("pod securityContext = %+v", sc)
	}
	if len(ps.Containers) != 1 || len(ps.InitContainers) != 0 {
		t.Fatalf("containers = %d, init = %d", len(ps.Containers), len(ps.InitContainers))
	}
	c := ps.Containers[0]
	if c.Name != "agent" || c.Image != "ghcr.io/loomux/agent:test" {
		t.Errorf("container = %s %s", c.Name, c.Image)
	}
	csc := c.SecurityContext
	if csc == nil || *csc.AllowPrivilegeEscalation || !*csc.ReadOnlyRootFilesystem || csc.Capabilities == nil || len(csc.Capabilities.Drop) != 1 || csc.Capabilities.Drop[0] != "ALL" || csc.Privileged != nil {
		t.Errorf("container securityContext = %+v", csc)
	}
	env := map[string]string{}
	for _, e := range c.Env {
		if strings.HasPrefix(e.Name, "LOOMUX_") {
			t.Errorf("a LOOMUX_* variable reaches the machine: %s", e.Name)
		}
		env[e.Name] = e.Value
	}
	if env["HOME"] != "/data/home" || env["CLAUDE_CONFIG_DIR"] != "/data/home/.claude" || env["TZ"] != "Europe/Warsaw" {
		t.Errorf("env = %v", env)
	}
	if len(c.Ports) != 1 || c.Ports[0].ContainerPort != 2222 || c.Ports[0].Name != "ssh" {
		t.Errorf("ports = %+v", c.Ports)
	}
	if c.Resources.Requests.Cpu().String() != "1" || c.Resources.Requests.Memory().String() != "2Gi" || c.Resources.Limits.Cpu().String() != "1" || c.Resources.Limits.StorageEphemeral().String() != "2Gi" {
		t.Errorf("resources = %+v", c.Resources)
	}
	if c.ReadinessProbe == nil || c.ReadinessProbe.TCPSocket == nil || c.ReadinessProbe.TCPSocket.Port.StrVal != "ssh" || c.ReadinessProbe.PeriodSeconds != 5 {
		t.Errorf("readiness = %+v", c.ReadinessProbe)
	}
	mounts := map[string]corev1.VolumeMount{}
	for _, m := range c.VolumeMounts {
		mounts[m.Name] = m
	}
	if mounts["data"].MountPath != "/data" || mounts["tmp"].MountPath != "/tmp" || mounts["run"].MountPath != "/run/loomux" || mounts["ssh-src"].MountPath != "/etc/loomux/ssh-src" || !mounts["ssh-src"].ReadOnly || len(mounts) != 4 {
		t.Errorf("mounts = %+v", c.VolumeMounts)
	}
	vols := map[string]corev1.Volume{}
	for _, v := range ps.Volumes {
		vols[v.Name] = v
		if v.HostPath != nil {
			t.Errorf("a hostPath volume: %s", v.Name)
		}
	}
	if vols["data"].PersistentVolumeClaim == nil || vols["data"].PersistentVolumeClaim.ClaimName != "lx-abc123-data" {
		t.Errorf("data volume = %+v", vols["data"])
	}
	if vols["tmp"].EmptyDir == nil || vols["tmp"].EmptyDir.SizeLimit.String() != "1Gi" {
		t.Errorf("tmp volume = %+v", vols["tmp"])
	}
	if vols["run"].EmptyDir == nil || vols["run"].EmptyDir.Medium != corev1.StorageMediumMemory || vols["run"].EmptyDir.SizeLimit.String() != "1Mi" {
		t.Errorf("run volume = %+v", vols["run"])
	}
	if s := vols["ssh-src"].Secret; s == nil || s.SecretName != "lx-abc123-ssh" || s.DefaultMode == nil || *s.DefaultMode != 0o400 {
		t.Errorf("ssh-src volume = %+v", vols["ssh-src"])
	}

	eph := sp
	eph.Persistent = false
	ephPod := podFor(eph, size, cfg, testInstance)
	if v := ephPod.Spec.Volumes[0]; v.PersistentVolumeClaim != nil || v.EmptyDir == nil || v.EmptyDir.SizeLimit.String() != "10Gi" {
		t.Errorf("an ephemeral machine's data volume = %+v", v)
	}
	// The disk-backed emptyDirs count against ephemeral-storage: the
	// limit must cover the data volume, /tmp and headroom.
	if got := ephPod.Spec.Containers[0].Resources.Limits.StorageEphemeral().String(); got != "12Gi" {
		t.Errorf("an ephemeral machine's ephemeral-storage limit = %s, want 12Gi", got)
	}
}

func TestSecretAndClaim(t *testing.T) {
	cfg, _ := parseConfig(baseConfig(t, nil))
	sp := spec("abc123")
	sec := secretFor(sp, testInstance, time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC))
	if sec.Name != "lx-abc123-ssh" || sec.Type != corev1.SecretTypeOpaque || string(sec.Data["host_ed25519"]) != sp.SSH.HostPrivateKey || string(sec.Data["authorized_keys"]) != sp.SSH.AuthorizedKey+"\n" {
		t.Errorf("secret = %+v", sec)
	}
	if strings.Contains(sec.Annotations[annotationSpec], "SECRETMATERIAL") || strings.Contains(sec.Annotations[annotationSpec], "AAAAC3NzaC1lZDI1NTE5AAAAIKey") {
		t.Error("the spec annotation carries key material")
	}
	back, err := specFrom(sec)
	if err != nil || back.ID != "abc123" || back.Size != "small" || !back.Persistent || back.Egress != "internet" || back.SSH.HostPrivateKey != "" {
		t.Errorf("specFrom = %+v, %v", back, err)
	}
	if got := createdFrom(sec, time.Now()); !got.Equal(time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("createdFrom = %v", got)
	}
	size, _ := cfg.size("medium")
	pvc := pvcFor(sp, size, cfg, testInstance)
	if pvc.Name != "lx-abc123-data" || *pvc.Spec.StorageClassName != "ceph-rbd-sc" || pvc.Spec.AccessModes[0] != corev1.ReadWriteOnce || pvc.Spec.Resources.Requests.Storage().String() != "20Gi" || pvc.Labels[protocol.LabelEnvironment] != "abc123" {
		t.Errorf("pvc = %+v", pvc)
	}
}

func TestStatusOf(t *testing.T) {
	ready := func(r bool) []corev1.ContainerStatus {
		return []corev1.ContainerStatus{{Name: "agent", Ready: r, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}, ImageID: "ghcr.io/loomux/agent@sha256:abc"}}
	}
	cases := []struct {
		name   string
		pod    corev1.Pod
		status string
		reason string
	}{
		{"running", corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: ready(true)}}, protocol.EnvRunning, ""},
		{"not ready", corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: ready(false)}}, protocol.EnvStarting, "sshd isn't answering yet"},
		{"pending", corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending}}, protocol.EnvCreating, "pending"},
		{"unschedulable", corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Message: "0/3 nodes are available"}}}}, protocol.EnvCreating, "unschedulable: 0/3 nodes are available"},
		{"image pull", corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{{Name: "agent", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "Back-off pulling image \"x\""}}}}}}, protocol.EnvError, "ImagePullBackOff: Back-off pulling image \"x\""},
		{"crash loop", corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: "agent", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}}}}}, protocol.EnvError, "CrashLoopBackOff: "},
		{"evicted", corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodFailed, Reason: "Evicted", Message: "The node was low on resource: ephemeral-storage."}}, protocol.EnvError, "Evicted: The node was low on resource: ephemeral-storage."},
		{"terminating", corev1.Pod{ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: &metav1.Time{Time: time.Now()}}, Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: ready(true)}}, protocol.EnvStopped, "the pod is stopping"},
		{"unknown", corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodUnknown}}, protocol.EnvError, "the pod's state is unknown (its node may be unreachable)"},
	}
	for _, c := range cases {
		status, reason := statusOf(&c.pod)
		if status != c.status || reason != c.reason {
			t.Errorf("%s: = %s %q, want %s %q", c.name, status, reason, c.status, c.reason)
		}
	}
	if d := digestOf(&cases[0].pod); d != "sha256:abc" {
		t.Errorf("digest = %q", d)
	}
}

// markRunning gives a pod the status the kubelet would once sshd answers.
func markRunning(t *testing.T, cs *fake.Clientset, name string) {
	t.Helper()
	pod, err := cs.CoreV1().Pods(testNS).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pod.Status.Phase = corev1.PodRunning
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "agent", Ready: true, RestartCount: 2, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}, ImageID: "ghcr.io/loomux/agent@sha256:deadbeef"}}
	// Through the tracker, not the clientset: the test isn't the plugin,
	// and the verb audit must see the plugin's calls only.
	if err := cs.Tracker().Update(corev1.SchemeGroupVersion.WithResource("pods"), pod, testNS); err != nil {
		t.Fatal(err)
	}
}

func TestLifecycle(t *testing.T) {
	cs := fake.NewClientset()
	p := configured(t, cs, map[string]any{"max_environments": 2.0})

	info, err := p.DescribeTargets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.Environments != 0 || info.MaxEnvironments != 2 || info.User != "agent" || info.Port != 2222 || info.SSHProxy != protocol.SSHProxyNone || info.Image != "ghcr.io/loomux/agent:test" || len(info.Sizes) != 3 || !info.PersistentDefault {
		t.Errorf("describe = %+v", info)
	}
	if len(info.EgressOptions) != 1 || info.EgressOptions[0] != protocol.EgressInternet {
		t.Errorf("before the enforcement check egress options = %v, want internet only", info.EgressOptions)
	}

	sp := spec("abc123")
	first, err := p.CreateTarget(ctx, sp)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if first.ID != "abc123" || first.Status != protocol.EnvCreating || first.Address.Host != "lx-abc123.loomux-agents.loomux-agents.svc.cluster.local" || first.Address.Port != 2222 || first.Address.Proxy != "none" || first.Size != "small" || !first.Persistent || first.CreatedAt.IsZero() {
		t.Errorf("created = %+v", first)
	}
	for _, name := range []string{"lx-abc123-ssh"} {
		if _, err := cs.CoreV1().Secrets(testNS).Get(ctx, name, metav1.GetOptions{}); err != nil {
			t.Errorf("secret %s: %v", name, err)
		}
	}
	if _, err := cs.CoreV1().PersistentVolumeClaims(testNS).Get(ctx, "lx-abc123-data", metav1.GetOptions{}); err != nil {
		t.Errorf("pvc: %v", err)
	}
	if _, err := cs.CoreV1().Pods(testNS).Get(ctx, "lx-abc123", metav1.GetOptions{}); err != nil {
		t.Errorf("pod: %v", err)
	}
	second, err := p.CreateTarget(ctx, sp)
	if err != nil || second.Address != first.Address || second.ID != first.ID {
		t.Errorf("create again = %+v, %v", second, err)
	}

	markRunning(t, cs, "lx-abc123")
	got, err := p.GetTarget(ctx, "abc123")
	if err != nil || got.Status != protocol.EnvRunning || got.ImageDigest != "sha256:deadbeef" {
		t.Errorf("get = %+v, %v", got, err)
	}
	h, err := p.TargetHealth(ctx, "abc123")
	if err != nil || h.Status != protocol.EnvRunning || h.Restarts != 2 || h.ImageDigest != "sha256:deadbeef" {
		t.Errorf("health = %+v, %v", h, err)
	}
	list, err := p.ListTargets(ctx)
	if err != nil || len(list) != 1 || list[0].ID != "abc123" || list[0].Status != protocol.EnvRunning {
		t.Errorf("list = %+v, %v", list, err)
	}
	if info, _ = p.DescribeTargets(ctx); info.Environments != 1 {
		t.Errorf("environments after create = %d", info.Environments)
	}

	// Stop: the pod goes, the record and the data stay.
	if err := p.StopTarget(ctx, "abc123"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := p.StopTarget(ctx, "abc123"); err != nil {
		t.Errorf("stop twice: %v", err)
	}
	if _, err := cs.CoreV1().Pods(testNS).Get(ctx, "lx-abc123", metav1.GetOptions{}); !kerrors.IsNotFound(err) {
		t.Errorf("pod after stop: %v", err)
	}
	if _, err := cs.CoreV1().PersistentVolumeClaims(testNS).Get(ctx, "lx-abc123-data", metav1.GetOptions{}); err != nil {
		t.Errorf("pvc after stop: %v", err)
	}
	if got, err := p.GetTarget(ctx, "abc123"); err != nil || got.Status != protocol.EnvStopped || got.Address != first.Address {
		t.Errorf("get after stop = %+v, %v", got, err)
	}
	if h, _ := p.TargetHealth(ctx, "abc123"); h.Status != protocol.EnvStopped {
		t.Errorf("health after stop = %+v", h)
	}
	if list, _ := p.ListTargets(ctx); len(list) != 1 || list[0].Status != protocol.EnvStopped {
		t.Errorf("list after stop = %+v", list)
	}

	// Start: the pod is made again from the record alone.
	if err := p.StartTarget(ctx, "abc123"); err != nil {
		t.Fatalf("start: %v", err)
	}
	pod, err := cs.CoreV1().Pods(testNS).Get(ctx, "lx-abc123", metav1.GetOptions{})
	if err != nil || pod.Spec.Volumes[0].PersistentVolumeClaim == nil || pod.Labels[protocol.LabelTarget] != "t-abc123" {
		t.Errorf("pod after start = %+v, %v", pod, err)
	}
	if err := p.StartTarget(ctx, "abc123"); err != nil {
		t.Errorf("start twice: %v", err)
	}

	// Recreate with another size: the data and the address stay.
	re := sp
	re.Size = "medium"
	re.Image = "ghcr.io/loomux/agent@sha256:1234"
	env, err := p.RecreateTarget(ctx, "abc123", re)
	if err != nil {
		t.Fatalf("recreate: %v", err)
	}
	if env.Address != first.Address || env.Size != "medium" {
		t.Errorf("recreated = %+v", env)
	}
	pod, _ = cs.CoreV1().Pods(testNS).Get(ctx, "lx-abc123", metav1.GetOptions{})
	if pod.Spec.Containers[0].Resources.Requests.Cpu().String() != "2" || pod.Spec.Containers[0].Image != "ghcr.io/loomux/agent@sha256:1234" {
		t.Errorf("pod after recreate = %+v", pod.Spec.Containers[0])
	}
	if got, _ := p.GetTarget(ctx, "abc123"); got.Size != "medium" || !got.CreatedAt.Equal(first.CreatedAt) {
		t.Errorf("the record didn't follow the recreate (or lost its creation time): %+v vs %v", got, first.CreatedAt)
	}
	// Every verb the plugin used is one the Role grants (design §8,
	// deploy/test/kind/10-rbac.yaml).
	granted := map[string]map[string]bool{
		"pods": {"create": true, "get": true, "list": true, "watch": true, "delete": true}, "pods/log": {"get": true},
		"persistentvolumeclaims": {"create": true, "get": true, "list": true, "delete": true},
		"secrets":                {"create": true, "get": true, "list": true, "update": true, "delete": true},
		"events":                 {"list": true}, "services": {"get": true},
	}
	for _, a := range cs.Actions() {
		res, verb := a.GetResource().Resource, a.GetVerb()
		if sub := a.GetSubresource(); sub != "" {
			res += "/" + sub
		}
		if !granted[res][verb] {
			t.Errorf("the plugin used %s %s, which the Role doesn't grant", verb, res)
		}
	}
	eph := re
	eph.Persistent = false
	if _, err := p.RecreateTarget(ctx, "abc123", eph); rpcCode(err) != rpc.CodeInvalidParams {
		t.Errorf("recreate to ephemeral: %v", err)
	}

	cmds, err := p.TargetAttachCommands(ctx, "abc123", "loomux-abc")
	if err != nil || len(cmds) != 1 || cmds[0].Via != "kubectl" || cmds[0].Command != "kubectl -n loomux-agents exec -it lx-abc123 -- tmux -L loomux attach -t loomux-abc" {
		t.Errorf("attach = %+v, %v", cmds, err)
	}
	if _, err := p.TargetAttachCommands(ctx, "abc123", "x; rm -rf /"); rpcCode(err) != rpc.CodeInvalidParams {
		t.Errorf("attach with a bad session: %v", err)
	}

	// Quota, and an ephemeral machine.
	eph2 := spec("eph2")
	eph2.Persistent = false
	if _, err := p.CreateTarget(ctx, eph2); err != nil {
		t.Fatalf("create ephemeral: %v", err)
	}
	if _, err := cs.CoreV1().PersistentVolumeClaims(testNS).Get(ctx, "lx-eph2-data", metav1.GetOptions{}); !kerrors.IsNotFound(err) {
		t.Error("an ephemeral machine got a claim")
	}
	if _, err := p.CreateTarget(ctx, spec("third")); rpcCode(err) != rpc.CodeQuota {
		t.Errorf("third machine of two: %v", err)
	}

	// Destroy: everything goes, twice is fine, then it's not found.
	if err := p.DestroyTarget(ctx, "abc123"); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if err := p.DestroyTarget(ctx, "abc123"); err != nil {
		t.Errorf("destroy twice: %v", err)
	}
	for _, check := range []func() error{
		func() error {
			_, err := cs.CoreV1().Pods(testNS).Get(ctx, "lx-abc123", metav1.GetOptions{})
			return err
		},
		func() error {
			_, err := cs.CoreV1().PersistentVolumeClaims(testNS).Get(ctx, "lx-abc123-data", metav1.GetOptions{})
			return err
		},
		func() error {
			_, err := cs.CoreV1().Secrets(testNS).Get(ctx, "lx-abc123-ssh", metav1.GetOptions{})
			return err
		},
	} {
		if err := check(); !kerrors.IsNotFound(err) {
			t.Errorf("after destroy: %v", err)
		}
	}
	if _, err := p.GetTarget(ctx, "abc123"); rpcCode(err) != rpc.CodeNotFound {
		t.Errorf("get after destroy: %v", err)
	}
	if err := p.StopTarget(ctx, "abc123"); rpcCode(err) != rpc.CodeNotFound {
		t.Errorf("stop after destroy: %v", err)
	}
	if _, err := p.GetTarget(ctx, "../etc"); rpcCode(err) != rpc.CodeNotFound {
		t.Errorf("get with a bad id: %v", err)
	}
}

func TestOtherInstanceIsInvisible(t *testing.T) {
	cs := fake.NewClientset()
	p := configured(t, cs, nil)
	other := spec("theirs")
	other.Labels[protocol.LabelInstance] = "99999999-0000-0000-0000-000000000000"
	if _, err := cs.CoreV1().Secrets(testNS).Create(ctx, secretFor(other, "99999999-0000-0000-0000-000000000000", time.Now()), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.CreateTarget(ctx, spec("mine")); err != nil {
		t.Fatal(err)
	}
	if list, _ := p.ListTargets(ctx); len(list) != 1 || list[0].ID != "mine" {
		t.Errorf("list = %+v", list)
	}
	if info, _ := p.DescribeTargets(ctx); info.Environments != 1 {
		t.Errorf("environments = %d", info.Environments)
	}
	if _, err := p.GetTarget(ctx, "theirs"); rpcCode(err) != rpc.CodeNotFound {
		t.Errorf("get theirs: %v", err)
	}
	if err := p.DestroyTarget(ctx, "theirs"); err != nil {
		t.Errorf("destroy theirs: %v", err)
	}
	if _, err := cs.CoreV1().Secrets(testNS).Get(ctx, "lx-theirs-ssh", metav1.GetOptions{}); err != nil {
		t.Error("destroy touched another instance's machine")
	}
	if _, err := p.CreateTarget(ctx, other); rpcCode(err) != rpc.CodeInvalidParams {
		t.Errorf("create over theirs: %v", err)
	}
}

func TestCreateRefusesBadSpecs(t *testing.T) {
	p := configured(t, fake.NewClientset(), nil)
	cases := map[string]func(s *protocol.EnvironmentSpec){
		"id":      func(s *protocol.EnvironmentSpec) { s.ID = "Bad_ID" },
		"id-long": func(s *protocol.EnvironmentSpec) { s.ID = strings.Repeat("a", 41) },
		"id-dash": func(s *protocol.EnvironmentSpec) { s.ID = "ends-with-" },
		"size":    func(s *protocol.EnvironmentSpec) { s.Size = "huge" },
		"egress":  func(s *protocol.EnvironmentSpec) { s.Egress = "lan" },
		"no-key":  func(s *protocol.EnvironmentSpec) { s.SSH.AuthorizedKey = "" },
		"image":   func(s *protocol.EnvironmentSpec) { s.Image = "x y" },
		"label":   func(s *protocol.EnvironmentSpec) { s.Labels["extra"] = "has space" },
		"target":  func(s *protocol.EnvironmentSpec) { s.TargetID = "a/b" },
		"instance": func(s *protocol.EnvironmentSpec) {
			s.Labels[protocol.LabelInstance] = "99999999-0000-0000-0000-000000000000"
		},
	}
	for name, mutate := range cases {
		s := spec("ok")
		mutate(&s)
		if _, err := p.CreateTarget(ctx, s); rpcCode(err) != rpc.CodeInvalidParams {
			t.Errorf("%s: want invalid_params, got %v", name, err)
		}
	}
}

func TestCheck(t *testing.T) {
	problem := func(res protocol.CheckResult, code string) *protocol.Problem {
		for i := range res.Problems {
			if res.Problems[i].Code == code {
				return &res.Problems[i]
			}
		}
		return nil
	}
	t.Run("enforced", func(t *testing.T) {
		cs := fake.NewClientset(&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "loomux-agents", Namespace: testNS}})
		canaryExits(cs, exitBlocked)
		p := configured(t, cs, nil)
		res, err := p.Check(ctx)
		if err != nil || !res.OK || len(res.Problems) != 0 {
			t.Fatalf("check = %+v, %v", res, err)
		}
		if info, _ := p.DescribeTargets(ctx); len(info.EgressOptions) != 2 {
			t.Errorf("egress options once enforced = %v", info.EgressOptions)
		}
		if pods, _ := cs.CoreV1().Pods(testNS).List(ctx, metav1.ListOptions{}); len(pods.Items) != 0 {
			t.Errorf("the canary wasn't deleted: %d pods", len(pods.Items))
		}
		if list, _ := p.ListTargets(ctx); len(list) != 0 {
			t.Errorf("the canary is listed as a machine: %+v", list)
		}
	})
	for name, code := range map[string]int32{"api reachable": exitAPIReachable, "internet reachable": exitNetReachable} {
		t.Run(name, func(t *testing.T) {
			cs := fake.NewClientset()
			canaryExits(cs, code)
			p := configured(t, cs, nil)
			res, _ := p.Check(ctx)
			pr := problem(res, ProblemNetworkPolicyNotEnforced)
			if !res.OK || pr == nil || pr.Severity != protocol.SeverityWarning {
				t.Fatalf("check = %+v", res)
			}
			if problem(res, ProblemServiceMissing) == nil {
				t.Error("no warning about the missing headless Service")
			}
			if info, _ := p.DescribeTargets(ctx); len(info.EgressOptions) != 1 || info.EgressOptions[0] != protocol.EgressInternet {
				t.Errorf("egress options when not enforced = %v", info.EgressOptions)
			}
		})
	}
	t.Run("required", func(t *testing.T) {
		cs := fake.NewClientset()
		canaryExits(cs, exitAPIReachable)
		p := configured(t, cs, map[string]any{"require_network_policy": true})
		res, _ := p.Check(ctx)
		if pr := problem(res, ProblemNetworkPolicyNotEnforced); res.OK || pr == nil || pr.Severity != protocol.SeverityError {
			t.Fatalf("check = %+v", res)
		}
	})
	t.Run("no network", func(t *testing.T) {
		cs := fake.NewClientset()
		canaryExits(cs, exitNoNetwork)
		p := configured(t, cs, nil)
		res, _ := p.Check(ctx)
		pr := problem(res, ProblemNetworkCheckFailed)
		if !res.OK || pr == nil || pr.Severity != protocol.SeverityWarning || !strings.Contains(pr.Message, "kube-dns") {
			t.Fatalf("check = %+v", res)
		}
		if problem(res, ProblemNetworkPolicyNotEnforced) != nil {
			t.Error("nothing reachable must not read as enforced or as not enforced")
		}
		if info, _ := p.DescribeTargets(ctx); len(info.EgressOptions) != 1 {
			t.Errorf("egress options with no verdict = %v", info.EgressOptions)
		}
	})
	t.Run("canary refused", func(t *testing.T) {
		cs := fake.NewClientset()
		cs.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, kerrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "x", errors.New("no"))
		})
		p := configured(t, cs, nil)
		res, _ := p.Check(ctx)
		if pr := problem(res, ProblemNetworkCheckFailed); !res.OK || pr == nil || pr.Severity != protocol.SeverityWarning || !strings.Contains(pr.Message, "canary") {
			t.Fatalf("check = %+v", res)
		}
	})
	t.Run("pending", func(t *testing.T) {
		cs := fake.NewClientset() // the canary never finishes: no kubelet
		p := configured(t, cs, nil)
		p.CanaryWait = 20 * time.Millisecond
		res, _ := p.Check(ctx)
		if pr := problem(res, ProblemNetworkCheckPending); !res.OK || pr == nil {
			t.Fatalf("check = %+v", res)
		}
		if info, _ := p.DescribeTargets(ctx); len(info.EgressOptions) != 1 {
			t.Errorf("egress options while pending = %v", info.EgressOptions)
		}
	})
	t.Run("service forbidden", func(t *testing.T) {
		cs := fake.NewClientset()
		cs.PrependReactor("get", "services", func(action k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, kerrors.NewForbidden(schema.GroupResource{Resource: "services"}, "loomux-agents", errors.New("no"))
		})
		canaryExits(cs, exitBlocked)
		p := configured(t, cs, nil)
		res, _ := p.Check(ctx)
		if pr := problem(res, ProblemServiceMissing); pr == nil || !strings.Contains(pr.Message, "services get") {
			t.Fatalf("a forbidden Service read must be reported, not read as present: %+v", res)
		}
	})
	t.Run("forbidden", func(t *testing.T) {
		cs := fake.NewClientset()
		cs.PrependReactor("list", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, kerrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", errors.New("no role"))
		})
		p := configured(t, cs, nil)
		res, _ := p.Check(ctx)
		if pr := problem(res, ProblemForbidden); res.OK || pr == nil || pr.Severity != protocol.SeverityError {
			t.Fatalf("check = %+v", res)
		}
	})
	t.Run("not configured", func(t *testing.T) {
		p := newTestPlugin(fake.NewClientset())
		if _, err := p.Check(ctx); rpcCode(err) != rpc.CodeUnavailable {
			t.Errorf("check before configure: %v", err)
		}
	})
}

func TestCanaryScript(t *testing.T) {
	s := canaryScript("10.96.0.1:443")
	for _, want := range []string{"probe 10.96.0.1 443 && exit 10", "probe kubernetes.default.svc.cluster.local 443 && exit 10", "probe 1.1.1.1 443 && exit 11", `probe "$dns" 53 || exit 12`, "exit 0"} {
		if !strings.Contains(s, want) {
			t.Errorf("script lacks %q:\n%s", want, s)
		}
	}
	pod := canaryPod("lx-netcheck-abcd", Config{AgentImage: "img"}, testInstance, "")
	if pod.Labels[labelRole] != "agent" || pod.Labels[labelEgress] != "none" || pod.Labels[labelCanary] != "true" || pod.Spec.RestartPolicy != corev1.RestartPolicyNever || *pod.Spec.AutomountServiceAccountToken || pod.Spec.SecurityContext.RunAsUser == nil || *pod.Spec.SecurityContext.RunAsUser != 10002 || len(pod.Spec.Volumes) != 0 {
		t.Errorf("canary pod = %+v", pod)
	}
	if apiHostPort("https://10.96.0.1:443") != "10.96.0.1:443" || apiHostPort("https://kube.example") != "kube.example:443" || apiHostPort("") != "" {
		t.Error("apiHostPort")
	}
}

// The plugin passes the host's conformance suite, served in-process
// over pipes against a fake cluster whose canary reports enforcement.
func TestConformance(t *testing.T) {
	plugintest.Run(t, func(t *testing.T) (*rpc.Conn, func()) {
		cs := fake.NewClientset()
		canaryExits(cs, exitBlocked)
		p := newTestPlugin(cs)
		hostR, pluginW := io.Pipe()
		pluginR, hostW := io.Pipe()
		sctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = sdk.Serve(sctx, p, pluginR, pluginW)
			_ = pluginW.Close()
		}()
		conn := rpc.NewConn(hostR, hostW, nil)
		return conn, func() {
			conn.Close()
			cancel()
			<-done
		}
	}, plugintest.Options{Config: map[string]any{"namespace": testNS, "storage_class": "standard", "agent_image": "ghcr.io/loomux/agent:test"}})
}
