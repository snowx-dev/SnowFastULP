#!/usr/bin/env bash
# Regenerate the fake release asset payloads under assets/ (v0.2.0 and
# v0.3.1, linux-amd64 and windows-amd64) and print the SHA256SUMS lines.
# Fake payloads are plain text: install.sh runs them as shell scripts,
# install.ps1 tests only assert file content (a fake .exe cannot execute).
# After changing any payload, refresh the committed SHA256SUMS-* fixtures
# with this script's output.
set -euo pipefail
cd "$(dirname "$0")"

body() {
  printf '# fake SnowFastULP release binary (%s)\n' "$1"
}

for ver in 0.2.0 0.3.1; do
  for spec in \
    "SnowFastULP:linux-amd64:" \
    "SnowFastSearch:linux-amd64:" \
    "SnowFastLog:linux-amd64:" \
    "SnowFastULP:windows-amd64:.exe" \
    "SnowFastSearch:windows-amd64:.exe" \
    "SnowFastLog:windows-amd64:.exe"; do
    name="${spec%%:*}"; rest="${spec#*:}"; plat="${rest%%:*}"; ext="${rest#*:}"
    body "$name-$ver-$plat" > "$name-$ver-$plat$ext"
  done
done

# Checksums in the flat SHA256SUMS format (matches release.yml).
for ver in 0.2.0 0.3.1; do
  sha256sum \
    "SnowFastULP-$ver-linux-amd64" \
    "SnowFastSearch-$ver-linux-amd64" \
    "SnowFastLog-$ver-linux-amd64" \
    "SnowFastULP-$ver-windows-amd64.exe" \
    "SnowFastSearch-$ver-windows-amd64.exe" \
    "SnowFastLog-$ver-windows-amd64.exe"
done
