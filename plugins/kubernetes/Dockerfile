# syntax=docker/dockerfile:1
#
# The Kubernetes plugin's own image, ghcr.io/loomux/plugin-kubernetes:
# the sidecar form (docs/design/target-providers.md §1.8, §10 item 3),
# started with --listen /run/loomux/plugins/kubernetes.sock beside
# loomuxd. A pure-Go plugin execs nothing, so the base is distroless
# static. Build from the repository root:
#
#   docker build -f plugins/kubernetes/Dockerfile -t loomux-plugin-kubernetes:dev .
#
# The module replaces github.com/Loomux/server with ../.., hence the
# whole tree as the context.
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY plugins/kubernetes/go.mod plugins/kubernetes/go.sum ./plugins/kubernetes/
RUN cd plugins/kubernetes && go mod download
COPY . .
RUN cd plugins/kubernetes \
 && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/loomux-plugin-kubernetes ./cmd/loomux-plugin-kubernetes

# Pinned by digest as well as tag (a base change is an image change).
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/loomux-plugin-kubernetes /loomux-plugin-kubernetes
COPY plugins/kubernetes/plugin.json /plugin.json
# The pod sets runAsUser 10003 / runAsGroup 10001 so the socket it makes
# is connectable by loomuxd (§10); this is the fallback.
USER 10003:10001
ENTRYPOINT ["/loomux-plugin-kubernetes"]
LABEL org.opencontainers.image.title="loomux-plugin-kubernetes" \
      org.opencontainers.image.description="Loomux Kubernetes target-provider plugin: a pod per machine" \
      org.opencontainers.image.source="https://github.com/Loomux/server" \
      org.opencontainers.image.licenses="MIT"
