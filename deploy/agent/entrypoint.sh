#!/bin/sh
# Entrypoint of the agent image (LOOM-178).
#
# Its only job is to put sshd's two files where sshd wants them, with
# the modes sshd insists on, then become sshd. The plugin mounts the
# host key Loomux generated and the authorized_keys line of the target's
# key at /etc/loomux/ssh-src (a Secret volume on Kubernetes: root-owned,
# 0644, read-only; an uploaded archive on Docker). sshd refuses a host
# key that is group- or world-readable, so they are copied into a
# private directory on tmpfs, as the server's own entrypoint copies
# ~/.ssh. Nothing else is set up: the data volume is the agent user's,
# and Loomux creates workspaces under /data/work itself.
set -eu

SRC=/etc/loomux/ssh-src
DST=/run/loomux/ssh

warn() { echo "loomux-agent: $*" >&2; }

if [ ! -d "${SRC}" ]; then
	warn "no ${SRC}: the plugin must mount the host key and authorized_keys there"
	exit 1
fi
mkdir -p "${DST}"
chmod 0700 "${DST}"
for name in host_ed25519 authorized_keys; do
	if [ ! -f "${SRC}/${name}" ]; then
		warn "${SRC}/${name} is missing"
		exit 1
	fi
	# A Secret volume's entries are symlinks into ..data/: -L follows them.
	cp -L "${SRC}/${name}" "${DST}/${name}"
	chmod 0600 "${DST}/${name}"
done

# The volume is mounted over /data: on Kubernetes the plugin sets
# fsGroup 10002 so a fresh one is writable; without that this fails here,
# not later inside an agent.
if [ ! -w /data ]; then
	warn "/data is not writable by $(id -un) ($(id -u)): the volume needs fsGroup/ownership 10002"
	exit 1
fi
mkdir -p /data/home /data/work
warn "sshd starting on port 2222 as $(id -un), workspaces under /data/work"
exec /usr/sbin/sshd -D -e -f /etc/loomux/sshd_config
