#!/bin/sh
# Prints a kubeconfig for the plugin's ServiceAccount (10-rbac.yaml), so
# the integration test runs with exactly the Role's verbs and nothing
# more: the cluster and CA from kind's context, a token for the
# ServiceAccount. Usage: kubeconfig-for-sa.sh [namespace [context]] > plugin.kubeconfig
# The context (kind-loomux, kind's for cluster/kind.yaml) is named on
# every kubectl, never the current one, which can be a real cluster.
set -eu
ns="${1:-loomux-agents}"
context="${2:-kind-loomux}"
server="$(kubectl --context "${context}" config view --minify --raw -o jsonpath='{.clusters[0].cluster.server}')"
ca="$(kubectl --context "${context}" config view --minify --raw -o jsonpath='{.clusters[0].cluster.certificate-authority-data}')"
token="$(kubectl --context "${context}" -n "${ns}" create token loomux-plugin-kubernetes --duration=2h)"
cat <<YAML
apiVersion: v1
kind: Config
clusters:
  - name: kind
    cluster:
      server: ${server}
      certificate-authority-data: ${ca}
users:
  - name: loomux-plugin-kubernetes
    user:
      token: ${token}
contexts:
  - name: plugin
    context:
      cluster: kind
      namespace: ${ns}
      user: loomux-plugin-kubernetes
current-context: plugin
YAML
