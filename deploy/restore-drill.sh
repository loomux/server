#!/usr/bin/env bash
# Restore drill (LOOM-127, docs/deploy/backup-restore.md): proves the newest
# nightly backup restores, without touching the running instance. It
#
#   1. reads the newest backup through a short-lived pod that mounts the
#      backup PVC read-only, checks its integrity and copies it out;
#   2. runs the given loomuxd image on that copy, locally, with throwaway
#      credentials (a random drill password and vault key, and a router
#      endpoint that's never called);
#   3. checks it migrated, is healthy, logs in, and serves the targets,
#      workspaces and conversations; then prints a row for the runbook's
#      drill record.
#
# It never suspends Flux and never writes to the live database. The copy
# holds conversations: it lives in a private temporary directory, deleted
# on exit (--keep leaves it, still private).
#
#   deploy/restore-drill.sh --image ghcr.io/loomux/server:<sha> \
#       [--context admin@theWyseKube] [--namespace loomux] [--port 18080] [--keep]
set -euo pipefail

image="" context="" namespace="loomux" port=18080 keep=false
while [ $# -gt 0 ]; do
  case "$1" in
  --image) image="$2"; shift 2 ;;
  --context) context="$2"; shift 2 ;;
  --namespace) namespace="$2"; shift 2 ;;
  --port) port="$2"; shift 2 ;;
  --keep) keep=true; shift ;;
  *) echo "usage: $0 --image IMAGE [--context CTX] [--namespace NS] [--port PORT] [--keep]" >&2; exit 2 ;;
  esac
done
[ -n "$image" ] || { echo "restore-drill: --image is required" >&2; exit 2; }

die() { echo "restore-drill: $*" >&2; exit 1; }
k() { if [ -n "$context" ]; then kubectl --context "$context" -n "$namespace" "$@"; else kubectl -n "$namespace" "$@"; fi; }

pod=loomux-restore-drill
container="loomux-drill-$$"
dir=$(mktemp -d)
chmod 700 "$dir"
cleanup() {
  k delete pod "$pod" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  docker rm -f "$container" >/dev/null 2>&1 || true
  if $keep; then echo "restore-drill: the copy is kept in $dir" >&2; else rm -rf "$dir"; fi
}
trap cleanup EXIT

# 1. The newest backup, through a read-only pod.
k apply -f - >/dev/null <<YAML
apiVersion: v1
kind: Pod
metadata: { name: $pod, namespace: $namespace }
spec:
  restartPolicy: Never
  securityContext: { fsGroup: 10001, runAsNonRoot: true, seccompProfile: { type: RuntimeDefault } }
  containers:
    - name: drill
      image: docker.io/keinos/sqlite3:3.46.1@sha256:055d4be20d868f4077598cbbc33ce4ff39dbefbbe0b87e39ca7b70f9f51caf51
      command: ["/bin/sh", "-c", "sleep 600"]
      securityContext: { allowPrivilegeEscalation: false, capabilities: { drop: ["ALL"] }, runAsUser: 10001, runAsGroup: 10001 }
      volumeMounts: [ { name: backup, mountPath: /backup, readOnly: true } ]
  volumes:
    - name: backup
      persistentVolumeClaim: { claimName: loomuxd-backup, readOnly: true }
YAML
k wait --for=condition=Ready "pod/$pod" --timeout=120s >/dev/null || die "the drill pod didn't start"
backup=$(k exec "$pod" -- sh -c 'ls -t /backup/loomux-*.db 2>/dev/null | head -1')
[ -n "$backup" ] || die "no backup found in the backup volume"
integrity=$(k exec "$pod" -- sqlite3 "$backup" 'PRAGMA integrity_check;')
[ "$integrity" = ok ] || die "integrity_check of $backup: $integrity"
from_schema=$(k exec "$pod" -- sqlite3 "$backup" 'select max(version_id) from schema_migrations;')
k exec "$pod" -- cat "$backup" >"$dir/loomux.db"
size=$(du -h "$dir/loomux.db" | cut -f1)
k delete pod "$pod" --wait=false >/dev/null

# 2. The image on the copy, with throwaway credentials.
password=$(head -c 18 /dev/urandom | base64 | tr -d '/+=')
hash=$(printf %s "$password" | docker run --rm -i "$image" -hash-password | tail -1)
docker run -d --name "$container" --user "$(id -u):$(id -g)" -e HOME=/tmp \
  -p "127.0.0.1:$port:8080" -v "$dir:/data" \
  -e LOOMUX_DB_PATH=/data/loomux.db -e "LOOMUX_AUTH_PASSWORD_HASH=$hash" \
  -e "LOOMUX_MASTER_KEY=$(head -c 32 /dev/urandom | base64)" \
  -e LOOMUX_WEB_UPDATES=off \
  -e LOOMUX_ROUTER_PRIMARY_BASE_URL=http://127.0.0.1:9/v1 \
  -e LOOMUX_ROUTER_PRIMARY_API_KEY=drill -e LOOMUX_ROUTER_PRIMARY_MODEL=drill "$image" >/dev/null

# 3. Checks.
base="http://127.0.0.1:$port/api/v1"
for _ in $(seq 60); do
  curl -fsS "$base/health" >/dev/null 2>&1 && break
  sleep 1
done
health=$(curl -fsS "$base/health" | sed -n 's/.*"status":"\([a-z]*\)".*/\1/p' | head -1)
[ "$health" = healthy ] || { docker logs "$container" 2>&1 | tail -20 >&2; die "health is '${health:-no answer}'"; }
token=$(curl -fsS -X POST "$base/login" -H 'Content-Type: application/json' \
  -d "{\"password\":\"$password\"}" | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
[ -n "$token" ] || die "login with the drill password failed"
targets=$(curl -fsS "$base/targets" -H "Authorization: Bearer $token" | grep -o '"kind":' | wc -l | tr -d ' ')
workspaces=$(curl -fsS "$base/workspaces" -H "Authorization: Bearer $token" | grep -o '"target_id":' | wc -l | tr -d ' ')
conversations=$(curl -fsS "$base/conversations" -H "Authorization: Bearer $token" | grep -o '"conversation_id":' | wc -l | tr -d ' ')
to_schema=$(docker run --rm -v "$dir:/data:ro" --user "$(id -u):$(id -g)" \
  docker.io/keinos/sqlite3:3.46.1@sha256:055d4be20d868f4077598cbbc33ce4ff39dbefbbe0b87e39ca7b70f9f51caf51 \
  sqlite3 -readonly /data/loomux.db 'select max(version_id) from schema_migrations;')
[ "$to_schema" -ge "$from_schema" ] || die "schema went from $from_schema to $to_schema"

echo "restore-drill: ok"
echo "| $(date +%F) | \`$(basename "$backup")\` (nightly, $size, schema $from_schema) | \`$image\` |" \
  "\`integrity_check\` ok; migrated $from_schema → $to_schema on start; health healthy; login ok;" \
  "$workspaces workspaces, $targets targets, $conversations conversations served |"
