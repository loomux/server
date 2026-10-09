#!/bin/sh
# Prints a kubeconfig for the plugin's ServiceAccount (10-rbac.yaml), so
# the integration test runs with exactly the Role's verbs and nothing
# more: the cluster and CA from the current context, a token for the
# ServiceAccount. Usage: kubeconfig-for-sa.sh [namespace] > plugin.kubeconfig
set -eu
ns="${1:-loomux-agents}"
server="$(kubectl config view --minify --raw -o jsonpath='{.clusters[0].cluster.server}')"
ca="$(kubectl config view --minify --raw -o jsonpath='{.clusters[0].cluster.certificate-authority-data}')"
token="$(kubectl -n "${ns}" create token loomux-plugin-kubernetes --duration=2h)"
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
