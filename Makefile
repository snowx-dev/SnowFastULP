# SnowFastULP build / test / docker shortcuts.
#
# Reproducible-build flags:
#   -trimpath        strip local filesystem paths from the binary
#   -buildvcs=false  strip git tag/commit metadata
#   -s -w            strip symbol + DWARF tables
#   -buildid=        clear Go's per-build identifier
# Combined with CGO_ENABLED=0, same source + same Go version → same SHA256.

VERSION       ?= 0.2-dev
BUILD_FLAGS   := -trimpath -buildvcs=false -ldflags="-s -w -buildid= -X github.com/snowx-dev/SnowFastULP/internal/version.String=$(VERSION)"
PKG           := ./cmd/sfu
PKG_SFS       := ./cmd/sfs
PKG_SFL       := ./cmd/sfl
PLATFORMS     := linux/amd64 darwin/arm64 windows/amd64
BIN_DIR         ?= bin
RELEASE_BIN_DIR ?= release-bins
DIST_DIR        ?= dist
RELEASE_ZIP     ?= SnowFastULP-$(VERSION)-binaries.zip
DOCKER_IMAGE  ?= sfu:local
GO_DOCKER_IMAGE ?= golang:1.25-alpine

.PHONY: build build-sfu build-sfs build-sfl build-sfl-secrets build-sfl-secrets-fast build-all release release-assets release-zip test vet clean checksums \
	docker-build docker-build-all sync-release-bins docker-run docker-run-sfs docker-run-sfl help

# Default target: print available targets when invoked as bare `make`.
help:
	@echo "Targets:"
	@echo "  build           Build sfu, sfs, and sfl for the current platform into ./$(BIN_DIR)/"
	@echo "  build-sfu       Build sfu only"
	@echo "  build-sfs       Build sfs only"
	@echo "  build-sfl       Build sfl only (lean; no secret scanning)"
	@echo "  build-sfl-secrets  Build sfl with the Titus secret scanner (-tags secrets) → $(BIN_DIR)/sfl"
	@echo "  build-sfl-secrets-fast  Build sfl with the hs matcher (-tags secrets,vectorscan; CGO; needs Vectorscan/Hyperscan) → $(BIN_DIR)/sfl"
	@echo "  build-all       Cross-compile both binaries for primary platforms"
	@echo "  release         Build primary platforms and ./$(BIN_DIR)/$(RELEASE_ZIP)"
	@echo "  release-assets  Build flat release downloads into ./$(DIST_DIR)/"
	@echo "  test            go test -race ./..."
	@echo "  vet             go vet + gofmt clean check"
	@echo "  checksums       SHA256SUMS for release binaries in ./$(BIN_DIR)/"
	@echo "  clean           Remove build artifacts"
	@echo ""
	@echo "  docker-build      Build a runtime image ($(DOCKER_IMAGE)) with sfu, sfs, and sfl"
	@echo "  docker-build-all  Build release binaries via Docker; sync ./$(BIN_DIR)/ → ./$(RELEASE_BIN_DIR)/"
	@echo "  docker-run        Run sfu in a container; pass ARGS=... for sfu args"
	@echo "  docker-run-sfs    Run sfs in a container; pass ARGS=... for sfs args"
	@echo "  docker-run-sfl    Run sfl in a container; pass ARGS=... for sfl args"
	@echo ""
	@echo "Override VERSION=0.1 to embed a release version in the build."

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

# Same sfl, but with `-tags secrets` so the Titus scanner is linked in (enables
# `sfl -secrets`; a larger binary). Writes bin/sfl, replacing the lean build.
build-sfl-secrets:
	@mkdir -p "$(BIN_DIR)"
	@os=$$(go env GOOS); \
	ext=""; [ "$$os" = "windows" ] && ext=".exe"; \
	out="$(BIN_DIR)/sfl$$ext"; \
	echo "→ $$out (-tags secrets)"; \
	CGO_ENABLED=0 go build $(BUILD_FLAGS) -tags secrets -o "$$out" $(PKG_SFL); \
	echo "Binary written to: $$out"

# Same as build-sfl-secrets, but with the high-performance hs matcher
# (-tags vectorscan) for faster scanning on very large member-count dumps.
# Requires the libhs C library (Vectorscan, or Hyperscan on x86_64) installed on
# the build machine at build AND runtime, so CGO is on: a native build only — no
# cross-compile, not byte-reproducible, not a static binary. It pays an upfront
# rule-compile cost, so it is only faster than the portable sfl-secrets on big
# workloads. See docs/guides/secrets-scanning for per-OS install + when-to-use.
build-sfl-secrets-fast:
	@mkdir -p "$(BIN_DIR)"
	@os=$$(go env GOOS); \
	ext=""; [ "$$os" = "windows" ] && ext=".exe"; \
	out="$(BIN_DIR)/sfl$$ext"; \
	echo "→ $$out (-tags secrets,vectorscan; CGO + libhs, needs Vectorscan/Hyperscan installed)"; \
	CGO_ENABLED=1 go build $(BUILD_FLAGS) -tags secrets,vectorscan -o "$$out" $(PKG_SFL); \
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
	echo "Binaries written under: ./$(BIN_DIR)/"

release: build-all checksums release-zip

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
		"$(RELEASE_ZIP)" > SHA256SUMS
	@cat "$(DIST_DIR)/SHA256SUMS"
	@echo "Release downloads: ./$(DIST_DIR)/"

release-zip:
	@command -v zip >/dev/null 2>&1 || { echo "zip is required to package release artifacts" >&2; exit 1; }
	@rm -f "$(BIN_DIR)/$(RELEASE_ZIP)"
	@find "$(BIN_DIR)/linux" "$(BIN_DIR)/darwin" "$(BIN_DIR)/windows" "$(BIN_DIR)/SHA256SUMS" \
		-exec touch -d "1970-01-01 00:00:00 UTC" {} +
	@cd "$(BIN_DIR)" && zip -qrX "$(RELEASE_ZIP)" linux darwin windows SHA256SUMS
	@echo "Release binaries: ./$(BIN_DIR)/"
	@echo "Release zip: ./$(BIN_DIR)/$(RELEASE_ZIP)"

test:
	go test -race ./...

vet:
	go vet ./...
	@unformatted=$$(gofmt -l . | grep -v '^third_party/'); \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt would change:" >&2; \
		echo "$$unformatted" >&2; \
		exit 1; \
	fi

checksums:
	@cd "$(BIN_DIR)" && rm -f SHA256SUMS && \
		find linux darwin windows -type f | sort | xargs sha256sum > SHA256SUMS && \
		cat SHA256SUMS

clean:
	@rm -rf sfu sfu.exe sfs sfs.exe sfl sfl.exe sfl-secrets sfl-secrets.exe sfl-secrets-fast sfl-secrets-fast.exe "$(DIST_DIR)/" "$(BIN_DIR)/"

# ─── Docker ────────────────────────────────────────────────────────────────

docker-build:
	docker build --build-arg VERSION=$(VERSION) -t $(DOCKER_IMAGE) .

docker-build-all:
	docker build --build-arg VERSION=$(VERSION) --target release --output type=local,dest=. .
	$(MAKE) sync-release-bins
	@echo "Release binaries: ./$(BIN_DIR)/ and ./$(RELEASE_BIN_DIR)/"
	@echo "Release zip: ./$(BIN_DIR)/$(RELEASE_ZIP) (copied to ./$(RELEASE_BIN_DIR)/)"

# Copy freshly built artifacts from ./bin/ into ./release-bins/ for git push.
# Preserves release-bins/README.md; overwrites platform binaries, SHA256SUMS, and zip.
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
		"$(RELEASE_BIN_DIR)/windows/amd64"
	cp -a "$(BIN_DIR)/linux/amd64/sfu" "$(RELEASE_BIN_DIR)/linux/amd64/"
	cp -a "$(BIN_DIR)/darwin/arm64/sfu" "$(RELEASE_BIN_DIR)/darwin/arm64/"
	cp -a "$(BIN_DIR)/windows/amd64/sfu.exe" "$(RELEASE_BIN_DIR)/windows/amd64/"
	cp -a "$(BIN_DIR)/linux/amd64/sfs" "$(RELEASE_BIN_DIR)/linux/amd64/"
	cp -a "$(BIN_DIR)/darwin/arm64/sfs" "$(RELEASE_BIN_DIR)/darwin/arm64/"
	cp -a "$(BIN_DIR)/windows/amd64/sfs.exe" "$(RELEASE_BIN_DIR)/windows/amd64/"
	cp -a "$(BIN_DIR)/linux/amd64/sfl" "$(RELEASE_BIN_DIR)/linux/amd64/"
	cp -a "$(BIN_DIR)/darwin/arm64/sfl" "$(RELEASE_BIN_DIR)/darwin/arm64/"
	cp -a "$(BIN_DIR)/windows/amd64/sfl.exe" "$(RELEASE_BIN_DIR)/windows/amd64/"
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
