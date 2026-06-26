#!/usr/bin/env bash
# Build a v0.1-format library (unsorted v2 .idx sidecars) from real ULP dumps
# for manually testing the dev-branch v2→v3 migration TUI.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
HERE="$(cd "$(dirname "$0")" && pwd)"
BIN_DIR="$HERE/.bin"
SCRATCH="$HERE/scratch"
LIB="$HERE/v1-library"
WORKTREE="/tmp/sfu-v01-build-$$"

SOURCE_DIR="${SOURCE_DIR:-/run/media/bigboi/b992e755-c9c0-4d4d-8ed5-81cbb85ccec5/Data_Archive/ulp/raws/txt}"
SAMPLE_A="${SAMPLE_A:-$SOURCE_DIR/0623_ulp.txt}"
SAMPLE_B="${SAMPLE_B:-$SOURCE_DIR/🔒 June26. - mo-on.cloud - #842.txt}"
# 0 = entire file (default). Set to N to slice for quicker smoke tests.
SAMPLE_A_LINES="${SAMPLE_A_LINES:-0}"
SAMPLE_B_LINES="${SAMPLE_B_LINES:-0}"
SPLIT_A="${SPLIT_A:-250000}"
SPLIT_B="${SPLIT_B:-250000}"
# Second-run input for dev migration test. 0 = reuse full SAMPLE_A in place.
PROBE_LINES="${PROBE_LINES:-0}"

need() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "prepare: missing $1" >&2
    exit 1
  }
}

need git
need go
need python3

[[ -f "$SAMPLE_A" ]] || {
  echo "prepare: sample A not found: $SAMPLE_A" >&2
  exit 1
}
[[ -f "$SAMPLE_B" ]] || {
  echo "prepare: sample B not found: $SAMPLE_B" >&2
  exit 1
}

mkdir -p "$BIN_DIR" "$SCRATCH"

build_v01_sfu() {
  local out="$BIN_DIR/sfu-v0.1"
  if [[ -x "$out" ]]; then
    return 0
  fi
  echo "prepare: building sfu v0.1 from git tag (one-time)"
  trap 'git worktree remove -f "$WORKTREE" 2>/dev/null || true' EXIT
  git -C "$ROOT" worktree add -f "$WORKTREE" v0.1 >/dev/null
  (
    cd "$WORKTREE"
    CGO_ENABLED=0 go build -trimpath -buildvcs=false \
      -ldflags="-s -w -X github.com/snowx-dev/SnowFastULP/internal/version.String=0.1" \
      -o "$out" ./cmd/sfu
  )
  git -C "$ROOT" worktree remove -f "$WORKTREE"
  trap - EXIT
}

# resolve_input SRC DST LINES → prints path to feed sfu (status on stderr)
resolve_input() {
  local src=$1 dst=$2 lines=$3
  if [[ "$lines" == "0" ]]; then
    echo "prepare: using full file $(basename "$src")" >&2
    printf '%s\n' "$src"
    return 0
  fi
  need head
  echo "prepare: slicing $(basename "$src") → $(basename "$dst") (${lines} lines)" >&2
  head -n "$lines" "$src" >"$dst"
  printf '%s\n' "$dst"
}

build_library() {
  local sfu=$1 input_a=$2 input_b=$3
  rm -rf "$LIB"
  mkdir -p "$LIB"

  echo "prepare: ingest A → $LIB (${SPLIT_A}-line parts) — this may take a while"
  "$sfu" -no-tui "$input_a" -od "$LIB/" -zst -split-zst "$SPLIT_A"

  echo "prepare: ingest B → $LIB (${SPLIT_B}-line parts, exercises library dedup)"
  "$sfu" -no-tui "$input_b" -od "$LIB/" -zst -split-zst "$SPLIT_B"
}

write_probe() {
  local probe_src=$1
  rm -f "$SCRATCH/migration-probe.txt"
  if [[ "$PROBE_LINES" == "0" ]]; then
    ln -sf "$probe_src" "$SCRATCH/migration-probe.txt"
    echo "prepare: migration probe → symlink to $(basename "$probe_src") (full file)"
  else
    need head
    head -n "$PROBE_LINES" "$probe_src" >"$SCRATCH/migration-probe.txt"
    echo "prepare: migration probe → $SCRATCH/migration-probe.txt (${PROBE_LINES} lines)"
  fi
}

verify_v2() {
  python3 - "$LIB" <<'PY'
import struct, glob, os, sys
lib = sys.argv[1]
idx = sorted(glob.glob(os.path.join(lib, "sfu_dedup_idx", "*.idx")))
if not idx:
    raise SystemExit("no .idx sidecars found")
vers = set()
unsorted = 0
for p in idx:
    with open(p, "rb") as f:
        ver = struct.unpack("<H", f.read(6)[4:6])[0]
    vers.add(ver)
    n = (os.path.getsize(p) - 32) // 8
    with open(p, "rb") as f:
        f.seek(32)
        keys = [struct.unpack("<Q", f.read(8))[0] for _ in range(min(50, n))]
    if keys != sorted(keys):
        unsorted += 1
if vers != {2}:
    raise SystemExit(f"expected only v2 sidecars, got {vers}")
if unsorted == 0:
    raise SystemExit("sidecars look sorted — not a legacy v0.1 library")
parts = len(glob.glob(os.path.join(lib, "*.zst")))
print(f"prepare: OK — {parts} archive parts, {len(idx)} v2 sidecars ({unsorted} confirmed unsorted)")
PY
}

lines_label() {
  local lines=$1 file=$2
  if [[ "$lines" == "0" ]]; then
    wc -l <"$file" | awk '{print $1 " (full file)"}'
  else
    printf '%s\n' "$lines"
  fi
}

write_manifest() {
  local parts idx a_label b_label
  parts=$(find "$LIB" -maxdepth 1 -name '*.zst' | wc -l)
  idx=$(find "$LIB/sfu_dedup_idx" -name '*.idx' 2>/dev/null | wc -l)
  a_label=$(lines_label "$SAMPLE_A_LINES" "$SAMPLE_A")
  b_label=$(lines_label "$SAMPLE_B_LINES" "$SAMPLE_B")
  cat >"$HERE/MANIFEST.txt" <<EOF
SnowFastMerge migration visual test fixture
Generated: $(date -u +%Y-%m-%dT%H:%M:%SZ)

Source A: $SAMPLE_A ($a_label lines)
Source B: $SAMPLE_B ($b_label lines)
Library:  $LIB
Parts:    $parts .zst archives
Sidecars: $idx legacy v2 .idx files (unsorted)

Second-run probe (re-ingest, triggers v2→v3 migration on dev sfu):
  $SCRATCH/migration-probe.txt

Do NOT run dev-branch sfu against this library until you are ready to
observe migration — the v2→v3 upgrade is one-time per sidecar.
EOF
}

echo "prepare: source dir $SOURCE_DIR"
build_v01_sfu
INPUT_A=$(resolve_input "$SAMPLE_A" "$SCRATCH/sample-a.txt" "$SAMPLE_A_LINES")
INPUT_B=$(resolve_input "$SAMPLE_B" "$SCRATCH/sample-b.txt" "$SAMPLE_B_LINES")
build_library "$BIN_DIR/sfu-v0.1" "$INPUT_A" "$INPUT_B"
write_probe "$INPUT_A"
verify_v2
write_manifest
echo "prepare: ready at $LIB"
cat "$HERE/MANIFEST.txt"
