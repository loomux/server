#!/usr/bin/env bash
# Smoke test for the agent image (LOOM-178): a green build says nothing
# about whether sshd starts as a non-root user or the CLIs run. Run in CI
# (.github/workflows/plugins.yml) and by hand:
#
#   docker build -t loomux-agent:dev deploy/agent && sh deploy/agent/smoke.sh loomux-agent:dev
#
# It checks the user, the binaries and sshd's config inside the image,
# then starts the container the way a plugin would (read-only root,
# tmpfs for /tmp, /run/loomux and /data, no capabilities, the two sshd
# files mounted read-only) and logs in over ssh with a generated key,
# pinning the host key it generated, as Loomux does.
set -euo pipefail

IMAGE="${1:?usage: smoke.sh <image>}"

echo "== inside the image"
docker run --rm --entrypoint /bin/sh "${IMAGE}" -c '
	set -e
	[ "$(id -u)" = 10002 ] && [ "$(id -un)" = agent ] || { echo "not running as agent (10002)"; exit 1; }
	[ "$HOME" = /data/home ] || { echo "HOME is $HOME"; exit 1; }
	[ "$DISABLE_AUTOUPDATER" = 1 ] || { echo "autoupdater not disabled"; exit 1; }
	env | grep -q "^LOOMUX_" && { echo "a LOOMUX_* variable is set in the image"; exit 1; }
	echo "claude   $(claude --version)"
	echo "codex    $(codex --version)"
	echo "opencode $(opencode --version)"
	echo "tmux     $(tmux -V)"
	echo "git      $(git --version)"
	echo "python3  $(python3 --version)"
	echo "rg       $(rg --version | head -n 1)"
	# sshd -t wants the host key to exist: a throwaway one at the real path.
	mkdir -p /run/loomux/ssh && chmod 0700 /run/loomux/ssh
	ssh-keygen -q -t ed25519 -N "" -f /run/loomux/ssh/host_ed25519
	/usr/sbin/sshd -t -f /etc/loomux/sshd_config
	touch /usr/local/bin/x 2>/dev/null && { echo "the root filesystem is writable by agent"; exit 1; }
	[ -w /data/work ] && [ -w /data/home ] && [ -w /run/loomux ] || { echo "/data or /run/loomux not writable"; exit 1; }
	echo "image ok"
'

echo "== ssh login"
tmp="$(mktemp -d)"
cid=""
cleanup() {
	if [ -n "${cid}" ]; then
		docker logs "${cid}" 2>&1 | tail -n 20 | sed 's/^/  container: /' || true
		docker rm -f "${cid}" >/dev/null 2>&1 || true
	fi
	rm -rf "${tmp}"
}
trap cleanup EXIT

ssh-keygen -q -t ed25519 -N "" -C "loomux-smoke-host" -f "${tmp}/host_ed25519"
ssh-keygen -q -t ed25519 -N "" -C "loomux-smoke" -f "${tmp}/loomux"
mkdir -p "${tmp}/src"
cp "${tmp}/host_ed25519" "${tmp}/src/host_ed25519"
echo "no-port-forwarding,no-agent-forwarding,no-X11-forwarding $(cat "${tmp}/loomux.pub")" > "${tmp}/src/authorized_keys"
# As a Secret volume lands: readable by all, writable by none.
chmod 0644 "${tmp}/src/"*
chmod 0755 "${tmp}/src"

cid="$(docker run -d \
	--read-only --tmpfs /tmp:size=64m --tmpfs /run/loomux:mode=1777,size=1m --tmpfs /data:mode=1777,size=64m \
	--user 10002:10002 --cap-drop ALL --security-opt no-new-privileges:true \
	-v "${tmp}/src:/etc/loomux/ssh-src:ro" \
	-p 127.0.0.1:0:2222 \
	"${IMAGE}")"
port="$(docker port "${cid}" 2222/tcp | head -n 1 | sed 's/.*://')"
echo "[127.0.0.1]:${port} $(cut -d' ' -f1,2 "${tmp}/host_ed25519.pub")" > "${tmp}/known_hosts"

ok=""
for _ in $(seq 1 30); do
	if out="$(ssh -i "${tmp}/loomux" -o IdentitiesOnly=yes -o UserKnownHostsFile="${tmp}/known_hosts" \
			-o StrictHostKeyChecking=yes -o BatchMode=yes -o ConnectTimeout=3 -o LogLevel=ERROR \
			-p "${port}" agent@127.0.0.1 'tmux -L loomux new-session -d -s smoke "sleep 5" && tmux -L loomux has-session -t smoke && tmux -L loomux kill-session -t smoke && pwd && id -u && test -w /data/work && mkdir -p "$HOME/.cache/loomux/completion-markers" && echo login-ok' 2>&1)"; then
		ok=1
		break
	fi
	sleep 1
done
if [ -z "${ok}" ]; then
	echo "ssh login failed: ${out:-no output}" >&2
	exit 1
fi
echo "${out}" | sed 's/^/  /'
echo "${out}" | grep -q '^login-ok$' || { echo "unexpected ssh output" >&2; exit 1; }
# A session lands in $HOME (sshd ignores the image's WORKDIR); Loomux
# names /data/work explicitly, and it must be writable.
echo "${out}" | grep -q '^/data/home$' || { echo "the login didn't land in /data/home" >&2; exit 1; }

# A human attach needs a pty, which a non-root sshd can only hand out
# when devpts is mounted the way runc/containerd do (gid=5, mode 0620).
pty="$(ssh -i "${tmp}/loomux" -o IdentitiesOnly=yes -o UserKnownHostsFile="${tmp}/known_hosts" \
		-o StrictHostKeyChecking=yes -o BatchMode=yes -o ConnectTimeout=3 -o LogLevel=ERROR \
		-tt -p "${port}" agent@127.0.0.1 'tty' 2>/dev/null | tr -d '\r')"
case "${pty}" in
	/dev/pts/*) echo "  pty ${pty}" ;;
	*) echo "no pty over ssh: ${pty:-no output}" >&2; exit 1 ;;
esac

# The wrong key is refused, and so is a password prompt (BatchMode).
ssh-keygen -q -t ed25519 -N "" -f "${tmp}/stranger"
if ssh -i "${tmp}/stranger" -o IdentitiesOnly=yes -o UserKnownHostsFile="${tmp}/known_hosts" \
		-o StrictHostKeyChecking=yes -o BatchMode=yes -o ConnectTimeout=3 -o LogLevel=ERROR \
		-p "${port}" agent@127.0.0.1 true 2>/dev/null; then
	echo "a stranger's key logged in" >&2
	exit 1
fi
echo "smoke ok"
