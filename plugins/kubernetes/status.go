package kubernetes

import (
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/Loomux/server/plugins/protocol"
)

// imageErrors are the waiting reasons that never resolve on their own.
var imageErrors = map[string]bool{
	"ErrImagePull": true, "ImagePullBackOff": true, "InvalidImageName": true,
	"CreateContainerConfigError": true, "CreateContainerError": true, "CrashLoopBackOff": true,
}

// statusOf maps a pod to an environment status and the plugin's words
// for it (design §8: "reported in status_reason in the plugin's words").
// A terminating pod is stopped: the caller knows whether a stop or a
// destroy is under way.
func statusOf(pod *corev1.Pod) (status, reason string) {
	if pod.DeletionTimestamp != nil {
		return protocol.EnvStopped, "the pod is stopping"
	}
	var cs *corev1.ContainerStatus
	for i := range pod.Status.ContainerStatuses {
		if pod.Status.ContainerStatuses[i].Name == "agent" {
			cs = &pod.Status.ContainerStatuses[i]
		}
	}
	if cs != nil && cs.State.Waiting != nil && imageErrors[cs.State.Waiting.Reason] {
		return protocol.EnvError, cs.State.Waiting.Reason + ": " + oneLine(cs.State.Waiting.Message)
	}
	switch pod.Status.Phase {
	case corev1.PodRunning:
		if cs != nil && cs.Ready {
			return protocol.EnvRunning, ""
		}
		return protocol.EnvStarting, "sshd isn't answering yet"
	case corev1.PodPending:
		for _, c := range pod.Status.Conditions {
			if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse {
				return protocol.EnvCreating, "unschedulable: " + oneLine(c.Message)
			}
		}
		if cs != nil && cs.State.Waiting != nil {
			return protocol.EnvCreating, cs.State.Waiting.Reason
		}
		return protocol.EnvCreating, "pending"
	case corev1.PodFailed:
		msg := pod.Status.Reason
		if pod.Status.Message != "" {
			msg += ": " + oneLine(pod.Status.Message)
		}
		if cs != nil && cs.State.Terminated != nil && msg == "" {
			msg = cs.State.Terminated.Reason
		}
		return protocol.EnvError, strings.TrimPrefix(msg, ": ")
	case corev1.PodSucceeded:
		return protocol.EnvError, "sshd exited"
	case corev1.PodUnknown:
		return protocol.EnvError, "the pod's state is unknown (its node may be unreachable)"
	}
	return protocol.EnvCreating, ""
}

// digestOf is the running image's digest, if the kubelet reports one.
func digestOf(pod *corev1.Pod) string {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name != "agent" {
			continue
		}
		if i := strings.Index(cs.ImageID, "sha256:"); i >= 0 {
			return cs.ImageID[i:]
		}
	}
	return ""
}

func restartsOf(pod *corev1.Pod) int {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == "agent" {
			return int(cs.RestartCount)
		}
	}
	return 0
}

func oneLine(s string) string {
	return truncate(strings.Join(strings.Fields(s), " "), 300)
}
