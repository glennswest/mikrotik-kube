# Multi-stage build for mikrotik-vk
# Produces a minimal static binary suitable for RouterOS containers.
#
# RouterOS container constraints:
#   - Expects a tar archive of a rootfs
#   - Limited resources (ARM64 or x86_64 depending on device)
#   - No systemd, no init system

# ── Stage 1: Build ──────────────────────────────────────────────────────────
FROM golang:1.22-alpine AS builder

RUN apk add --no-cache git ca-certificates

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Build static binary (CGO_ENABLED=0 for scratch compatibility)
ARG TARGETOS=linux
ARG TARGETARCH=arm64
ARG VERSION=dev
ARG COMMIT=none

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
    -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
    -o /mikrotik-vk \
    ./cmd/mikrotik-vk/

# ── Stage 2: Runtime ────────────────────────────────────────────────────────
FROM alpine:3.19

RUN apk add --no-cache \
    ca-certificates \
    curl \
    tini

# Create non-root user
RUN addgroup -S mikrotik-vk && adduser -S -G mikrotik-vk mikrotik-vk

# Create data directories
RUN mkdir -p /etc/mikrotik-vk /data/registry /data/cache /data/volumes \
    && chown -R mikrotik-vk:mikrotik-vk /data

COPY --from=builder /mikrotik-vk /usr/local/bin/mikrotik-vk

# Default config
COPY deploy/config.yaml /etc/mikrotik-vk/config.yaml

EXPOSE 5000 8080

# Use tini as PID 1 (proper signal handling in containers)
ENTRYPOINT ["/sbin/tini", "--"]
CMD ["mikrotik-vk", "--config", "/etc/mikrotik-vk/config.yaml"]
