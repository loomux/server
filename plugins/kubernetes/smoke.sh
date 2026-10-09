#!/usr/bin/env bash
# Smoke test for the plugin image: start it the way the host would
# (stdio), send plugin.describe, and expect the manifest back. Usage:
#   bash plugins/kubernetes/smoke.sh <image>
set -euo pipefail
IMAGE="${1:?usage: smoke.sh <image>}"
req='{"jsonrpc":"2.0","id":1,"method":"plugin.describe"}'
out="$(printf 'Content-Length: %d\r\n\r\n%s' "${#req}" "${req}" | timeout 20 docker run -i --rm --read-only --cap-drop ALL --security-opt no-new-privileges:true --user 10003:10001 "${IMAGE}" | head -c 4096 || true)"
echo "${out}" | grep -q '"name":"kubernetes"' || { echo "no manifest in the plugin's answer: ${out}" >&2; exit 1; }
echo "${out}" | grep -q '"capabilities"' || { echo "manifest lacks capabilities: ${out}" >&2; exit 1; }
docker run --rm --entrypoint /loomux-plugin-kubernetes "${IMAGE}" --listen /tmp/x.sock </dev/null >/dev/null 2>&1 & pid=$!
sleep 2; kill "${pid}" 2>/dev/null || true; wait "${pid}" 2>/dev/null || true
echo "plugin image ok"
