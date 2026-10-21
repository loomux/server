### Security

- The Kubernetes plugin no longer certifies `egress: none` while a NetworkPolicy lets confined machines reach a destination its canary doesn't try (LOOM-201). Before, the check only tried the API server and `1.1.1.1:443`. One more additive policy allowing `169.254.169.254` or a LAN service passed it, and machines labelled `none` could reach that destination. `none` now also rests on the namespace's NetworkPolicies, read on every check, describe and create. Every policy that may select a confined machine must allow only kube-dns (`kube-system`, `k8s-app=kube-dns`, UDP and TCP 53), and one must isolate every such machine for egress. Otherwise check reports `network_policy_allows_egress`, naming the policy, and `none` is withdrawn. The canary also tries the metadata address and `github.com:443`, all of its probes at once.
- A verdict of "enforced" no longer lasts as long as the plugin process. Describe and create use it for 10 minutes. Describe never waits for the canary: past 5 minutes it starts one in the background to renew the verdict. Create, start and recreate refuse a `none` pod unless the policies and such a verdict both hold, and run the canary when there is none.

### Fixed

- Two Kubernetes machine creates at once can no longer both take the last place under `max_environments` (LOOM-201).

### Changed

- The Kubernetes plugin's Role needs `networkpolicies list` (`networking.k8s.io`) in the agents namespace (LOOM-201). Without it, check reports `network_policy_check_failed` and `egress: none` isn't offered. Grant it before upgrading a deployment that uses `none`; `deploy/test/kind/10-rbac.yaml` shows the rule.
