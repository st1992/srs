# syntax=docker/dockerfile:1

# ---- Build stage ----
FROM golang:1.26-alpine AS build

WORKDIR /src

# Cache module downloads separately from the source.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

# Build the static binary.
# BUILD_EXPIRES can be overridden at build time:
#   docker build --build-arg BUILD_EXPIRES=2026-09-14 .
# Defaults to 3 months from the Go module's initial release date.
ARG BUILD_EXPIRES=2026-09-14
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux \
    go build -trimpath \
    -ldflags="-s -w -X main.buildExpires=${BUILD_EXPIRES}" \
    -o /out/siprec-recorder .

# ---- Runtime stage ----
# Debian slim so we can `apt install sngrep` for in-pod SIP packet inspection.
FROM debian:bookworm-slim

# ca-certificates : TLS to GCP Pub/Sub / GCS.
# tzdata          : correct timestamps in structured logs when TZ is overridden.
# sngrep          : interactive SIP message viewer for debugging on the pod
#                   (e.g. `kubectl exec -it <pod> -- sngrep -d any port 5060`).
RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        ca-certificates \
        tzdata \
        sngrep \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --system siprec \
    && useradd  --system --gid siprec --home-dir /app --shell /usr/sbin/nologin siprec

WORKDIR /app

COPY --from=build /out/siprec-recorder /usr/local/bin/siprec-recorder
COPY config.example.yaml /app/config.yaml

# Create the recordings directory and fix ownership before declaring the volume
# so that Docker seeds named-volume mounts with the correct siprec ownership.
RUN mkdir -p /app/recordings && chown -R siprec:siprec /app
VOLUME ["/app/recordings"]

USER siprec

# SIP signalling (UDP).
EXPOSE 5060/udp
# RTP media port range — must match rtp_port_start / rtp_port_end in config.yaml.
# Default example config uses 10000-11000 (~500 concurrent sessions).
# Increase rtp_port_end (and re-EXPOSE here) for higher concurrency deployments.
EXPOSE 10000-65535/udp

# Liveness probe: check that the process is still running and the SIP port is bound.
# Adjust the period/retries to suit your orchestrator's expectations.
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD [ "sh", "-c", "kill -0 1 && grep -q 'siprec-recorder' /proc/1/cmdline" ]

ENTRYPOINT ["siprec-recorder"]
CMD ["-config", "/app/config.yaml"]
