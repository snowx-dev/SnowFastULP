#!/usr/bin/env bash
# Cross-compile sfu/sfs/sfl for Windows and stage files for the dockur VM shared folder.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
OUT="$ROOT/demo/windows-vm/shared"
BIN="$OUT/bin"
VERSION="${VERSION:-0.2-dev}"

mkdir -p "$BIN"

BUILD_FLAGS=(
  -trimpath
  -buildvcs=false
  "-ldflags=-s -w -buildid= -X github.com/snowx-dev/SnowFastULP/internal/version.String=${VERSION}"
)

for cmd in sfu sfs sfl; do
  echo "→ windows/amd64 ${cmd}.exe"
  CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build "${BUILD_FLAGS[@]}" \
    -o "$BIN/${cmd}.exe" "$ROOT/cmd/$cmd"
done

cp "$ROOT/demo/windows-vm/smoke-test.ps1" "$OUT/"
cp "$ROOT/demo/windows-vm/test-input.txt" "$OUT/"
cp "$ROOT/demo/windows-vm/smoke-test.ps1" "$ROOT/demo/windows-vm/oem/"
cp "$ROOT/demo/windows-vm/test-input.txt" "$ROOT/demo/windows-vm/oem/"
cp "$ROOT/scripts/install.ps1" "$OUT/"

cat <<EOF

Built Windows binaries in:
  $BIN

Next:
  1. Start the VM:  (cd demo/windows-vm && docker compose up -d)
  2. RDP to localhost:3389 (user Docker / admin unless overridden)
  3. In Windows, run:
       powershell -ExecutionPolicy Bypass -File C:\Users\Docker\Desktop\Shared\smoke-test.ps1

Log: C:\SnowFastMerge\smoke.log
EOF
