# syntax=docker/dockerfile:1.7
#
# Multi-stage build:
#   1. builder  — golang:alpine, compiles runtime + release artifacts
#   2. release  — scratch export stage for ./bin/ binaries + zip
#   3. runtime  — distroless/static, ships sfu, sfs, and sfl
#
# For full byte-reproducibility, pin the base images by digest:
#   FROM golang:1.25-alpine@sha256:<digest> AS builder
#   FROM gcr.io/distroless/static-debian12:nonroot@sha256:<digest>
# Tags below track latest patch within the same Go minor / Debian minor.

# ─── 1. builder ─────────────────────────────────────────────────────────────
FROM golang:1.25-alpine AS builder
ARG VERSION=0.1
ARG TARGETOS=linux
ARG TARGETARCH=amd64
WORKDIR /src

RUN apk add --no-cache make zip

# Cache module downloads in their own layer so source-only changes don't
# reinvalidate the dep fetch.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Build the shareable release bundle; runtime copies target-platform binaries.
RUN make release VERSION="${VERSION}" BIN_DIR=/out/bin RELEASE_ZIP="SnowFastULP-${VERSION}-binaries.zip"
RUN ext=""; [ "${TARGETOS}" = "windows" ] && ext=".exe"; \
    cp "/out/bin/${TARGETOS}/${TARGETARCH}/sfu${ext}" /out/sfu; \
    cp "/out/bin/${TARGETOS}/${TARGETARCH}/sfs${ext}" /out/sfs; \
    cp "/out/bin/${TARGETOS}/${TARGETARCH}/sfl${ext}" /out/sfl

# ─── 2. release export ──────────────────────────────────────────────────────
FROM scratch AS release
COPY --from=builder /out/bin /bin

# ─── 3. runtime ─────────────────────────────────────────────────────────────
# The :debug-nonroot tag ships busybox at /busybox so the dispatcher script
# below has a working /busybox/sh; the plain :nonroot tag has no shell at
# all, which would break entrypoint dispatch. Static layout otherwise.
FROM gcr.io/distroless/static-debian12:debug-nonroot
COPY --from=builder /out/sfu /usr/local/bin/sfu
COPY --from=builder /out/sfs /usr/local/bin/sfs
COPY --from=builder /out/sfl /usr/local/bin/sfl
COPY --chmod=0755 scripts/docker-entrypoint.sh /entrypoint.sh
WORKDIR /work
# Routes argv[0] in {sfu, sfs, sfl} to the matching binary; anything else falls
# through to sfu so the historical `docker run IMAGE input.txt -o ./out/`
# invocation keeps working unchanged.
ENTRYPOINT ["/entrypoint.sh"]
