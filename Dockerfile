# syntax=docker/dockerfile:1

# --- build stage ------------------------------------------------------------
# Static binary: everything (migrations, templates, static assets) is embedded.
# Runs on the build machine's own architecture and cross-compiles for the
# target (fast multi-arch builds, no emulation).
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
WORKDIR /src
# Cache module downloads separately from the source.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
        -o /out/mmp ./cmd/mmp

# --- runtime base -----------------------------------------------------------
# ca-certificates: providers over HTTPS; tzdata: XMLTV local times.
FROM alpine:3.22 AS runtime
LABEL org.opencontainers.image.source="https://github.com/JustDr00py/mmp" \
      org.opencontainers.image.description="MMP: self-hosted IPTV multiplexer with automatic failover" \
      org.opencontainers.image.licenses="AGPL-3.0-only"
RUN apk add --no-cache ca-certificates tzdata \
 && mkdir -p /var/lib/mmp/backups && chown -R nobody:nobody /var/lib/mmp && chmod 700 /var/lib/mmp/backups
# Backups go here; mount a volume on it to keep them (see docker-compose.yml).
ENV MMP_BACKUP_DIR=/var/lib/mmp/backups
COPY --from=build /out/mmp /usr/local/bin/mmp
USER nobody:nobody
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
        CMD wget -q -O /dev/null http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["/usr/local/bin/mmp"]

# --- mmp-ffmpeg variant ------------------------------------------------------
# Adds ffmpeg (and ffprobe) for channels with an ffmpeg stream profile and the
# background prober. Build with: docker build --target mmp-ffmpeg .
FROM runtime AS mmp-ffmpeg
USER root
RUN apk add --no-cache ffmpeg
USER nobody:nobody

# --- default image (no ffmpeg) ----------------------------------------------
# Last stage, so a plain `docker build .` produces the small variant.
FROM runtime AS mmp
