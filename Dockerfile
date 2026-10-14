# syntax=docker/dockerfile:1

# --- build stage ------------------------------------------------------------
# Static binary: everything (migrations, templates, static assets) is embedded.
FROM golang:1.26-alpine AS build
WORKDIR /src
# Cache module downloads separately from the source.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-X main.version=${VERSION}" \
        -o /out/mmp ./cmd/mmp

# --- runtime base -----------------------------------------------------------
# ca-certificates: providers over HTTPS; tzdata: XMLTV local times.
FROM alpine:3.22 AS runtime
RUN apk add --no-cache ca-certificates tzdata
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
