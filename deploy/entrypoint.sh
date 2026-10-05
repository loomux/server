#!/bin/sh
# Container entrypoint for loomuxd (LOOM-52).
#
# Its only job is to materialise $HOME/.ssh before exec'ing the server.
#
# Why this exists at all: loomuxd reaches remote targets by exec'ing the
# ssh(1) binary (targets/remote.go), and production constructs the remote
# executor with no options — see targets/targets.go and the RemoteOption
# doc comment, which states outright that real targets "resolve via the
# user's own ~/.ssh/config". So ~/.ssh *is* the configuration surface:
# the identity, the SOCKS5 ProxyCommand for a userspace-mode Tailscale
# sidecar, and the pre-seeded known_hosts all live there.
#
# And ~/.ssh cannot simply be the Secret mount. ssh refuses a private key
# that is group/world-readable or not owned by the caller, and a Kubernetes
# Secret volume lands root-owned, mode 0644, on a read-only filesystem.
# Copying is what fixes the ownership: cp creates the destination as the
# running uid, so no chown (and no root) is needed.
set -eu

SSH_SRC="${LOOMUX_SSH_SOURCE:-/etc/loomux/ssh}"
SSH_DIR="${HOME:-/home/loomux}/.ssh"

warn() { echo "loomux-entrypoint: $*" >&2; }

if [ -d "${SSH_SRC}" ]; then
	mkdir -p "${SSH_DIR}"
	chmod 0700 "${SSH_DIR}"

	copied=""
	# A Secret volume's entries are symlinks into ..data/, so -L is
	# required; the dot-prefixed bookkeeping entries never match the glob.
	for src in "${SSH_SRC}"/*; do
		[ -f "${src}" ] || continue
		name="$(basename "${src}")"
		cp -L "${src}" "${SSH_DIR}/${name}"
		chmod 0600 "${SSH_DIR}/${name}"
		copied="${copied} ${name}"
	done

	if [ -z "${copied}" ]; then
		warn "WARNING: ${SSH_SRC} exists but is empty — no SSH material installed."
	else
		warn "installed into ${SSH_DIR}:${copied}"
	fi

	# Worth its own warning because the failure is both certain and
	# late: production runs ssh with BatchMode=yes and no
	# StrictHostKeyChecking override, so an unknown host key is not a
	# prompt, it is a refusal — and it surfaces as a dispatch failing
	# minutes or hours after a deploy that looked healthy.
	if [ -n "${copied}" ] && [ ! -f "${SSH_DIR}/known_hosts" ]; then
		warn "WARNING: no known_hosts in ${SSH_SRC}. Every remote dispatch will fail with"
		warn "         'Host key verification failed' — BatchMode=yes cannot prompt to accept"
		warn "         a key. Pre-seed it with ssh-keyscan; see docs/deploy/ssh.md."
	fi
else
	warn "no ${SSH_SRC} — starting without SSH material (local targets only)."
fi

# `docker run <image> -hash-password` passes loomuxd's own flags; accept
# `docker run <image> loomuxd -hash-password` too, rather than running
# `loomuxd loomuxd …`, whose flag parsing would stop at the stray word and
# start the server instead.
if [ "${1:-}" = loomuxd ]; then
	shift
fi
exec /usr/local/bin/loomuxd "$@"
