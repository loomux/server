package kubernetes

import (
	"context"
	"regexp"
	"sort"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/Loomux/server/plugins/protocol"
	"github.com/Loomux/server/plugins/rpc"
)

// The targets.* group (design §1.9, §8): create = Secret, PVC, Pod
// (get-or-create each); stop = delete the Pod; start = make it again;
// recreate = delete and make it with the new spec; destroy = delete all
// three, each tolerating 404. The Secret is the machine's record: while
// it exists the machine exists, stopped when its pod is gone.

func (p *Plugin) DescribeTargets(ctx context.Context) (protocol.TargetsInfo, error) {
	cfg, client, host, err := p.state()
	if err != nil {
		return protocol.TargetsInfo{}, err
	}
	secrets, err := p.listSecrets(ctx, client, cfg, host.InstanceID)
	if err != nil {
		return protocol.TargetsInfo{}, err
	}
	egress := []string{protocol.EgressInternet}
	if state, _ := p.verdict(); state == enforcementEnforced {
		egress = append(egress, protocol.EgressNone)
	}
	return protocol.TargetsInfo{
		Sizes:             cfg.Sizes,
		PersistentDefault: true,
		EgressOptions:     egress,
		Image:             cfg.AgentImage,
		MaxEnvironments:   cfg.MaxEnvironments,
		Environments:      len(secrets),
		AddressTemplate:   cfg.addressTemplate(),
		SSHProxy:          protocol.SSHProxyNone,
		User:              "agent",
		Port:              sshPort,
	}, nil
}

func (p *Plugin) CreateTarget(ctx context.Context, spec protocol.EnvironmentSpec) (protocol.Environment, error) {
	cfg, client, host, err := p.state()
	if err != nil {
		return protocol.Environment{}, err
	}
	if err := validateSpec(spec, cfg, host); err != nil {
		return protocol.Environment{}, err
	}
	size, _ := cfg.size(spec.Size)
	instance := host.InstanceID
	secrets := client.CoreV1().Secrets(cfg.Namespace)
	sec, err := secrets.Get(ctx, secretName(spec.ID), metav1.GetOptions{})
	switch {
	case kerrors.IsNotFound(err):
		existing, err := p.listSecrets(ctx, client, cfg, instance)
		if err != nil {
			return protocol.Environment{}, err
		}
		if len(existing) >= cfg.MaxEnvironments {
			return protocol.Environment{}, &rpc.Error{Code: rpc.CodeQuota, Message: "this plugin already has its " + itoa(cfg.MaxEnvironments) + " machines"}
		}
		sec, err = secrets.Create(ctx, secretFor(spec, instance, p.Now()), metav1.CreateOptions{})
		if kerrors.IsAlreadyExists(err) {
			sec, err = secrets.Get(ctx, secretName(spec.ID), metav1.GetOptions{})
		}
		if err != nil {
			return protocol.Environment{}, unavailable(err)
		}
	case err != nil:
		return protocol.Environment{}, unavailable(err)
	case sec.Labels[protocol.LabelInstance] != instance:
		return protocol.Environment{}, invalidParams("id %s belongs to another Loomux instance", spec.ID)
	}
	if spec.Persistent {
		pvcs := client.CoreV1().PersistentVolumeClaims(cfg.Namespace)
		if _, err := pvcs.Get(ctx, pvcName(spec.ID), metav1.GetOptions{}); kerrors.IsNotFound(err) {
			if _, err := pvcs.Create(ctx, pvcFor(spec, size, cfg, instance), metav1.CreateOptions{}); err != nil && !kerrors.IsAlreadyExists(err) {
				return protocol.Environment{}, unavailable(err)
			}
		} else if err != nil {
			return protocol.Environment{}, unavailable(err)
		}
	}
	pod, err := p.ensurePod(ctx, client, cfg, spec, size, instance)
	if err != nil {
		return protocol.Environment{}, err
	}
	return p.environment(pod, spec, cfg, sec), nil
}

// ensurePod is the machine's pod, made if it isn't there. One that is
// still terminating (a stop or recreate moments ago) is waited for.
func (p *Plugin) ensurePod(ctx context.Context, client kubernetes.Interface, cfg Config, spec protocol.EnvironmentSpec, size protocol.Size, instance string) (*corev1.Pod, error) {
	pods := client.CoreV1().Pods(cfg.Namespace)
	name := podName(spec.ID)
	for attempt := 0; ; attempt++ {
		pod, err := pods.Get(ctx, name, metav1.GetOptions{})
		switch {
		case err == nil && pod.DeletionTimestamp == nil:
			return pod, nil
		case err == nil:
			if err := p.waitGone(ctx, pods, name); err != nil {
				return nil, err
			}
		case !kerrors.IsNotFound(err):
			return nil, unavailable(err)
		}
		pod, err = pods.Create(ctx, podFor(spec, size, cfg, instance), metav1.CreateOptions{})
		if err == nil {
			return pod, nil
		}
		if !kerrors.IsAlreadyExists(err) || attempt >= 2 {
			return nil, unavailable(err)
		}
	}
}

// waitGone waits for a deleted pod to be gone, within DeleteWait.
func (p *Plugin) waitGone(ctx context.Context, pods podClient, name string) error {
	deadline := time.After(p.DeleteWait)
	for {
		_, err := pods.Get(ctx, name, metav1.GetOptions{})
		if kerrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return unavailable(err)
		}
		select {
		case <-ctx.Done():
			return &rpc.Error{Code: rpc.CodeUnavailable, Message: "the previous pod is still stopping; try again shortly"}
		case <-deadline:
			return &rpc.Error{Code: rpc.CodeUnavailable, Message: "the previous pod is still stopping; try again shortly"}
		case <-time.After(p.Poll):
		}
	}
}

type podClient interface {
	Get(ctx context.Context, name string, opts metav1.GetOptions) (*corev1.Pod, error)
}

// machine is one machine's Secret (its record) and pod, if any.
func (p *Plugin) machine(ctx context.Context, client kubernetes.Interface, cfg Config, host protocol.HostInfo, id string) (*corev1.Secret, protocol.EnvironmentSpec, *corev1.Pod, error) {
	if !idPattern.MatchString(id) {
		return nil, protocol.EnvironmentSpec{}, nil, notFound(id)
	}
	sec, err := client.CoreV1().Secrets(cfg.Namespace).Get(ctx, secretName(id), metav1.GetOptions{})
	if kerrors.IsNotFound(err) {
		return nil, protocol.EnvironmentSpec{}, nil, notFound(id)
	}
	if err != nil {
		return nil, protocol.EnvironmentSpec{}, nil, unavailable(err)
	}
	if sec.Labels[protocol.LabelInstance] != host.InstanceID {
		// Another server's machine: invisible to this one.
		return nil, protocol.EnvironmentSpec{}, nil, notFound(id)
	}
	spec, err := specFrom(sec)
	if err != nil {
		return nil, spec, nil, err
	}
	pod, err := client.CoreV1().Pods(cfg.Namespace).Get(ctx, podName(id), metav1.GetOptions{})
	if kerrors.IsNotFound(err) {
		pod = nil
	} else if err != nil {
		return nil, spec, nil, unavailable(err)
	}
	return sec, spec, pod, nil
}

// environment is a machine as the host sees it.
func (p *Plugin) environment(pod *corev1.Pod, spec protocol.EnvironmentSpec, cfg Config, sec *corev1.Secret) protocol.Environment {
	env := protocol.Environment{
		ID: spec.ID, Address: cfg.address(spec.ID), Size: spec.Size, Persistent: spec.Persistent, Egress: spec.Egress,
		CreatedAt: createdFrom(sec, p.Now()),
	}
	if pod == nil {
		env.Status, env.Reason = protocol.EnvStopped, ""
		return env
	}
	env.Status, env.Reason = statusOf(pod)
	env.ImageDigest = digestOf(pod)
	return env
}

func (p *Plugin) GetTarget(ctx context.Context, id string) (protocol.Environment, error) {
	cfg, client, host, err := p.state()
	if err != nil {
		return protocol.Environment{}, err
	}
	sec, spec, pod, err := p.machine(ctx, client, cfg, host, id)
	if err != nil {
		return protocol.Environment{}, err
	}
	return p.environment(pod, spec, cfg, sec), nil
}

// listSecrets is this server's machines' records.
func (p *Plugin) listSecrets(ctx context.Context, client kubernetes.Interface, cfg Config, instance string) ([]corev1.Secret, error) {
	list, err := client.CoreV1().Secrets(cfg.Namespace).List(ctx, metav1.ListOptions{LabelSelector: instanceSelector(instance)})
	if err != nil {
		return nil, unavailable(err)
	}
	return list.Items, nil
}

func (p *Plugin) ListTargets(ctx context.Context) ([]protocol.Environment, error) {
	cfg, client, host, err := p.state()
	if err != nil {
		return nil, err
	}
	secrets, err := p.listSecrets(ctx, client, cfg, host.InstanceID)
	if err != nil {
		return nil, err
	}
	podList, err := client.CoreV1().Pods(cfg.Namespace).List(ctx, metav1.ListOptions{LabelSelector: instanceSelector(host.InstanceID)})
	if err != nil {
		return nil, unavailable(err)
	}
	pods := map[string]*corev1.Pod{}
	for i := range podList.Items {
		pods[podList.Items[i].Name] = &podList.Items[i]
	}
	out := []protocol.Environment{}
	for i := range secrets {
		spec, err := specFrom(&secrets[i])
		if err != nil {
			p.Logger.Printf("skipping %s: %v", secrets[i].Name, err)
			continue
		}
		out = append(out, p.environment(pods[podName(spec.ID)], spec, cfg, &secrets[i]))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (p *Plugin) StartTarget(ctx context.Context, id string) error {
	cfg, client, host, err := p.state()
	if err != nil {
		return err
	}
	sec, spec, pod, err := p.machine(ctx, client, cfg, host, id)
	if err != nil {
		return err
	}
	if pod != nil && pod.DeletionTimestamp == nil {
		return nil
	}
	size, ok := cfg.size(spec.Size)
	if !ok {
		return invalidParams("the machine's size %q is no longer offered; recreate it with one of %s", spec.Size, cfg.sizeNames())
	}
	_, err = p.ensurePod(ctx, client, cfg, spec, size, sec.Labels[protocol.LabelInstance])
	return err
}

func (p *Plugin) StopTarget(ctx context.Context, id string) error {
	cfg, client, host, err := p.state()
	if err != nil {
		return err
	}
	if _, _, _, err := p.machine(ctx, client, cfg, host, id); err != nil {
		return err
	}
	return p.deletePod(ctx, client, cfg, id, 30)
}

func (p *Plugin) deletePod(ctx context.Context, client kubernetes.Interface, cfg Config, id string, grace int64) error {
	err := client.CoreV1().Pods(cfg.Namespace).Delete(ctx, podName(id), metav1.DeleteOptions{GracePeriodSeconds: ptr(grace)})
	if err != nil && !kerrors.IsNotFound(err) {
		return unavailable(err)
	}
	return nil
}

func (p *Plugin) RecreateTarget(ctx context.Context, id string, spec protocol.EnvironmentSpec) (protocol.Environment, error) {
	cfg, client, host, err := p.state()
	if err != nil {
		return protocol.Environment{}, err
	}
	if spec.ID == "" {
		spec.ID = id
	}
	if spec.ID != id {
		return protocol.Environment{}, invalidParams("the spec's id differs from the machine's")
	}
	if err := validateSpec(spec, cfg, host); err != nil {
		return protocol.Environment{}, err
	}
	sec, old, _, err := p.machine(ctx, client, cfg, host, id)
	if err != nil {
		return protocol.Environment{}, err
	}
	if spec.Persistent != old.Persistent {
		return protocol.Environment{}, invalidParams("a machine can't change between persistent and ephemeral")
	}
	size, _ := cfg.size(spec.Size)
	instance := sec.Labels[protocol.LabelInstance]
	// The record follows the new spec (secrets update, which the Role
	// grants); the creation time stays.
	fresh := secretFor(spec, instance, createdFrom(sec, p.Now()))
	sec.Labels, sec.Data = fresh.Labels, fresh.Data
	for k, v := range fresh.Annotations {
		sec.Annotations[k] = v
	}
	if sec, err = client.CoreV1().Secrets(cfg.Namespace).Update(ctx, sec, metav1.UpdateOptions{}); err != nil {
		return protocol.Environment{}, unavailable(err)
	}
	if err := p.deletePod(ctx, client, cfg, id, 5); err != nil {
		return protocol.Environment{}, err
	}
	pod, err := p.ensurePod(ctx, client, cfg, spec, size, instance)
	if err != nil {
		return protocol.Environment{}, err
	}
	return p.environment(pod, spec, cfg, sec), nil
}

func (p *Plugin) DestroyTarget(ctx context.Context, id string) error {
	cfg, client, host, err := p.state()
	if err != nil {
		return err
	}
	if !idPattern.MatchString(id) {
		return nil
	}
	sec, err := client.CoreV1().Secrets(cfg.Namespace).Get(ctx, secretName(id), metav1.GetOptions{})
	if kerrors.IsNotFound(err) {
		return nil // idempotent
	}
	if err != nil {
		return unavailable(err)
	}
	if sec.Labels[protocol.LabelInstance] != host.InstanceID {
		return nil // another server's: not ours to destroy
	}
	if err := p.deletePod(ctx, client, cfg, id, 5); err != nil {
		return err
	}
	if err := client.CoreV1().PersistentVolumeClaims(cfg.Namespace).Delete(ctx, pvcName(id), metav1.DeleteOptions{}); err != nil && !kerrors.IsNotFound(err) {
		return unavailable(err)
	}
	if err := client.CoreV1().Secrets(cfg.Namespace).Delete(ctx, secretName(id), metav1.DeleteOptions{}); err != nil && !kerrors.IsNotFound(err) {
		return unavailable(err)
	}
	return nil
}

func (p *Plugin) TargetHealth(ctx context.Context, id string) (protocol.EnvironmentHealth, error) {
	cfg, client, host, err := p.state()
	if err != nil {
		return protocol.EnvironmentHealth{}, err
	}
	_, _, pod, err := p.machine(ctx, client, cfg, host, id)
	if err != nil {
		return protocol.EnvironmentHealth{}, err
	}
	if pod == nil {
		return protocol.EnvironmentHealth{Status: protocol.EnvStopped}, nil
	}
	h := protocol.EnvironmentHealth{Restarts: restartsOf(pod), ImageDigest: digestOf(pod)}
	h.Status, h.Reason = statusOf(pod)
	if h.Status == protocol.EnvError || h.Status == protocol.EnvCreating {
		if ev := p.lastEvent(ctx, client, cfg, podName(id)); ev != "" {
			h.Reason = truncate(h.Reason+" ("+ev+")", 500)
		}
	}
	return h, nil
}

// lastEvent is the newest Event about the pod, in the cluster's words.
func (p *Plugin) lastEvent(ctx context.Context, client kubernetes.Interface, cfg Config, pod string) string {
	list, err := client.CoreV1().Events(cfg.Namespace).List(ctx, metav1.ListOptions{FieldSelector: "involvedObject.kind=Pod,involvedObject.name=" + pod})
	if err != nil || len(list.Items) == 0 {
		return ""
	}
	items := list.Items
	sort.Slice(items, func(i, j int) bool { return eventTime(items[i]).After(eventTime(items[j])) })
	return oneLine(items[0].Reason + ": " + items[0].Message)
}

func eventTime(e corev1.Event) time.Time {
	if !e.LastTimestamp.IsZero() {
		return e.LastTimestamp.Time
	}
	if !e.EventTime.IsZero() {
		return e.EventTime.Time
	}
	return e.CreationTimestamp.Time
}

var sessionPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

func (p *Plugin) TargetAttachCommands(ctx context.Context, id, session string) ([]protocol.AttachCommand, error) {
	cfg, client, host, err := p.state()
	if err != nil {
		return nil, err
	}
	if !sessionPattern.MatchString(session) {
		return nil, invalidParams("session must be letters, digits, dots, dashes and underscores")
	}
	if _, _, _, err := p.machine(ctx, client, cfg, host, id); err != nil {
		return nil, err
	}
	return []protocol.AttachCommand{{
		Via:     "kubectl",
		Command: "kubectl -n " + cfg.Namespace + " exec -it " + podName(id) + " -- tmux -L loomux attach -t " + session,
	}}, nil
}

func itoa(n int) string { return strconv.Itoa(n) }
