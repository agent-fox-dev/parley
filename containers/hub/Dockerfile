# The hub image: parleyd as user 10001, with its configuration and data on
# the one volume /var/lib/parley, and the parley CLI beside it. Build it from
# the repository root:
#
#   docker build -f containers/hub/Dockerfile \
#     --build-arg VERSION=1.4.0 --build-arg COMMIT=3f2a1bc \
#     --build-arg BUILD_TIME=2026-01-02T03:04:05Z -t parley-hub .
#
# A build argument left out reports buildinfo's default (dev, unknown).

# parleyd is built with cgo: the outline of every language but Go comes from
# tree-sitter grammars, which are C, so the build stage holds a C toolchain
# (build-base). parley is built without cgo, in a build of its own.
FROM docker.io/library/golang:1.27-alpine AS build
RUN apk add --no-cache build-base
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG VERSION
ARG COMMIT
ARG BUILD_TIME
RUN CGO_ENABLED=1 go build -trimpath -ldflags "-s -w \
      -X github.com/agent-fox-dev/parley/internal/buildinfo.Version=${VERSION} \
      -X github.com/agent-fox-dev/parley/internal/buildinfo.Commit=${COMMIT} \
      -X github.com/agent-fox-dev/parley/internal/buildinfo.BuildTime=${BUILD_TIME}" \
      -o /out/ ./cmd/parleyd
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w \
      -X github.com/agent-fox-dev/parley/internal/buildinfo.Version=${VERSION} \
      -X github.com/agent-fox-dev/parley/internal/buildinfo.Commit=${COMMIT} \
      -X github.com/agent-fox-dev/parley/internal/buildinfo.BuildTime=${BUILD_TIME}" \
      -o /out/ ./cmd/parley

# Alpine packages git 2.38 or later, which the hub refuses to start without.
# The stage is alpine like the build stage, so parleyd finds the C library
# it was linked against (musl) and needs no package of its own.
FROM docker.io/library/alpine:3.22
RUN apk add --no-cache git ca-certificates \
 && addgroup -S -g 10001 parley \
 && adduser -S -D -H -u 10001 -G parley -h /var/lib/parley -s /sbin/nologin parley \
 && mkdir -p /var/lib/parley/config /var/lib/parley/data \
 && chown -R 10001:10001 /var/lib/parley
COPY --from=build /out/parleyd /out/parley /usr/local/bin/

# The config dir is on the volume, because the bootstrap writes the admin
# token file there and the hub refuses to start while it exists: a
# read-only mount cannot hold it and the container's own filesystem loses
# it. The configuration's defaults listen on 0.0.0.0:8080.
ENV XDG_CONFIG_HOME=/var/lib/parley/config \
    XDG_DATA_HOME=/var/lib/parley/data
USER 10001:10001
WORKDIR /var/lib/parley
VOLUME /var/lib/parley
EXPOSE 8080
ENTRYPOINT ["parleyd"]
