# syntax=docker/dockerfile:1

# ---- build ----------------------------------------------------------------
# The compiler always runs on the builder's native platform and cross-compiles
# for the target, so a multi-arch build does not run Go under emulation.
FROM --platform=$BUILDPLATFORM golang:1.27.1-alpine3.23 AS build
WORKDIR /src
RUN apk add --no-cache git
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/transmuxd ./cmd/transmuxd && \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/playbackd ./cmd/playbackd

# ---- test -----------------------------------------------------------------
# Built with --target test. ffmpeg is present so the integration tests can
# generate a real H.264 stream and probe the resulting segments.
FROM golang:1.27.1-alpine3.23 AS test
WORKDIR /src
RUN apk add --no-cache ffmpeg gcc musl-dev
COPY go.mod go.sum ./
RUN go mod download
COPY . .
CMD ["go", "test", "./...", "-count=1"]

# ---- runtime --------------------------------------------------------------
FROM alpine:3.23 AS runtime

ARG VERSION=dev
ARG REVISION=unknown
# The licenses label covers transmux itself. The image also ships Alpine's
# ffmpeg, which is configured with --enable-gpl --enable-version3, so the
# binaries in the image are effectively GPL-3.0-or-later (`ffmpeg -L`), even
# though the apk metadata lists the source licences as GPL-2.0-or-later AND
# LGPL-2.1-or-later. transmux only runs ffmpeg as a separate process.
LABEL org.opencontainers.image.title="transmux" \
      org.opencontainers.image.description="RTSP to HLS ingest (transmuxd) and authenticated playback (playbackd)" \
      org.opencontainers.image.source="https://github.com/jeonghun-app/transmux" \
      org.opencontainers.image.url="https://github.com/jeonghun-app/transmux" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.licenses="Apache-2.0"

# tini is PID 1 so any ffmpeg child that outlives its supervisor is reaped
# instead of accumulating as a zombie.
RUN apk add --no-cache ffmpeg tini ca-certificates && \
    addgroup -g 10001 -S transmux && \
    adduser -u 10001 -S -G transmux transmux

COPY --from=build /out/transmuxd /usr/local/bin/transmuxd
COPY --from=build /out/playbackd /usr/local/bin/playbackd

# The spool is intended to be a tmpfs mount at runtime. It is sized by
# local_list_size x bitrate x channels; see docs/capacity-model.md.
RUN mkdir -p /run/transmux/spool /var/lib/transmux/state /var/lib/transmux/playback /etc/transmux && \
    chown -R transmux:transmux /run/transmux /var/lib/transmux

USER transmux
EXPOSE 8080 8090
ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/transmuxd"]
CMD ["-config", "/etc/transmux/config.json"]
