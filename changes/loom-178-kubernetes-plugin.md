### Added

- The Kubernetes target-provider plugin (LOOM-179, `plugins/kubernetes`):
  a pod per machine in one namespace, restricted-compliant, with a
  Secret for sshd's files and a PersistentVolumeClaim for its data,
  reached through a headless Service. It tests whether NetworkPolicy
  is enforced with a canary pod and offers `egress: none` only when it
  is; `require_network_policy` refuses machines otherwise. Bundled in
  the server image and published as `ghcr.io/loomux/plugin-kubernetes`
  (the sidecar form). CI runs it against a kind cluster with the
  manifests of `deploy/test/kind/`.
