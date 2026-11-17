# syntax=docker/dockerfile:1

# Hardened Wolfi-based Go images (https://taha2samy-3.github.io/node/), pinned
# by digest.
FROM --platform=$BUILDPLATFORM ghcr.io/taha2samy-3/go:v1.27-dev@sha256:d29a9e485d94e70dde6773d7d9e4ee3597cd0027273ff016acb314b290b378d8 AS build

WORKDIR /app/src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/app/go/pkg/mod,uid=1000,gid=1000 \
    go mod download

COPY cmd/ cmd/
COPY internal/ internal/

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
RUN --mount=type=cache,target=/app/go/pkg/mod,uid=1000,gid=1000 \
    --mount=type=cache,target=/app/.cache/go-build,uid=1000,gid=1000 \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH GOCACHE=/app/.cache/go-build \
    go build -trimpath -buildvcs=false \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /app/webhook ./cmd/webhook

FROM ghcr.io/taha2samy-3/go:v1.27@sha256:ce9b5f4a0a8974c3bdea4349a28f87a6d4c6d3f438189fb4da3d3f067d8d53a4

COPY --from=build --chown=0:0 --chmod=0555 /app/webhook /usr/local/bin/webhook

# Numeric UID so Kubernetes can enforce runAsNonRoot.
USER 1000:1000
# 8888: webhook API (localhost only by default); 8080: /healthz.
EXPOSE 8888 8080
ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/webhook"]
CMD []
