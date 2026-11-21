# syntax=docker/dockerfile:1
# SPDX-FileCopyrightText: 2026 The tikker Authors
# SPDX-License-Identifier: GPL-3.0-or-later
#
# tikker image: static binary on FROM scratch, non-root, read-only rootfs friendly.
# Build from the repository root, e.g. `make docker` (see deploy/docker/README.md).
#
# The builder runs on $BUILDPLATFORM and cross-compiles for $TARGETPLATFORM, so no
# QEMU emulation is needed. The final stage contains no shell and no package manager.

# Go 1.26 (current stable at the time of writing) on Alpine. The digest pins the
# multi-arch index. Refresh it with: docker buildx imagetools inspect golang:1.26-alpine
ARG GO_IMAGE=golang:1.26-alpine@sha256:ecb67f987aa1a57794c91d19ade7ae0209f2f0f5496aaef9e6ae85ddf92d02ab

FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS builder

ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown

# CA bundle for the final image: the Go net/http client needs it to reach HTTPS routers and APIs.
RUN apk add --no-cache ca-certificates

WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

# -tags timetzdata embeds the IANA time zone database into the binary, because the
# scratch image has no /usr/share/zoneinfo. The cost is well under 1 MB.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    set -eux; \
    GOARM=""; \
    if [ "$TARGETARCH" = "arm" ]; then GOARM="${TARGETVARIANT#v}"; GOARM="${GOARM:-7}"; fi; \
    export GOARM; \
    mkdir -p /out/data; \
    CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" go build \
      -trimpath \
      -tags timetzdata \
      -ldflags "-s -w \
        -X github.com/m4drak/tikker/internal/version.Version=${VERSION} \
        -X github.com/m4drak/tikker/internal/version.Commit=${COMMIT} \
        -X github.com/m4drak/tikker/internal/version.Date=${DATE}" \
      -o /out/tikker ./cmd/tikker

# Non-root account (65532 is the conventional "nobody"-style id). Only the numeric
# id is needed at run time; the passwd entry makes tools print a name instead of a number.
RUN printf 'tikker:x:65532:65532:tikker:/data:/sbin/nologin\n' > /out/passwd \
 && printf 'tikker:x:65532:\n' > /out/group

FROM scratch

ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown

LABEL org.opencontainers.image.title="tikker" \
      org.opencontainers.image.description="SMS gateway for MikroTik RouterOS 7 devices (LTE/5G modem, SIM)" \
      org.opencontainers.image.source="https://github.com/m4drak/tikker" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.created="${DATE}" \
      org.opencontainers.image.licenses="GPL-3.0-or-later"

COPY --from=builder /out/tikker /tikker
COPY --from=builder /out/passwd /etc/passwd
COPY --from=builder /out/group /etc/group
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
# The only writable path (SPEC §2.2): database and secret.key live here.
COPY --from=builder --chown=65532:65532 /out/data /data

# Runtime settings. GOGC/GOMEMLIMIT keep RSS at rest within SPEC §2.2 (<= 40 MB):
# without them the Go heap grows to ~60 MB before the first collection cycle. The
# GOMEMLIMIT is a soft limit, so it costs extra GC work under load rather than failing.
ENV TIKKER_DATA_DIR=/data \
    TIKKER_LISTEN=:8080 \
    GOGC=30 \
    GOMEMLIMIT=24MiB

USER 65532:65532
VOLUME /data
EXPOSE 8080

# Probes /readyz on the local listener; exit 0 only for 200 OK.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD ["/tikker", "healthcheck"]

ENTRYPOINT ["/tikker"]
CMD ["serve"]
