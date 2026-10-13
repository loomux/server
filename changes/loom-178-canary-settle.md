### Fixed

- The Kubernetes plugin's network-isolation check no longer reads an enforcing cluster as "not enforced" (LOOM-178). A policy engine confines a new pod only once it has learned its address (about 2 s with kube-network-policies), and the canary probed once, at start: it now tries a target that answers again, 15 times a second apart, and only one still answering counts.
- A transient API error while the canary runs (etcd `leader changed`) no longer fails the check; it is reported only if it lasts as long as the canary. Canaries left behind by a failed delete are removed by the next check, and the canary's deadline now covers a slow image pull.
