package kubernetes

import (
	"context"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/Loomux/server/plugins/protocol"
)

// Egress "trusted" (LOOM-226): the opt-in network class for the
// machines an admin names in trusted_machines, such as a hub that
// dispatches to the LAN's hosts. Its pods are labelled
// loomux.io/egress=trusted, which §10's internet policy (d) doesn't
// select and the deny-all check for "none" ignores; what lets them out
// is the cluster's own policy (e), applied by its admin:
//
//	podSelector: {loomux.io/role: agent, loomux.io/egress: trusted}
//	egress: to 0.0.0.0/0 except the pod CIDR, the service CIDR,
//	        169.254.0.0/16 and 127.0.0.0/8
//
// So a trusted machine reaches the internet, the rest of RFC 1918 (the
// LAN, its VIP and hosts, and the cluster's nodes with the API server's
// and kubelets' ports) and the tailnet's 100.64.0.0/10, and not other
// pods, Services or the metadata address. The plugin makes no
// NetworkPolicy (its Role can't): without (e) a trusted machine reaches
// kube-dns only, and check says so.
//
// Three locks, each an admin's: the cluster's policy (e), the plugin's
// trusted_machines, and the egress asked for at create, which the host
// never defaults to and records on the machine. Taking a name out of
// trusted_machines withdraws it: the machine is in error and can't be
// started or recreated until it is destroyed or named again; deleting
// (e) cuts its LAN and tailnet at once.

// ProblemTrustedPolicyMissing (warning): trusted_machines names
// machines, but no NetworkPolicy lets egress "trusted" machines out.
const ProblemTrustedPolicyMissing = "trusted_egress_policy_missing"

// trustRefused is the error for a trusted spec its name doesn't allow.
func trustRefused(cfg Config, spec protocol.EnvironmentSpec) error {
	if spec.Egress != protocol.EgressTrusted || cfg.trusted(spec.Name) {
		return nil
	}
	return invalidParams("egress trusted is only for the machines the plugin's trusted_machines names (an admin's setting), and %q isn't one", truncate(spec.Name, 64))
}

// trustGate is a trusted machine's status once its name is no longer
// in trusted_machines: error, the pod left as it is, like any machine
// in error.
func trustGate(cfg Config, spec protocol.EnvironmentSpec, status, reason string) (string, string) {
	if err := trustRefused(cfg, spec); err != nil {
		return protocol.EnvError, "egress trusted withdrawn: the plugin's trusted_machines no longer names this machine; destroy it, or name it again"
	}
	return status, reason
}

// trustedPolicyProblem is check's word on trusted machines: when
// trusted_machines names any, a NetworkPolicy that selects every one of
// them must let them out (§10's (e)). Nil when it does, when none are
// named, or when the policies can't be read (the deny-all check reports
// that).
func trustedPolicyProblem(ctx context.Context, client kubernetes.Interface, cfg Config, instance string) *protocol.Problem {
	if len(cfg.TrustedMachines) == 0 {
		return nil
	}
	list, err := client.NetworkingV1().NetworkPolicies(cfg.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil
	}
	for _, np := range list.Items {
		if selectsMachines(np.Spec.PodSelector, instance, protocol.EgressTrusted) != matchYes || !isolatesEgress(np.Spec) {
			continue
		}
		for _, rule := range np.Spec.Egress {
			if beyondDNS(rule) != "" {
				return nil
			}
		}
	}
	return &protocol.Problem{
		Code: ProblemTrustedPolicyMissing, Severity: protocol.SeverityWarning,
		Message: "trusted_machines names machines, but no NetworkPolicy in " + cfg.Namespace + " lets " + labelEgress + "=" + protocol.EgressTrusted +
			" machines out: they reach kube-dns only; apply design §10's policy (e)",
	}
}

// trustChanged tells the host of each trusted machine a configure named
// or stopped naming (LOOM-226), so its status follows at once rather
// than at the host's next reconcile (targets.changed, LOOM-213).
func (p *Plugin) trustChanged(prev, cfg Config, client kubernetes.Interface, instance string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	secrets, err := p.listSecrets(ctx, client, cfg, instance)
	if err != nil {
		return // the host's reconcile sees it
	}
	for i := range secrets {
		spec, err := specFrom(&secrets[i])
		if err != nil || spec.Egress != protocol.EgressTrusted || prev.trusted(spec.Name) == cfg.trusted(spec.Name) {
			continue
		}
		p.notifier.TargetsChanged(spec.ID, "")
	}
}
