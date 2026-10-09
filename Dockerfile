# syntax=docker/dockerfile:1
#
# Container image for loomuxd (LOOM-50). Published as
# ghcr.io/loomux/server — theWyseKube's manifests pin that exact name.
#
# The runtime base is NOT scratch/distroless, and that is load-bearing:
# loomuxd shells out to real binaries at runtime — ssh (targets/remote.go),
# tmux and sh (targets/local.go) — rather than using an in-process SSH
# library. A static binary in an empty base image builds clean, passes CI,
# and then fails on every dispatch. See docs/deploy/container.md.

# ---------------------------------------------------------------------------
# Stage 1 — build the binary
# ---------------------------------------------------------------------------
FROM golang:1.27-alpine AS build

WORKDIR /src

# Dependencies first so editing source doesn't re-download the module graph.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# VERSION is stamped into version.Version, which backs both `loomuxd
# -version` and GET /api/v1/version.
#
# CI passes a human-readable string that CONTAINS SPACES:
# "<short sha> (web <loomux/web ref>)". That is why the -X value below is
# single-quoted, and the quotes are load-bearing: `go build` splits the
# -ldflags value on whitespace (respecting quotes) before handing it to the
# linker, so an unquoted value makes the linker see "(web" and "<ref>)" as
# unknown flags and the build fails. Keep the quotes even if some future
# caller only passes a bare SHA.
ARG VERSION=dev

# CGO_ENABLED=0 is safe here: the only C-ish dependency would be SQLite, and
# this repo uses modernc.org/sqlite (pure Go). That also makes the musl/glibc
# question moot for the runtime base.
RUN CGO_ENABLED=0 go build \
        -trimpath \
        -ldflags "-s -w -X 'github.com/Loomux/server/version.Version=${VERSION}'" \
        -o /out/loomuxd ./cmd/loomuxd

# ---------------------------------------------------------------------------
# Stage 1b — the first-party plugins, bundled (LOOM-178, design §1.8)
# ---------------------------------------------------------------------------
# Each plugin is its own Go module under plugins/<name> (so client-go and
# the like never enter the server's go.mod), built here and placed under
# LOOMUX_PLUGIN_BUNDLE_DIR as <name>/plugin.json + <name>/loomux-plugin-<name>.
# The modules replace github.com/Loomux/server with ../.., hence the whole
# tree in the context.
FROM golang:1.27-alpine AS plugins

WORKDIR /src
COPY go.mod go.sum ./
COPY plugins/kubernetes/go.mod plugins/kubernetes/go.sum ./plugins/kubernetes/
RUN cd plugins/kubernetes && go mod download

COPY . .
RUN cd plugins/kubernetes \
 && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" \
        -o /out/plugins/kubernetes/loomux-plugin-kubernetes ./cmd/loomux-plugin-kubernetes \
 && cp plugin.json /out/plugins/kubernetes/plugin.json

# ---------------------------------------------------------------------------
# Stage 2 — runtime
# ---------------------------------------------------------------------------
FROM alpine:3.22

# openssh-client / tmux / busybox sh: the binaries loomuxd execs.
# netcat-openbsd: `nc -X 5 -x host:port` is the SOCKS5 ProxyCommand used to
#   reach targets through a userspace-mode Tailscale sidecar. busybox's own
#   nc has no -X, so the real OpenBSD netcat is required, not incidental.
# socat: second SOCKS5 implementation (SOCKS5-CONNECT), kept as a fallback
#   since the two disagree about DNS handling in some setups.
# ca-certificates: outbound HTTPS to the router model's API.
RUN apk add --no-cache \
        ca-certificates \
        tzdata \
        openssh-client \
        tmux \
        netcat-openbsd \
        socat

# The runtime user. uid/gid 10001 is fixed so k8s can set
# runAsUser/runAsGroup/fsGroup to match: ssh calls getpwuid() and refuses to
# run as a uid with no passwd entry, so this image cannot take an arbitrary
# uid the way a pure-Go image could.
RUN addgroup -g 10001 loomux \
 && adduser -D -u 10001 -G loomux -h /home/loomux loomux \
 && mkdir -p /var/lib/loomux /srv/loomux/web /usr/local/lib/loomux/plugins /run/loomux/plugins \
 && chown -R loomux:loomux /home/loomux /var/lib/loomux /srv/loomux /usr/local/lib/loomux /run/loomux \
 && chmod 0700 /home/loomux \
 && chgrp -R 0 /home/loomux /var/lib/loomux \
 && chmod -R g=u /home/loomux /var/lib/loomux

COPY --from=build /out/loomuxd /usr/local/bin/loomuxd
COPY --from=plugins --chown=loomux:loomux /out/plugins/ /usr/local/lib/loomux/plugins/

# Materialises $HOME/.ssh from a read-only Secret mount before exec'ing
# loomuxd. Remote dispatch is configured through ~/.ssh (identity, the
# SOCKS5 ProxyCommand, known_hosts) because production builds the remote
# executor with no options — see docs/deploy/ssh.md.
# Plain COPY, not COPY --chmod: --chmod requires BuildKit, and this file
# must also build under the legacy builder (docs/deploy/container.md
# documents a bare `docker build .` as supported). COPY preserves the
# source mode instead, so this relies on the script being committed
# executable — git records the exec bit, and losing it would leave the
# image unable to start at all.
COPY deploy/entrypoint.sh /usr/local/bin/loomux-entrypoint

# The web client's built static assets. loomuxd serves them itself (LOOM-33)
# for any non-/api/* path, with SPA fallback to index.html.
#
# WEB_DIST is a path *in the build context*, not a URL or a repo ref. The
# default is a committed placeholder page, so `docker build .` works with no
# credentials and no network beyond the module/APK mirrors; CI checks
# loomux/web out at a pinned ref and overrides it. The alternatives — cloning
# loomux/web inside this Dockerfile, or fetching a release artifact — are
# rejected for reasons documented in docs/deploy/container.md.
ARG WEB_DIST=deploy/web-placeholder
COPY --chown=loomux:loomux ${WEB_DIST}/ /srv/loomux/web/

# LOOMUX_LOCAL_TARGETS=off (LOOM-141): an agent on a local target would
# run as loomuxd's own user inside this container, able to read its
# database, vault key and SSH keys. Register machines as remote targets.
# Plugins (LOOM-178): first-party plugins are bundled under
# LOOMUX_PLUGIN_BUNDLE_DIR (the Kubernetes plugin, from stage 1b); a
# sidecar container serves its socket in LOOMUX_PLUGIN_SOCKET_DIR (an
# emptyDir shared with it).
ENV HOME=/home/loomux \
    LOOMUX_LOCAL_TARGETS=off \
    LOOMUX_PLUGIN_BUNDLE_DIR=/usr/local/lib/loomux/plugins \
    LOOMUX_PLUGIN_SOCKET_DIR=/run/loomux/plugins \
    LOOMUX_STATIC_DIR=/srv/loomux/web \
    LOOMUX_DB_PATH=/var/lib/loomux/loomux.db \
    LOOMUX_HTTP_ADDR=:8080

USER loomux
WORKDIR /var/lib/loomux
EXPOSE 8080

# GET /api/v1/health (unauthenticated) is the route for k8s
# liveness/readiness probes.
#
# Arguments are forwarded to loomuxd unchanged, so `docker run <image>
# -version` still works. Use --entrypoint to bypass the SSH setup.
ENTRYPOINT ["/usr/local/bin/loomux-entrypoint"]

LABEL org.opencontainers.image.title="loomux-server" \
      org.opencontainers.image.description="Loomux server (loomuxd): chat-driven multi-agent tmux orchestration" \
      org.opencontainers.image.source="https://github.com/Loomux/server" \
      org.opencontainers.image.licenses="MIT"
