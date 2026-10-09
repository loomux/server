package kubernetes

import (
	"encoding/json"
	"regexp"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/Loomux/server/plugins/protocol"
	"github.com/Loomux/server/plugins/rpc"
)

// Labels and annotations (design §8). Every object the plugin makes
// carries them, so it only ever touches its own, and the §10
// NetworkPolicies select on role and egress.
const (
	managedBy         = "loomux-plugin-kubernetes"
	labelManagedBy    = "app.kubernetes.io/managed-by"
	labelRole         = "loomux.io/role"
	labelEgress       = "loomux.io/egress"
	labelCanary       = "loomux.io/canary"
	roleAgent         = "agent"
	annotationName    = "loomux.io/target-name"
	annotationSpec    = "loomux.io/spec"
	annotationCreated = "loomux.io/created-at"

	managedSelector = labelManagedBy + "=" + managedBy

	agentUID = int64(10002)
	sshPort  = 2222
)

var (
	// idPattern: lx-<id> must be a DNS label, so an id starts and ends
	// alphanumeric.
	idPattern    = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)
	labelPattern = regexp.MustCompile(`^(([A-Za-z0-9][-A-Za-z0-9_.]{0,61})?[A-Za-z0-9])?$`)
)

func podName(id string) string    { return "lx-" + id }
func secretName(id string) string { return "lx-" + id + "-ssh" }
func pvcName(id string) string    { return "lx-" + id + "-data" }

// labelsFor are the labels of every object of one machine.
func labelsFor(spec protocol.EnvironmentSpec, instance string) map[string]string {
	return map[string]string{
		labelManagedBy:            managedBy,
		protocol.LabelInstance:    instance,
		protocol.LabelTarget:      spec.TargetID,
		protocol.LabelEnvironment: spec.ID,
		labelRole:                 roleAgent,
		labelEgress:               spec.Egress,
	}
}

// instanceSelector lists one server's machines.
func instanceSelector(instance string) string {
	return managedSelector + "," + protocol.LabelInstance + "=" + instance
}

// cleanText is user text fit for an annotation: one line, bounded.
func cleanText(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	return truncate(s, 200)
}

// validateSpec refuses what can't become object names or labels, and a
// spec that claims another instance: the host's id from configure is
// the authority.
func validateSpec(spec protocol.EnvironmentSpec, cfg Config, host protocol.HostInfo) error {
	if !idPattern.MatchString(spec.ID) {
		return invalidParams("id must be lower-case letters, digits and dashes, at most 40, not ending in a dash")
	}
	if !labelPattern.MatchString(spec.TargetID) {
		return invalidParams("target_id isn't a label value")
	}
	if _, ok := cfg.size(spec.Size); !ok {
		return invalidParams("size %q isn't offered; one of %s", spec.Size, cfg.sizeNames())
	}
	switch spec.Egress {
	case protocol.EgressInternet, protocol.EgressNone:
	default:
		return invalidParams("egress must be internet or none")
	}
	if spec.Image != "" && strings.ContainsAny(spec.Image, " \t\r\n\"'`$") {
		return invalidParams("image must be an image reference")
	}
	if strings.TrimSpace(spec.SSH.AuthorizedKey) == "" || strings.TrimSpace(spec.SSH.HostPrivateKey) == "" {
		return invalidParams("ssh.authorized_key and ssh.host_private_key are required")
	}
	for k, v := range spec.Labels {
		if !labelPattern.MatchString(v) {
			return invalidParams("label %s isn't a label value", k)
		}
	}
	if v := spec.Labels[protocol.LabelInstance]; v != "" && v != host.InstanceID {
		return invalidParams("the spec is labelled for another Loomux instance")
	}
	if !labelPattern.MatchString(host.InstanceID) || host.InstanceID == "" {
		return invalidParams("the host's instance id isn't a label value")
	}
	return nil
}

// storedSpec is what the Secret remembers of the spec, so start and
// recreate can make the pod again from the id alone: everything but the
// key material, which is the Secret's data.
func storedSpec(spec protocol.EnvironmentSpec) protocol.EnvironmentSpec {
	spec.SSH = protocol.SSHBootstrap{Port: spec.SSH.Port}
	return spec
}

// secretFor is the machine's Secret: sshd's two files, mounted
// read-only at /etc/loomux/ssh-src and copied by the image's
// entrypoint; and the spec, as its annotation.
func secretFor(spec protocol.EnvironmentSpec, instance string, now time.Time) *corev1.Secret {
	js, _ := json.Marshal(storedSpec(spec))
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:   secretName(spec.ID),
			Labels: labelsFor(spec, instance),
			Annotations: map[string]string{
				annotationName:    cleanText(spec.Name),
				annotationSpec:    string(js),
				annotationCreated: now.UTC().Format(time.RFC3339),
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"host_ed25519":    []byte(spec.SSH.HostPrivateKey),
			"authorized_keys": []byte(strings.TrimSpace(spec.SSH.AuthorizedKey) + "\n"),
		},
	}
}

// specFrom is the spec a Secret remembers.
func specFrom(sec *corev1.Secret) (protocol.EnvironmentSpec, error) {
	var spec protocol.EnvironmentSpec
	if err := json.Unmarshal([]byte(sec.Annotations[annotationSpec]), &spec); err != nil || spec.ID == "" {
		return spec, &rpc.Error{Code: rpc.CodeInternal, Message: "the machine's record on the cluster is damaged (secret " + sec.Name + ")"}
	}
	return spec, nil
}

func createdFrom(sec *corev1.Secret, fallback time.Time) time.Time {
	if t, err := time.Parse(time.RFC3339, sec.Annotations[annotationCreated]); err == nil {
		return t
	}
	if !sec.CreationTimestamp.IsZero() {
		return sec.CreationTimestamp.Time
	}
	return fallback
}

// pvcFor is a persistent machine's data volume.
func pvcFor(spec protocol.EnvironmentSpec, size protocol.Size, cfg Config, instance string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: pvcName(spec.ID), Labels: labelsFor(spec, instance),
			Annotations: map[string]string{annotationName: cleanText(spec.Name)},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: ptr(cfg.StorageClass),
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(size.Disk)},
			},
		},
	}
}

// securityContext is what every pod the plugin makes runs under: Pod
// Security "restricted".
func podSecurity() *corev1.PodSecurityContext {
	return &corev1.PodSecurityContext{
		RunAsNonRoot: ptr(true), RunAsUser: ptr(agentUID), RunAsGroup: ptr(agentUID), FSGroup: ptr(agentUID),
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

func containerSecurity() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: ptr(false), ReadOnlyRootFilesystem: ptr(true),
		Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
}

// podFor is the machine's pod, field by field as design §8 has it.
func podFor(spec protocol.EnvironmentSpec, size protocol.Size, cfg Config, instance string) *corev1.Pod {
	image := spec.Image
	if image == "" {
		image = cfg.AgentImage
	}
	data := corev1.Volume{Name: "data"}
	// The kubelet counts every disk-backed emptyDir and the writable
	// layer against the container's ephemeral-storage limit: /tmp (1Gi)
	// always, and the data volume of an ephemeral machine (a PVC
	// doesn't count). 1Gi of headroom on top, or the machine would be
	// evicted at a fraction of its own disk.
	ephemeral := resource.MustParse("2Gi")
	if spec.Persistent {
		data.PersistentVolumeClaim = &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvcName(spec.ID)}
	} else {
		data.EmptyDir = &corev1.EmptyDirVolumeSource{SizeLimit: ptr(resource.MustParse(size.Disk))}
		ephemeral = resource.MustParse(size.Disk)
		ephemeral.Add(resource.MustParse("2Gi"))
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: podName(spec.ID), Labels: labelsFor(spec, instance),
			Annotations: map[string]string{annotationName: cleanText(spec.Name)},
		},
		Spec: corev1.PodSpec{
			Hostname:                     podName(spec.ID),
			Subdomain:                    cfg.Subdomain,
			AutomountServiceAccountToken: ptr(false),
			EnableServiceLinks:           ptr(false),
			RestartPolicy:                corev1.RestartPolicyAlways,
			SecurityContext:              podSecurity(),
			Containers: []corev1.Container{{
				Name:  "agent",
				Image: image,
				Env: []corev1.EnvVar{
					{Name: "HOME", Value: "/data/home"},
					{Name: "CLAUDE_CONFIG_DIR", Value: "/data/home/.claude"},
					{Name: "TZ", Value: cfg.Timezone},
				},
				Ports: []corev1.ContainerPort{{Name: "ssh", ContainerPort: sshPort}},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(size.CPU), corev1.ResourceMemory: resource.MustParse(size.Memory)},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU: resource.MustParse(size.CPU), corev1.ResourceMemory: resource.MustParse(size.Memory),
						corev1.ResourceEphemeralStorage: ephemeral,
					},
				},
				SecurityContext: containerSecurity(),
				ReadinessProbe: &corev1.Probe{
					ProbeHandler:  corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromString("ssh")}},
					PeriodSeconds: 5,
				},
				VolumeMounts: []corev1.VolumeMount{
					{Name: "data", MountPath: "/data"},
					{Name: "tmp", MountPath: "/tmp"},
					{Name: "run", MountPath: "/run/loomux"},
					{Name: "ssh-src", MountPath: "/etc/loomux/ssh-src", ReadOnly: true},
				},
			}},
			Volumes: []corev1.Volume{
				data,
				{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: ptr(resource.MustParse("1Gi"))}}},
				{Name: "run", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory, SizeLimit: ptr(resource.MustParse("1Mi"))}}},
				{Name: "ssh-src", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: secretName(spec.ID), DefaultMode: ptr(int32(0o400))}}},
			},
		},
	}
}
