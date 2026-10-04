# SnowFastULP build / test / docker shortcuts.
#
# Reproducible-build flags:
#   -trimpath        strip local filesystem paths from the binary
#   -buildvcs=false  strip git tag/commit metadata
#   -s -w            strip symbol + DWARF tables
#   -buildid=        clear Go's per-build identifier
# Combined with CGO_ENABLED=0, same source + same Go version → same SHA256.

VERSION       ?= 0.3
BUILD_FLAGS   := -trimpath -buildvcs=false -ldflags="-s -w -buildid= -X github.com/snowx-dev/SnowFastULP/internal/version.String=$(VERSION)"
PKG           := ./cmd/sfu
PKG_SFS       := ./cmd/sfs
PKG_SFL       := ./cmd/sfl
PLATFORMS     := linux/amd64 darwin/arm64 windows/amd64
# Android ships as a STATIC linux/arm64 build under the -android-arm64 asset
# name: no cgo in the tree, so a static binary runs everywhere a terminal
# does on the device — Termux, proot-distro, chroot, plain adb shell — and
# on real linux/arm64 hosts too (selfupdate + install.sh map linux/arm64 to
# it). GOOS=android would link against bionic via /system/bin/linker64 and
# only run natively, so it is NOT what we publish.
ANDROID_GOOS  := linux
ANDROID_ARCH  := arm64
BIN_DIR         ?= bin
RELEASE_BIN_DIR ?= release-bins
DIST_DIR        ?= dist
RELEASE_ZIP     ?= SnowFastULP-$(VERSION)-binaries.zip
DOCKER_IMAGE  ?= sfu:local

.PHONY: build build-sfu build-sfs build-sfl build-all release release-assets release-zip manifest test vet clean checksums \
	docker-build docker-build-all sync-release-bins docker-run docker-run-sfs docker-run-sfl example-sync help \
	check-release-version

# Default target: print available targets when invoked as bare `make`.
help:
	@echo "Targets:"
	@echo "  build           Build sfu, sfs, and sfl for the current platform into ./$(BIN_DIR)/"
	@echo "  build-sfu       Build sfu only"
	@echo "  build-sfs       Build sfs only"
	@echo "  build-sfl       Build sfl only"
	@echo "  build-all       Cross-compile both binaries for primary platforms"
	@echo "  release         Build primary platforms and ./$(BIN_DIR)/$(RELEASE_ZIP)"
	@echo "  release-assets  Build flat release downloads into ./$(DIST_DIR)/"
	@echo "  manifest        Generate ./$(DIST_DIR)/update-manifest.json from ./$(DIST_DIR)/SHA256SUMS"
	@echo "  test            go test -race ./... plus third_party/yekazip nested module"
	@echo "  vet             go vet + gofmt clean check"
	@echo "  checksums       SHA256SUMS for release binaries in ./$(BIN_DIR)/"
	@echo "  clean           Remove build artifacts"
	@echo "  example-sync    Copy config.toml.example into internal/config (embedded template)"
	@echo ""
	@echo "  docker-build      Build a runtime image ($(DOCKER_IMAGE)) with sfu, sfs, and sfl"
	@echo "  docker-build-all  Build release binaries via Docker; sync ./$(BIN_DIR)/ → ./$(RELEASE_BIN_DIR)/"
	@echo "  docker-run        Run sfu in a container; pass ARGS=... for sfu args"
	@echo "  docker-run-sfs    Run sfs in a container; pass ARGS=... for sfs args"
	@echo "  docker-run-sfl    Run sfl in a container; pass ARGS=... for sfl args"
	@echo ""
	@echo "Override VERSION=x.y.z to embed a release version in the build;"
	@echo "release targets (release, release-assets, manifest, release-zip,"
	@echo "checksums) refuse any version containing -dev unless ALLOW_DEV_STAMP=1."

# Release targets must carry a real version stamp: a bare `make release-assets`,
# `make manifest`, `make release-zip`, or `make checksums` (default
# VERSION=0.2-dev) must never produce dev-stamped release downloads — and, now
# that the update manifest is generated from them, never a manifest claiming
# version 0.2-dev. The guard refuses the default, the empty string, or any
# version containing `-dev` (substring match, not just a suffix — dev-ish
# stamps always fail). The GitHub release workflow always passes an explicit
# VERSION; for genuine local testing set ALLOW_DEV_STAMP=1.
check-release-version:
	@if [ -z "$(ALLOW_DEV_STAMP)" ]; then \
		case "$(VERSION)" in \
			""|*-dev*) \
				echo "refusing dev-stamped release: VERSION='$(VERSION)'" >&2; \
				echo "  pass a real version:  VERSION=0.3.1 make release-assets" >&2; \
				echo "  dev-stamp escape:     ALLOW_DEV_STAMP=1 make release-assets" >&2; \
				exit 1 ;; \
		esac; \
	fi

build: build-sfu build-sfs build-sfl

build-sfu:
	@mkdir -p "$(BIN_DIR)"
	@os=$$(go env GOOS); \
	ext=""; [ "$$os" = "windows" ] && ext=".exe"; \
	out="$(BIN_DIR)/sfu$$ext"; \
	echo "→ $$out"; \
	CGO_ENABLED=0 go build $(BUILD_FLAGS) -o "$$out" $(PKG); \
	echo "Binary written to: $$out"

build-sfs:
	@mkdir -p "$(BIN_DIR)"
	@os=$$(go env GOOS); \
	ext=""; [ "$$os" = "windows" ] && ext=".exe"; \
	out="$(BIN_DIR)/sfs$$ext"; \
	echo "→ $$out"; \
	CGO_ENABLED=0 go build $(BUILD_FLAGS) -o "$$out" $(PKG_SFS); \
	echo "Binary written to: $$out"

build-sfl:
	@mkdir -p "$(BIN_DIR)"
	@os=$$(go env GOOS); \
	ext=""; [ "$$os" = "windows" ] && ext=".exe"; \
	out="$(BIN_DIR)/sfl$$ext"; \
	echo "→ $$out"; \
	CGO_ENABLED=0 go build $(BUILD_FLAGS) -o "$$out" $(PKG_SFL); \
	echo "Binary written to: $$out"


build-all: clean
	@mkdir -p "$(BIN_DIR)"
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		ext=""; [ "$$os" = "windows" ] && ext=".exe"; \
		out_sfu="$(BIN_DIR)/$$os/$$arch/sfu$$ext"; \
		out_sfs="$(BIN_DIR)/$$os/$$arch/sfs$$ext"; \
		out_sfl="$(BIN_DIR)/$$os/$$arch/sfl$$ext"; \
		mkdir -p "$$(dirname "$$out_sfu")"; \
		echo "→ $$out_sfu"; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 \
			go build $(BUILD_FLAGS) -o "$$out_sfu" $(PKG) || exit 1; \
		echo "→ $$out_sfs"; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 \
			go build $(BUILD_FLAGS) -o "$$out_sfs" $(PKG_SFS) || exit 1; \
		echo "→ $$out_sfl"; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 \
			go build $(BUILD_FLAGS) -o "$$out_sfl" $(PKG_SFL) || exit 1; \
	done; \
	echo "→ android/arm64 (static linux/arm64, published as -android-arm64)"; \
	mkdir -p "$(BIN_DIR)/android/arm64"; \
	echo "→ $(BIN_DIR)/android/arm64/sfu"; \
	GOOS=$(ANDROID_GOOS) GOARCH=$(ANDROID_ARCH) CGO_ENABLED=0 \
		go build $(BUILD_FLAGS) -o "$(BIN_DIR)/android/arm64/sfu" $(PKG) || exit 1; \
	echo "→ $(BIN_DIR)/android/arm64/sfs"; \
	GOOS=$(ANDROID_GOOS) GOARCH=$(ANDROID_ARCH) CGO_ENABLED=0 \
		go build $(BUILD_FLAGS) -o "$(BIN_DIR)/android/arm64/sfs" $(PKG_SFS) || exit 1; \
	echo "→ $(BIN_DIR)/android/arm64/sfl"; \
	GOOS=$(ANDROID_GOOS) GOARCH=$(ANDROID_ARCH) CGO_ENABLED=0 \
		go build $(BUILD_FLAGS) -o "$(BIN_DIR)/android/arm64/sfl" $(PKG_SFL) || exit 1; \
	echo "Binaries written under: ./$(BIN_DIR)/"

release: check-release-version build-all checksums release-zip

release-assets: release
	@rm -rf "$(DIST_DIR)"
	@mkdir -p "$(DIST_DIR)"
	@cp "$(BIN_DIR)/linux/amd64/sfu" "$(DIST_DIR)/SnowFastULP-$(VERSION)-linux-amd64"
	@cp "$(BIN_DIR)/darwin/arm64/sfu" "$(DIST_DIR)/SnowFastULP-$(VERSION)-macos-arm64"
	@cp "$(BIN_DIR)/windows/amd64/sfu.exe" "$(DIST_DIR)/SnowFastULP-$(VERSION)-windows-amd64.exe"
	@cp "$(BIN_DIR)/linux/amd64/sfs" "$(DIST_DIR)/SnowFastSearch-$(VERSION)-linux-amd64"
	@cp "$(BIN_DIR)/darwin/arm64/sfs" "$(DIST_DIR)/SnowFastSearch-$(VERSION)-macos-arm64"
	@cp "$(BIN_DIR)/windows/amd64/sfs.exe" "$(DIST_DIR)/SnowFastSearch-$(VERSION)-windows-amd64.exe"
	@cp "$(BIN_DIR)/linux/amd64/sfl" "$(DIST_DIR)/SnowFastLog-$(VERSION)-linux-amd64"
	@cp "$(BIN_DIR)/darwin/arm64/sfl" "$(DIST_DIR)/SnowFastLog-$(VERSION)-macos-arm64"
	@cp "$(BIN_DIR)/windows/amd64/sfl.exe" "$(DIST_DIR)/SnowFastLog-$(VERSION)-windows-amd64.exe"
	@cp "$(BIN_DIR)/android/arm64/sfu" "$(DIST_DIR)/SnowFastULP-$(VERSION)-android-arm64"
	@cp "$(BIN_DIR)/android/arm64/sfs" "$(DIST_DIR)/SnowFastSearch-$(VERSION)-android-arm64"
	@cp "$(BIN_DIR)/android/arm64/sfl" "$(DIST_DIR)/SnowFastLog-$(VERSION)-android-arm64"
	@cp "$(BIN_DIR)/$(RELEASE_ZIP)" "$(DIST_DIR)/$(RELEASE_ZIP)"
	@cd "$(DIST_DIR)" && sha256sum \
		SnowFastULP-$(VERSION)-linux-amd64 \
		SnowFastULP-$(VERSION)-macos-arm64 \
		SnowFastULP-$(VERSION)-windows-amd64.exe \
		SnowFastSearch-$(VERSION)-linux-amd64 \
		SnowFastSearch-$(VERSION)-macos-arm64 \
		SnowFastSearch-$(VERSION)-windows-amd64.exe \
		SnowFastLog-$(VERSION)-linux-amd64 \
		SnowFastLog-$(VERSION)-macos-arm64 \
		SnowFastLog-$(VERSION)-windows-amd64.exe \
		SnowFastULP-$(VERSION)-android-arm64 \
		SnowFastSearch-$(VERSION)-android-arm64 \
		SnowFastLog-$(VERSION)-android-arm64 \
		"$(RELEASE_ZIP)" > SHA256SUMS
	@cat "$(DIST_DIR)/SHA256SUMS"
	@echo "Release downloads: ./$(DIST_DIR)/"

# Generate the update manifest the selfupdate consumers fetch from
# https://sfu-update.snowx.dev/ (internal/selfupdate fetchLatest). Digests
# come from dist/SHA256SUMS — the exact artifact attached to the release —
# so manifest and assets can never disagree, and the required
# <prefix>-<version>-<platform> keys are validated to exist before emission.
# No notes_url: the public tree ships no changelog; clients treat a missing
# notes file as non-fatal (`release notes unavailable`).
#
# Publishing contract: the release workflow attaches dist/update-manifest.json
# to the draft release (after every other asset). After the release is
# published, the release owner mirrors that exact file to
# https://sfu-update.snowx.dev/ — the manifest endpoint is hosted outside this
# repo and is never written by CI, so a published release is always fully
# attached before the manifest can point clients at it.
manifest: check-release-version
	@test -f "$(DIST_DIR)/SHA256SUMS" || { \
		echo "missing $(DIST_DIR)/SHA256SUMS — run make release-assets first" >&2; \
		exit 1; \
	}
	@go run ./cmd/genmanifest "$(VERSION)" "$(DIST_DIR)/SHA256SUMS" > "$(DIST_DIR)/update-manifest.json.tmp" \
		|| { rm -f "$(DIST_DIR)/update-manifest.json.tmp"; exit 1; }
	@mv -f "$(DIST_DIR)/update-manifest.json.tmp" "$(DIST_DIR)/update-manifest.json"
	@cat "$(DIST_DIR)/update-manifest.json"

release-zip: check-release-version
	@command -v zip >/dev/null 2>&1 || { echo "zip is required to package release artifacts" >&2; exit 1; }
	@rm -f "$(BIN_DIR)/$(RELEASE_ZIP)"
	@find "$(BIN_DIR)/linux" "$(BIN_DIR)/darwin" "$(BIN_DIR)/windows" "$(BIN_DIR)/android" "$(BIN_DIR)/SHA256SUMS" \
		-exec touch -d @0 {} +
	@cd "$(BIN_DIR)" && zip -qrX "$(RELEASE_ZIP)" linux darwin windows android SHA256SUMS
	@echo "Release binaries: ./$(BIN_DIR)/"
	@echo "Release zip: ./$(BIN_DIR)/$(RELEASE_ZIP)"

test:
	go test -race ./...
	cd third_party/yekazip && go test ./...

vet:
	go vet ./...
	@unformatted=$$(gofmt -l . | grep -v '^third_party/'); \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt would change:" >&2; \
		echo "$$unformatted" >&2; \
		exit 1; \
	fi

# Checksum list embedded in the release ZIP: covers every cross-compiled
# binary under bin/{linux,darwin,windows,android}, path-keyed (e.g.
# linux/amd64/sfu) and sorted. This is NOT dist/SHA256SUMS — the flat
# release-asset list
# produced by release-assets, attached to the GitHub release, and the sole
# digest source for `make manifest`. release-bins/SHA256SUMS (written by
# sync-release-bins) is a byte copy of this file. The three lists differ
# on purpose in filename shape and file set; only dist/SHA256SUMS is
# authoritative for the update manifest.
checksums: check-release-version
	@cd "$(BIN_DIR)" && rm -f SHA256SUMS && \
		find linux darwin windows android -type f | sort | xargs sha256sum > SHA256SUMS && \
		cat SHA256SUMS

clean:
	@rm -rf sfu sfu.exe sfs sfs.exe sfl sfl.exe "$(DIST_DIR)/" "$(BIN_DIR)/"

# Copy the root example config into internal/config for go:embed manually. Drift is caught by
# TestEmbeddedExampleMatchesRootExample in make test.
example-sync:
	cp config.toml.example internal/config/example.toml

# ─── Docker ────────────────────────────────────────────────────────────────

docker-build:
	docker build --build-arg VERSION=$(VERSION) -t $(DOCKER_IMAGE) .

docker-build-all: check-release-version
	docker build --build-arg VERSION=$(VERSION) --target release --output type=local,dest=. .
	$(MAKE) sync-release-bins
	@echo "Release binaries: ./$(BIN_DIR)/ and ./$(RELEASE_BIN_DIR)/"
	@echo "Release zip: ./$(BIN_DIR)/$(RELEASE_ZIP) (copied to ./$(RELEASE_BIN_DIR)/)"

# Copy freshly built artifacts from ./bin/ into ./release-bins/ for offline
# release preparation. Legacy: release-bins/ was removed from git in a09807b
# ("GH Releases is canonical now"); dist/ assets produced by release-assets
# are the canonical release artifacts. Kept because docker-build-all still
# drives it. Preserves release-bins/README.md; overwrites platform binaries,
# SHA256SUMS (a byte copy of bin/SHA256SUMS — the zip-internal list, NOT the
# dist/SHA256SUMS the manifest is generated from), and zip.
sync-release-bins:
	@test -f "$(BIN_DIR)/linux/amd64/sfu" || { \
		echo "missing $(BIN_DIR)/linux/amd64/sfu — run make docker-build-all or make release first" >&2; \
		exit 1; \
	}
	@test -f "$(BIN_DIR)/linux/amd64/sfs" || { \
		echo "missing $(BIN_DIR)/linux/amd64/sfs — run make docker-build-all or make release first" >&2; \
		exit 1; \
	}
	@test -f "$(BIN_DIR)/linux/amd64/sfl" || { \
		echo "missing $(BIN_DIR)/linux/amd64/sfl — run make docker-build-all or make release first" >&2; \
		exit 1; \
	}
	@test -f "$(BIN_DIR)/$(RELEASE_ZIP)" || { \
		echo "missing $(BIN_DIR)/$(RELEASE_ZIP)" >&2; exit 1; \
	}
	@mkdir -p \
		"$(RELEASE_BIN_DIR)/linux/amd64" \
		"$(RELEASE_BIN_DIR)/darwin/arm64" \
		"$(RELEASE_BIN_DIR)/windows/amd64" \
		"$(RELEASE_BIN_DIR)/android/arm64"
	cp -a "$(BIN_DIR)/linux/amd64/sfu" "$(RELEASE_BIN_DIR)/linux/amd64/"
	cp -a "$(BIN_DIR)/darwin/arm64/sfu" "$(RELEASE_BIN_DIR)/darwin/arm64/"
	cp -a "$(BIN_DIR)/windows/amd64/sfu.exe" "$(RELEASE_BIN_DIR)/windows/amd64/"
	cp -a "$(BIN_DIR)/android/arm64/sfu" "$(RELEASE_BIN_DIR)/android/arm64/"
	cp -a "$(BIN_DIR)/linux/amd64/sfs" "$(RELEASE_BIN_DIR)/linux/amd64/"
	cp -a "$(BIN_DIR)/darwin/arm64/sfs" "$(RELEASE_BIN_DIR)/darwin/arm64/"
	cp -a "$(BIN_DIR)/windows/amd64/sfs.exe" "$(RELEASE_BIN_DIR)/windows/amd64/"
	cp -a "$(BIN_DIR)/android/arm64/sfs" "$(RELEASE_BIN_DIR)/android/arm64/"
	cp -a "$(BIN_DIR)/linux/amd64/sfl" "$(RELEASE_BIN_DIR)/linux/amd64/"
	cp -a "$(BIN_DIR)/darwin/arm64/sfl" "$(RELEASE_BIN_DIR)/darwin/arm64/"
	cp -a "$(BIN_DIR)/windows/amd64/sfl.exe" "$(RELEASE_BIN_DIR)/windows/amd64/"
	cp -a "$(BIN_DIR)/android/arm64/sfl" "$(RELEASE_BIN_DIR)/android/arm64/"
	cp -a "$(BIN_DIR)/SHA256SUMS" "$(BIN_DIR)/$(RELEASE_ZIP)" "$(RELEASE_BIN_DIR)/"
	@echo "→ synced ./$(BIN_DIR)/ → ./$(RELEASE_BIN_DIR)/ (README.md unchanged)"

# Pass-through args via ARGS=... e.g. `make docker-run ARGS=/work/inputs/`.
# The current host dir is bind-mounted at /work; outputs (./done/) land on
# the host as if you'd run sfu natively.
docker-run: docker-build
	docker run --rm --user "$$(id -u):$$(id -g)" -v "$(PWD):/work" $(DOCKER_IMAGE) $(ARGS)

docker-run-sfs: docker-build
	docker run --rm \
		--user "$$(id -u):$$(id -g)" -v "$(PWD):/work" $(DOCKER_IMAGE) sfs $(ARGS)

docker-run-sfl: docker-build
	docker run --rm \
		--user "$$(id -u):$$(id -g)" -v "$(PWD):/work" $(DOCKER_IMAGE) sfl $(ARGS)
