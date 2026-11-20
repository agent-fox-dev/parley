# The reference sandbox image: git, a POSIX shell, the CA certificates and the
# parley CLI, run as user 1000 with a writable home and the working directory
# /workspace, which the user owns. It satisfies the image contract of a
# sandbox and is the example an operator's own image follows: OpenShell
# creates a sandbox without a working directory, and its user cannot create
# /workspace, so the image holds it.
# Build it from the repository root:
#
#   docker build -f containers/sandbox/Dockerfile \
#     --build-arg VERSION=1.4.0 --build-arg COMMIT=3f2a1bc \
#     --build-arg BUILD_TIME=2026-01-02T03:04:05Z -t parley-sandbox .
#
# A build argument left out reports buildinfo's default (dev, unknown).

FROM docker.io/library/golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG VERSION
ARG COMMIT
ARG BUILD_TIME
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w \
      -X github.com/agent-fox-dev/parley/internal/buildinfo.Version=${VERSION} \
      -X github.com/agent-fox-dev/parley/internal/buildinfo.Commit=${COMMIT} \
      -X github.com/agent-fox-dev/parley/internal/buildinfo.BuildTime=${BUILD_TIME}" \
      -o /out/ ./cmd/parley

# Alpine packages git 2.38 or later and has /bin/sh. No parleyd and no agent:
# the sandbox runs what it is started with, so there is no entry point.
FROM docker.io/library/alpine:3.22
RUN apk add --no-cache git ca-certificates \
 && addgroup -g 1000 sandbox \
 && adduser -D -u 1000 -G sandbox -h /home/sandbox -s /bin/sh sandbox \
 && mkdir /workspace && chown 1000:1000 /workspace
COPY --from=build /out/parley /usr/local/bin/
USER 1000:1000
ENV HOME=/home/sandbox
WORKDIR /home/sandbox
