# syntax=docker/dockerfile:1
#
# The Docker plugin's own image, ghcr.io/loomux/plugin-docker: the
# sidecar form (docs/design/target-providers.md §1.8), started with
# --listen /run/loomux/plugins/docker.sock beside loomuxd. A pure-Go
# plugin execs nothing (docker system dial-stdio runs on the docker
# host, over SSH), so the base is distroless static. Build from the
# repository root:
#
#   docker build -f plugins/docker/Dockerfile -t loomux-plugin-docker:dev .
#
# The module replaces github.com/Loomux/server with ../.., hence the
# whole tree as the context.
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY plugins/docker/go.mod plugins/docker/go.sum ./plugins/docker/
RUN cd plugins/docker && go mod download
COPY . .
RUN cd plugins/docker \
 && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/loomux-plugin-docker ./cmd/loomux-plugin-docker

# Pinned by digest as well as tag (a base change is an image change).
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/loomux-plugin-docker /loomux-plugin-docker
COPY plugins/docker/plugin.json /plugin.json
# The pod sets runAsUser 10003 / runAsGroup 10001 so the socket it makes
# is connectable by loomuxd (§10); this is the fallback.
USER 10003:10001
ENTRYPOINT ["/loomux-plugin-docker"]
LABEL org.opencontainers.image.title="loomux-plugin-docker" \
      org.opencontainers.image.description="Loomux Docker target-provider plugin: a container per machine on one docker host" \
      org.opencontainers.image.source="https://github.com/Loomux/server" \
      org.opencontainers.image.licenses="MIT"
