# syntax=docker/dockerfile:1.7
#
# Multi-stage build:
#   1. builder  — golang:alpine, compiles runtime + release artifacts
#   2. release  — scratch export stage for ./bin/ binaries + zip
#   3. runtime  — distroless/static, ships sfu, sfs, and sfl
#
# Both base images are pinned by digest (multi-arch manifest-list digests,
# i.e. what the tags resolve to today) so patch bumps are explicit changes:
#   golang:1.25-alpine                        = sha256:1ae0735f00daffa3aaf1363a5184c0d2dc55c78e3db4ec70241cdac97bf84b59
#   distroless static-debian12:debug-nonroot  = sha256:d5563cc7f2f44313f332e91138cc8c6a158899afeeeab2fce3b0f9ccdb3cf9ee
# Refresh (per tag, then update the @sha256 pins below):
#   docker manifest inspect golang:1.25-alpine
#   docker manifest inspect gcr.io/distroless/static-debian12:debug-nonroot
# (or `crane digest <ref>`).

# ─── 1. builder ─────────────────────────────────────────────────────────────
FROM golang:1.25-alpine@sha256:1ae0735f00daffa3aaf1363a5184c0d2dc55c78e3db4ec70241cdac97bf84b59 AS builder
ARG VERSION=0.3
ARG TARGETOS=linux
ARG TARGETARCH=amd64
WORKDIR /src

RUN apk add --no-cache make zip

# Cache module downloads in their own layer so source-only changes don't
# reinvalidate the dep fetch.
COPY go.mod go.sum ./
# The vendored yekazip replace target must be present before the download:
# go.mod's `replace github.com/yeka/zip => ./third_party/yekazip` makes
# `go mod download` read ./third_party/yekazip/go.mod. Kept above `COPY . .`
# so source-only changes still don't reinvalidate this layer.
COPY third_party/yekazip/ ./third_party/yekazip/
RUN go mod download

COPY . .

# Build the shareable release bundle; runtime copies target-platform binaries.
# Local docker builds default to the dev stamp (ARG VERSION=0.2-dev above),
# so the in-container `make release` needs the explicit escape hatch. Real
# release artifacts come from the GitHub release workflow with an explicit
# VERSION; the docker-build-all make target re-guards dev stamps host-side.
RUN make release VERSION="${VERSION}" BIN_DIR=/out/bin \
	RELEASE_ZIP="SnowFastULP-${VERSION}-binaries.zip" ALLOW_DEV_STAMP=1
# H-15: the release layout has no linux/arm64 tree — the supported arm64
# binaries are the STATIC linux/arm64 builds stored under bin/android/arm64
# (published as the -android-arm64 assets). Map the BuildKit platform to the
# directory make release actually produces, or the runtime-stage cp fails.
RUN set -e; \
    ext=""; [ "${TARGETOS}" = "windows" ] && ext=".exe"; \
    srcdir="/out/bin/${TARGETOS}/${TARGETARCH}"; \
    if [ "${TARGETOS}" = "linux" ] && [ "${TARGETARCH}" = "arm64" ]; then \
        srcdir="/out/bin/android/arm64"; \
    fi; \
    cp "${srcdir}/sfu${ext}" /out/sfu; \
    cp "${srcdir}/sfs${ext}" /out/sfs; \
    cp "${srcdir}/sfl${ext}" /out/sfl

# ─── 2. release export ──────────────────────────────────────────────────────
FROM scratch AS release
COPY --from=builder /out/bin /bin

# ─── 3. runtime ─────────────────────────────────────────────────────────────
# The :debug-nonroot tag ships busybox at /busybox so the dispatcher script
# below has a working /busybox/sh; the plain :nonroot tag has no shell at
# all, which would break entrypoint dispatch. Static layout otherwise.
FROM gcr.io/distroless/static-debian12:debug-nonroot@sha256:d5563cc7f2f44313f332e91138cc8c6a158899afeeeab2fce3b0f9ccdb3cf9ee
COPY --from=builder /out/sfu /usr/local/bin/sfu
COPY --from=builder /out/sfs /usr/local/bin/sfs
COPY --from=builder /out/sfl /usr/local/bin/sfl
COPY --chmod=0755 scripts/docker-entrypoint.sh /entrypoint.sh
WORKDIR /work
# Routes argv[0] in {sfu, sfs, sfl} to the matching binary; anything else falls
# through to sfu so the historical `docker run IMAGE input.txt -o ./out/`
# invocation keeps working unchanged.
ENTRYPOINT ["/entrypoint.sh"]
