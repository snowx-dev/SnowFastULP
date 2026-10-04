#!/usr/bin/env bash
# Test harness for scripts/install.sh.
#
# Serves fixture manifests + fake release assets over localhost
# (scripts/tests/server.py — python3 is acceptable in tests only, never a
# runtime dependency of the installer), points install.sh at it via
# SNOWFAST_UPDATE_URL, and runs every case in a sandbox (own HOME, PATH,
# XDG dirs).
#
# Usage: scripts/tests/test.sh [-k <case-name>]
set -uo pipefail

TESTS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
INSTALLER="$TESTS_DIR/../install.sh"
FIXTURES="$TESTS_DIR/fixtures"
SERVER="$TESTS_DIR/server.py"

SANDBOX="$(mktemp -d)"
trap 'kill "$SERVER_PID" 2>/dev/null; rm -rf "$SANDBOX"' EXIT

PASS=0
FAIL=0

say() { printf '%s\n' "$*"; }
fail() { say "FAIL: $*"; FAIL=$((FAIL + 1)); }
pass() { say "ok   - $*"; PASS=$((PASS + 1)); }

require() {
  command -v "$1" >/dev/null 2>&1 || { say "missing required command: $1"; exit 1; }
}
require bash
require curl
require python3
require sha256sum
require mktemp
require chmod

# ---------------------------------------------------------------------------
# Sandbox + HTTP server
# ---------------------------------------------------------------------------
PORT=""
SERVER_PID=""

pick_port() {
  python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
}

start_server() {
  PORT="$(pick_port)"
  python3 "$SERVER" "$PORT" "$FIXTURES" >/dev/null 2>&1 &
  SERVER_PID=$!
  # Wait for readiness (up to ~5s).
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    if curl -fsS "http://127.0.0.1:$PORT/manifest" -o /dev/null 2>/dev/null; then
      return 0
    fi
    sleep 0.5
  done
  say "test server failed to start"
  exit 1
}

# new_home <case>: fresh sandbox HOME/XDG state + a PATH that contains
# neither install dirs nor any PATH-dir the auto-detection could adopt.
new_sandbox() {
  local case_name="$1"
  mkdir -p "$SANDBOX/$case_name/home" "$SANDBOX/$case_name/bin" "$SANDBOX/$case_name/xdg" "$SANDBOX/$case_name/state"
  export HOME="$SANDBOX/$case_name/home"
  export XDG_CONFIG_HOME="$SANDBOX/$case_name/xdg/config"
  export XDG_DATA_HOME="$SANDBOX/$case_name/xdg/data"
  export SHELL=/bin/bash
  export NO_COLOR=1
  unset SNOWFAST_INSTALL_DIR SNOWFAST_VERSION SNOWFAST_UPDATE_URL SNOWFAST_REF SNOWFAST_RAW_BASE 2>/dev/null || true
  TEST_ENV_PATH="/usr/bin:/bin"
}

run_installer() {
  # PATH restricted so auto-detection can only find what the case sets up;
  # env -i gives a hermetic environment. Selected vars (SNOWFAST_*) are
  # passed through explicitly per case.
  env -i \
    HOME="$HOME" \
    XDG_CONFIG_HOME="$XDG_CONFIG_HOME" \
    XDG_DATA_HOME="$XDG_DATA_HOME" \
    SHELL="$SHELL" \
    NO_COLOR=1 \
    PATH="$TEST_ENV_PATH:/tmp/shellcheck-dl/bin:/usr/bin:/bin" \
    "$@"
}

# fresh_fake_profile <home>: a profile file install.sh may append to.
fresh_fake_profile() {
  mkdir -p "$HOME"
  : > "$HOME/.bashrc"
}

assert_file_executable() {
  [ -f "$1" ] && [ -x "$1" ]
}


case_latest() {
  say ""
  say "==> case: latest install (pretty manifest)"
  new_sandbox latest
  mkdir -p "$HOME/.local/bin"
  fresh_fake_profile
  run_installer SNOWFAST_UPDATE_URL="http://127.0.0.1:$PORT/manifest" \
    SNOWFAST_RELEASE_BASE="http://127.0.0.1:$PORT/releases/download" \
    SNOWFAST_RAW_BASE="http://127.0.0.1:$PORT/raw" \
    bash "$INSTALLER" >"$SANDBOX/latest/out.log" 2>&1
  local rc=$?
  [ "$rc" -eq 0 ] || { fail "installer exit code $rc"; sed -n '1,30p' "$SANDBOX/latest/out.log"; return; }
  assert_file_executable "$HOME/.local/bin/sfu" || { fail "sfu not executable"; return; }
  assert_file_executable "$HOME/.local/bin/sfs" || { fail "sfs not executable"; return; }
  assert_file_executable "$HOME/.local/bin/sfl" || { fail "sfl not executable"; return; }
  grep -q "SnowFastULP-0.2.0-linux-amd64)" "$HOME/.local/bin/sfu" || { fail "sfu content wrong"; return; }
  grep -Fq "added $HOME/.local/bin to PATH in $HOME/.bashrc" "$SANDBOX/latest/out.log" \
    || grep -Fq "added $HOME/.local/bin to PATH" "$SANDBOX/latest/out.log" || { fail "PATH not appended"; return; }
  grep -Fq "SnowFastULP installer" "$HOME/.bashrc" || { fail "PATH block missing from profile"; return; }
  pass "latest install"
}

case_pinned() {
  # W2 regression: SNOWFAST_VERSION=0.3.1 against a LATEST manifest for
  # 0.2.0 — checksums must come from the pinned tag's SHA256SUMS.
  say ""
  say "==> case: pinned install via SNOWFAST_VERSION (W2 regression)"
  new_sandbox pinned
  mkdir -p "$HOME/.local/bin"
  fresh_fake_profile
  run_installer \
    SNOWFAST_UPDATE_URL="http://127.0.0.1:$PORT/manifest" \
    SNOWFAST_RELEASE_BASE="http://127.0.0.1:$PORT/releases/download" \
    SNOWFAST_RAW_BASE="http://127.0.0.1:$PORT/raw" \
    SNOWFAST_VERSION=0.3.1 \
    bash "$INSTALLER" >"$SANDBOX/pinned/out.log" 2>&1
  local rc=$?
  [ "$rc" -eq 0 ] || { fail "installer exit code $rc (W2 not fixed?)"; sed -n '1,30p' "$SANDBOX/pinned/out.log"; return; }
  grep -q "SnowFastULP-0.3.1-linux-amd64)" "$HOME/.local/bin/sfu" || { fail "wrong version installed"; return; }
  pass "pinned install (W2)"
}

case_minified() {
  # W3 regression: minified single-line manifest must parse.
  say ""
  say "==> case: minified manifest (W3 regression)"
  new_sandbox minified
  mkdir -p "$HOME/.local/bin"
  fresh_fake_profile
  run_installer \
    SNOWFAST_UPDATE_URL="http://127.0.0.1:$PORT/manifest-minified" \
    SNOWFAST_RELEASE_BASE="http://127.0.0.1:$PORT/releases/download" \
    SNOWFAST_RAW_BASE="http://127.0.0.1:$PORT/raw" \
    bash "$INSTALLER" >"$SANDBOX/minified/out.log" 2>&1
  local rc=$?
  [ "$rc" -eq 0 ] || { fail "installer exit code $rc (W3 not fixed?)"; sed -n '1,30p' "$SANDBOX/minified/out.log"; return; }
  grep -q "SnowFastULP-0.3.1-linux-amd64)" "$HOME/.local/bin/sfu" || { fail "wrong version installed"; return; }
  pass "minified manifest (W3)"
}

case_dry_run() {
  say ""
  say "==> case: dry run"
  new_sandbox dryrun
  mkdir -p "$HOME/.local/bin"
  fresh_fake_profile
  run_installer \
    SNOWFAST_UPDATE_URL="http://127.0.0.1:$PORT/manifest" \
    SNOWFAST_RELEASE_BASE="http://127.0.0.1:$PORT/releases/download" \
    SNOWFAST_RAW_BASE="http://127.0.0.1:$PORT/raw" \
    bash "$INSTALLER" --dry-run >"$SANDBOX/dryrun/out.log" 2>&1
  local rc=$?
  [ "$rc" -eq 0 ] || { fail "installer exit code $rc"; sed -n '1,30p' "$SANDBOX/dryrun/out.log"; return; }
  grep -Fq "[ok] dry run complete" "$SANDBOX/dryrun/out.log" || { fail "dry-run banner missing"; return; }
  [ ! -e "$HOME/.local/bin/sfu" ] || { fail "dry run must not install"; return; }
  [ ! -e "$XDG_CONFIG_HOME/snowfast/config.toml" ] || { fail "dry run must not write config"; return; }
  grep -q "SnowFastULP installer" "$HOME/.bashrc" && { fail "dry run must not touch profile"; return; }
  grep -Fq "releases/download/v0.2.0/SnowFastULP-0.2.0-linux-amd64" "$SANDBOX/dryrun/out.log" \
    || { fail "dry run does not list release asset URL"; return; }
  pass "dry run"
}

case_dry_run_minified() {
  say ""
  say "==> case: dry run with minified manifest"
  new_sandbox dryrunmin
  mkdir -p "$HOME/.local/bin"
  run_installer \
    SNOWFAST_UPDATE_URL="http://127.0.0.1:$PORT/manifest-minified" \
    SNOWFAST_RELEASE_BASE="http://127.0.0.1:$PORT/releases/download" \
    SNOWFAST_RAW_BASE="http://127.0.0.1:$PORT/raw" \
    bash "$INSTALLER" --dry-run >"$SANDBOX/dryrunmin/out.log" 2>&1
  local rc=$?
  [ "$rc" -eq 0 ] || { fail "installer exit code $rc"; return; }
  grep -Fq "[ok] dry run complete" "$SANDBOX/dryrunmin/out.log" || { fail "dry-run banner missing"; return; }
  pass "dry run (minified)"
}

case_ref_default_is_tag() {
  # W11 regression: the config example must be fetched from the release
  # tag ref (v0.3.1 here), not main. The fixture server echoes the ref it
  # served in the body; assert the pinned tag's ref was used.
  say ""
  say "==> case: config example fetched from pinned tag ref (W11)"
  new_sandbox refdefault
  mkdir -p "$HOME/.local/bin"
  run_installer \
    SNOWFAST_UPDATE_URL="http://127.0.0.1:$PORT/manifest" \
    SNOWFAST_RELEASE_BASE="http://127.0.0.1:$PORT/releases/download" \
    SNOWFAST_RAW_BASE="http://127.0.0.1:$PORT/raw" \
    SNOWFAST_VERSION=0.3.1 \
    bash "$INSTALLER" >"$SANDBOX/refdefault/out.log" 2>&1
  local rc=$?
  [ "$rc" -eq 0 ] || { fail "installer exit code $rc"; sed -n '1,30p' "$SANDBOX/refdefault/out.log"; return; }
  [ -f "$XDG_CONFIG_HOME/snowfast/config.toml" ] || { fail "config not created"; return; }
  grep -Fq "served for ref: v0.3.1" "$XDG_CONFIG_HOME/snowfast/config.toml" \
    || { fail "config not fetched from release tag ref"; return; }
  pass "config example fetched for pinned tag (W11)"
}

case_ref_override_respected() {
  # Explicit SNOWFAST_REF must override the tag default.
  say ""
  say "==> case: SNOWFAST_REF override"
  new_sandbox refoverride
  mkdir -p "$HOME/.local/bin"
  run_installer \
    SNOWFAST_UPDATE_URL="http://127.0.0.1:$PORT/manifest" \
    SNOWFAST_RELEASE_BASE="http://127.0.0.1:$PORT/releases/download" \
    SNOWFAST_RAW_BASE="http://127.0.0.1:$PORT/raw" \
    SNOWFAST_REF=some-branch \
    bash "$INSTALLER" >"$SANDBOX/refoverride/out.log" 2>&1
  local rc=$?
  [ "$rc" -eq 0 ] || { fail "installer exit code $rc"; return; }
  grep -Fq "served for ref: some-branch" "$XDG_CONFIG_HOME/snowfast/config.toml" \
    || { fail "SNOWFAST_REF override not honored"; return; }
  pass "SNOWFAST_REF override"
}

case_missing_sums_fails_clearly() {
  say ""
  say "==> case: missing SHA256SUMS fails with tag in message"
  new_sandbox nosums
  mkdir -p "$HOME/.local/bin"
  # v9.9.9 has no SHA256SUMS-9.9.9 fixture -> 404.
  run_installer \
    SNOWFAST_UPDATE_URL="http://127.0.0.1:$PORT/manifest" \
    SNOWFAST_RELEASE_BASE="http://127.0.0.1:$PORT/releases/download" \
    SNOWFAST_RAW_BASE="http://127.0.0.1:$PORT/raw" \
    SNOWFAST_VERSION=9.9.9 \
    bash "$INSTALLER" >"$SANDBOX/nosums/out.log" 2>&1
  local rc=$?
  [ "$rc" -ne 0 ] || { fail "installer must fail when SHA256SUMS is missing"; return; }
  grep -Fq "SHA256SUMS" "$SANDBOX/nosums/out.log" || { fail "error does not name SHA256SUMS"; return; }
  grep -Fq "v9.9.9" "$SANDBOX/nosums/out.log" || { fail "error does not name the tag"; return; }
  pass "missing SHA256SUMS failure names tag"
}

case_checksum_mismatch_fails() {
  say ""
  say "==> case: corrupted asset fails checksum"
  new_sandbox badsum
  mkdir -p "$HOME/.local/bin"
  # Serve a mutated fixture copy: payload appended -> served asset no
  # longer matches the committed SHA256SUMS.
  local mutated="$SANDBOX/badsum/fixtures"
  cp -r "$FIXTURES" "$mutated"
  printf 'X' >> "$mutated/assets/SnowFastULP-0.3.1-linux-amd64"
  local bad_port bad_pid
  bad_port="$(pick_port)"
  python3 "$SERVER" "$bad_port" "$mutated" >/dev/null 2>&1 &
  bad_pid=$!
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    curl -fsS "http://127.0.0.1:$bad_port/manifest" -o /dev/null 2>/dev/null && break
    sleep 0.5
  done
  run_installer \
    SNOWFAST_UPDATE_URL="http://127.0.0.1:$bad_port/manifest" \
    SNOWFAST_RELEASE_BASE="http://127.0.0.1:$bad_port/releases/download" \
    SNOWFAST_RAW_BASE="http://127.0.0.1:$bad_port/raw" \
    SNOWFAST_VERSION=0.3.1 \
    bash "$INSTALLER" >"$SANDBOX/badsum/out.log" 2>&1
  local rc=$?
  kill "$bad_pid" 2>/dev/null
  [ "$rc" -ne 0 ] || { fail "installer must fail on checksum mismatch"; return; }
  grep -Fq "checksum mismatch for SnowFastULP-0.3.1-linux-amd64" "$SANDBOX/badsum/out.log" \
    || { fail "checksum-mismatch message missing"; sed -n '1,30p' "$SANDBOX/badsum/out.log"; return; }
  # W9 companion: no temp droppings left in the install dir.
  local strays
  strays="$(find "$HOME/.local/bin" -name '*.tmp.*' | wc -l)"
  [ "$strays" -eq 0 ] || { fail "temp droppings left in install dir"; return; }
  pass "corrupted asset fails checksum"
}

case_path_untouched_when_already_on_path() {
  say ""
  say "==> case: PATH already configured -> profile untouched"
  new_sandbox alreadypath
  mkdir -p "$HOME/.local/bin"
  fresh_fake_profile
  # install.sh checks its own PATH for the install dir; provide it.
  TEST_ENV_PATH="/usr/bin:/bin:$HOME/.local/bin"
  run_installer \
    SNOWFAST_UPDATE_URL="http://127.0.0.1:$PORT/manifest" \
    SNOWFAST_RELEASE_BASE="http://127.0.0.1:$PORT/releases/download" \
    SNOWFAST_RAW_BASE="http://127.0.0.1:$PORT/raw" \
    bash "$INSTALLER" >"$SANDBOX/alreadypath/out.log" 2>&1
  local rc=$?
  [ "$rc" -eq 0 ] || { fail "installer exit code $rc"; return; }
  grep -Fq "is already on PATH" "$SANDBOX/alreadypath/out.log" || { fail "expected already-on-PATH path"; return; }
  [ ! -s "$HOME/.bashrc" ] || { fail "profile must stay untouched"; return; }
  pass "PATH already configured"
}

# H-06 (1): an existing destination DIRECTORY must abort the install. The
# pre-fix installer `mv`s the payload *inside* the directory and reports
# success.
case_dir_destination_rejected() {
  say ""
  say "==> case: directory destination rejected (H-06)"
  new_sandbox dirdest
  mkdir -p "$HOME/.local/bin"
  fresh_fake_profile
  printf '#!/bin/sh\necho old-sfu\n' >"$HOME/.local/bin/sfu"
  chmod 0755 "$HOME/.local/bin/sfu"
  mkdir "$HOME/.local/bin/sfs"
  local sfu_before sfu_after strays
  sfu_before="$(sha256sum "$HOME/.local/bin/sfu" | awk '{print $1}')"
  run_installer \
    SNOWFAST_UPDATE_URL="http://127.0.0.1:$PORT/manifest" \
    SNOWFAST_RELEASE_BASE="http://127.0.0.1:$PORT/releases/download" \
    SNOWFAST_RAW_BASE="http://127.0.0.1:$PORT/raw" \
    bash "$INSTALLER" >"$SANDBOX/dirdest/out.log" 2>&1
  local rc=$?
  [ "$rc" -ne 0 ] || { fail "installer must fail on directory destination"; sed -n '1,30p' "$SANDBOX/dirdest/out.log"; return; }
  grep -Fq "refusing to install" "$SANDBOX/dirdest/out.log" || { fail "error does not explain the refusal"; return; }
  grep -Fq "$HOME/.local/bin/sfs" "$SANDBOX/dirdest/out.log" || { fail "error does not name the destination"; return; }
  [ -d "$HOME/.local/bin/sfs" ] || { fail "pre-existing destination directory was replaced"; return; }
  [ -z "$(ls -A "$HOME/.local/bin/sfs")" ] || { fail "payload was nested inside the destination directory"; return; }
  sfu_after="$(sha256sum "$HOME/.local/bin/sfu" 2>/dev/null | awk '{print $1}')"
  [ "$sfu_before" = "$sfu_after" ] || { fail "pre-existing sfu was modified before the failure"; return; }
  strays="$(find "$HOME/.local/bin" \( -name '*.tmp.*' -o -name '*.preinst.*' \) | wc -l)"
  [ "$strays" -eq 0 ] || { fail "installer left droppings in the install dir"; return; }
  pass "directory destination rejected"
}

# H-06 (2): a hard failure mid-loop must roll back to the exact pre-run
# set. The `mv` shim (earlier in PATH, real mv captured first) fails only
# for the sfs destination, so sfu is committed before the failure.
case_rollback_restores_prerun_set() {
  say ""
  say "==> case: failed install rolls back to the pre-run set (H-06)"
  new_sandbox rollback
  local shim="$SANDBOX/rollback/shim"
  mkdir -p "$shim" "$HOME/.local/bin"
  fresh_fake_profile
  local real_mv="$SANDBOX/rollback/real-mv"
  cp "$(command -v mv)" "$real_mv"
  printf '#!/usr/bin/env bash\nif [ "$2" = "%s/.local/bin/sfs" ]; then\n  exit 1\nfi\nexec "%s" "$@"\n' "$HOME" "$real_mv" >"$shim/mv"
  chmod 0755 "$shim/mv"
  printf 'old-sfu-marker\n' >"$HOME/.local/bin/sfu"
  chmod 0755 "$HOME/.local/bin/sfu"
  printf 'old-sfl-marker\n' >"$HOME/.local/bin/sfl"
  chmod 0755 "$HOME/.local/bin/sfl"
  local sfu_before sfl_before sfu_after sfl_after strays
  sfu_before="$(sha256sum "$HOME/.local/bin/sfu" | awk '{print $1}')"
  sfl_before="$(sha256sum "$HOME/.local/bin/sfl" | awk '{print $1}')"
  TEST_ENV_PATH="$shim:/usr/bin:/bin"
  run_installer \
    SNOWFAST_UPDATE_URL="http://127.0.0.1:$PORT/manifest" \
    SNOWFAST_RELEASE_BASE="http://127.0.0.1:$PORT/releases/download" \
    SNOWFAST_RAW_BASE="http://127.0.0.1:$PORT/raw" \
    bash "$INSTALLER" >"$SANDBOX/rollback/out.log" 2>&1
  local rc=$?
  TEST_ENV_PATH="/usr/bin:/bin"
  [ "$rc" -ne 0 ] || { fail "installer must fail when a replacement fails"; sed -n '1,30p' "$SANDBOX/rollback/out.log"; return; }
  sfu_after="$(sha256sum "$HOME/.local/bin/sfu" 2>/dev/null | awk '{print $1}')"
  [ "$sfu_before" = "$sfu_after" ] || { fail "old sfu not restored byte-identical"; return; }
  sfl_after="$(sha256sum "$HOME/.local/bin/sfl" 2>/dev/null | awk '{print $1}')"
  [ "$sfl_before" = "$sfl_after" ] || { fail "old sfl not restored byte-identical"; return; }
  [ ! -e "$HOME/.local/bin/sfs" ] || { fail "failed command must not stay installed"; return; }
  strays="$(find "$HOME/.local/bin" \( -name '*.tmp.*' -o -name '*.preinst.*' \) | wc -l)"
  [ "$strays" -eq 0 ] || { fail "rollback left backup/temp droppings: $(find "$HOME/.local/bin" \( -name '*.tmp.*' -o -name '*.preinst.*' \))"; return; }
  pass "failed install rolls back to the pre-run set"
}

# H-06 (3): a raw-host outage must not abort the install after the
# binaries are already committed. Config creation is soft-fail.
case_config_outage_soft_fail() {
  say ""
  say "==> case: config outage no longer aborts install (H-06)"
  new_sandbox configoutage
  mkdir -p "$HOME/.local/bin"
  fresh_fake_profile
  : >"$FIXTURES/FAIL-RAW"
  run_installer \
    SNOWFAST_UPDATE_URL="http://127.0.0.1:$PORT/manifest" \
    SNOWFAST_RELEASE_BASE="http://127.0.0.1:$PORT/releases/download" \
    SNOWFAST_RAW_BASE="http://127.0.0.1:$PORT/raw" \
    bash "$INSTALLER" >"$SANDBOX/configoutage/out.log" 2>&1
  local rc=$?
  rm -f "$FIXTURES/FAIL-RAW"
  [ "$rc" -eq 0 ] || { fail "raw outage must not abort the install (rc=$rc)"; sed -n '1,30p' "$SANDBOX/configoutage/out.log"; return; }
  assert_file_executable "$HOME/.local/bin/sfu" || { fail "sfu not installed"; return; }
  assert_file_executable "$HOME/.local/bin/sfs" || { fail "sfs not installed"; return; }
  assert_file_executable "$HOME/.local/bin/sfl" || { fail "sfl not installed"; return; }
  [ ! -f "$XDG_CONFIG_HOME/snowfast/config.toml" ] || { fail "config must not be created when the example is unavailable"; return; }
  grep -Fq "could not download config example" "$SANDBOX/configoutage/out.log" || { fail "no warning about the skipped config"; return; }
  pass "config outage no longer aborts install"
}

main() {
  [ -f "$INSTALLER" ] || { say "installer not found at $INSTALLER"; exit 1; }
  start_server
  say "test server: http://127.0.0.1:$PORT (fixtures: $FIXTURES)"

  case_latest
  case_pinned
  case_minified
  case_dry_run
  case_dry_run_minified
  case_ref_default_is_tag
  case_ref_override_respected
  case_missing_sums_fails_clearly
  case_checksum_mismatch_fails
  case_path_untouched_when_already_on_path
  case_dir_destination_rejected
  case_rollback_restores_prerun_set
  case_config_outage_soft_fail

  say ""
  say "passed: $PASS  failed: $FAIL"
  [ "$FAIL" -eq 0 ] || exit 1
}

main "$@"
