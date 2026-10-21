package kubernetes

import (
	"context"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"

	"github.com/Loomux/server/plugins/protocol"
)

// The deny-all check (LOOM-201). The canary shows that the engine
// enforces policy at all; it can't show that no policy lets a confined
// machine out to a destination it didn't try (the metadata address, a
// LAN service), and NetworkPolicies are additive. So egress "none" also
// rests on the policies themselves, read on every check, describe and
// create: every NetworkPolicy of the namespace that may select an
// egress "none" machine allows it §10's (c) and nothing else, kube-dns
// pods in kube-system on port 53; and one that selects every such
// machine isolates it for egress.
//
// What this trusts: that the engine implements NetworkPolicy as
// specified on every node. The canary samples that it enforces it, on
// the one node it lands on. Policy APIs the plugin's namespaced Role
// can't read can allow what NetworkPolicy denies: AdminNetworkPolicy,
// Calico's and Cilium's own kinds. Nothing of theirs may let agent pods
// out; the canary's destinations are the backstop for those alone.
// Whether a pod's traffic to its own node is policed is up to the
// engine. And policies are live: one an admin adds after a machine's
// pod is made applies to it at once, and only the next check sees it.

// nsNameLabel is the label the API server gives every namespace.
const nsNameLabel = "kubernetes.io/metadata.name"

// policyVerdict is enforced when the namespace's NetworkPolicies deny
// an egress "none" machine of instance all but kube-dns; otherwise
// which one doesn't (enforcementPolicyOpen), or why they couldn't be
// read (enforcementFailed).
func policyVerdict(ctx context.Context, client kubernetes.Interface, namespace, instance string) (enforcementState, string) {
	list, err := client.NetworkingV1().NetworkPolicies(namespace).List(ctx, metav1.ListOptions{})
	switch {
	case kerrors.IsForbidden(err) || kerrors.IsUnauthorized(err):
		return enforcementFailed, "couldn't list NetworkPolicies in " + namespace + " (the Role lacks networkpolicies list): " + safeStatus(err)
	case err != nil:
		return enforcementFailed, "couldn't list NetworkPolicies in " + namespace + ": " + safeStatus(err)
	}
	isolated := false
	for _, np := range list.Items {
		m := selectsConfined(np.Spec.PodSelector, instance)
		if m == matchNo || !isolatesEgress(np.Spec) {
			continue
		}
		if m == matchYes {
			isolated = true
		}
		for _, rule := range np.Spec.Egress {
			if what := beyondDNS(rule); what != "" {
				return enforcementPolicyOpen, fmt.Sprintf("NetworkPolicy %q lets a confined machine out %s", np.Name, what)
			}
		}
	}
	if !isolated {
		return enforcementPolicyOpen, "no NetworkPolicy in " + namespace + " isolates every confined machine for egress (§10's default deny for " + labelRole + "=" + roleAgent + ")"
	}
	return enforcementEnforced, ""
}

// match is whether a selector selects every confined machine (yes),
// perhaps some (maybe), or none (no).
type match int

const (
	matchNo match = iota
	matchMaybe
	matchYes
)

// selectsConfined is how sel selects the egress "none" machines of
// instance. Their labels are labelsFor's: these four known, the target
// and environment ids present with any value, no others.
func selectsConfined(sel metav1.LabelSelector, instance string) match {
	known := map[string]string{labelManagedBy: managedBy, protocol.LabelInstance: instance, labelRole: roleAgent, labelEgress: protocol.EgressNone}
	reqs := append([]metav1.LabelSelectorRequirement(nil), sel.MatchExpressions...)
	for k, v := range sel.MatchLabels {
		reqs = append(reqs, metav1.LabelSelectorRequirement{Key: k, Operator: metav1.LabelSelectorOpIn, Values: []string{v}})
	}
	m := matchYes
	for _, r := range reqs {
		m = min(m, requirementOn(r, known))
	}
	return m
}

func requirementOn(r metav1.LabelSelectorRequirement, known map[string]string) match {
	if r.Key == protocol.LabelTarget || r.Key == protocol.LabelEnvironment {
		switch r.Operator {
		case metav1.LabelSelectorOpExists:
			return matchYes
		case metav1.LabelSelectorOpDoesNotExist:
			return matchNo
		}
		return matchMaybe // In, NotIn: it depends on the machine
	}
	v, present := known[r.Key]
	var ok bool
	switch r.Operator {
	case metav1.LabelSelectorOpIn:
		ok = present && slices.Contains(r.Values, v)
	case metav1.LabelSelectorOpNotIn:
		ok = !present || !slices.Contains(r.Values, v)
	case metav1.LabelSelectorOpExists:
		ok = present
	case metav1.LabelSelectorOpDoesNotExist:
		ok = !present
	default:
		return matchMaybe
	}
	if ok {
		return matchYes
	}
	return matchNo
}

// isolatesEgress is whether a policy is an egress policy: it says so,
// or says nothing and has egress rules (the API's default).
func isolatesEgress(spec networkingv1.NetworkPolicySpec) bool {
	if len(spec.PolicyTypes) == 0 {
		return len(spec.Egress) > 0
	}
	return slices.Contains(spec.PolicyTypes, networkingv1.PolicyTypeEgress)
}

// beyondDNS is what an egress rule allows beyond kube-dns pods in
// kube-system on port 53 over UDP or TCP, in words; empty when nothing.
func beyondDNS(rule networkingv1.NetworkPolicyEgressRule) string {
	if len(rule.To) == 0 {
		return "to any destination"
	}
	for _, peer := range rule.To {
		if peer.IPBlock != nil {
			return "to " + peer.IPBlock.CIDR
		}
		if peer.NamespaceSelector == nil || !requires(*peer.NamespaceSelector, nsNameLabel, "kube-system") ||
			peer.PodSelector == nil || !requires(*peer.PodSelector, "k8s-app", "kube-dns") {
			return "to pods other than kube-dns in kube-system"
		}
	}
	if len(rule.Ports) == 0 {
		return "to kube-dns on any port"
	}
	for _, p := range rule.Ports {
		tcpOrUDP := p.Protocol == nil || *p.Protocol == corev1.ProtocolTCP || *p.Protocol == corev1.ProtocolUDP
		if !tcpOrUDP || p.Port == nil || p.Port.Type != intstr.Int || p.Port.IntVal != 53 || (p.EndPort != nil && *p.EndPort != 53) {
			return "to kube-dns beyond UDP and TCP port 53"
		}
	}
	return ""
}

// requires is whether every object sel selects has label key=value.
func requires(sel metav1.LabelSelector, key, value string) bool {
	if v, ok := sel.MatchLabels[key]; ok && v == value {
		return true
	}
	for _, r := range sel.MatchExpressions {
		if r.Key == key && r.Operator == metav1.LabelSelectorOpIn && len(r.Values) > 0 {
			all := true
			for _, v := range r.Values {
				all = all && v == value
			}
			if all {
				return true
			}
		}
	}
	return false
}
