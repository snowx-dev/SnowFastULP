#!/usr/bin/env bash
set -euo pipefail

REPO_OWNER="${SNOWFAST_REPO_OWNER:-snowx-dev}"
REPO_NAME="${SNOWFAST_REPO_NAME:-SnowFastULP}"
DOCS_URL="${SNOWFAST_DOCS_URL:-https://snowfast.snowx.dev/docs}"
RAW_REF="${SNOWFAST_REF:-}"
RAW_BASE="${SNOWFAST_RAW_BASE:-}"
UPDATE_URL="${SNOWFAST_UPDATE_URL:-https://sfu-update.snowx.dev/}"
RELEASE_BASE="${SNOWFAST_RELEASE_BASE:-}"
DRY_RUN=false
# Temp file currently being installed (see install_binary); the EXIT trap
# removes it so a failed install never leaves droppings in the bin dir.
install_tmp=""
# H-06 transactional install state. Before the first replacement every
# existing destination is renamed to a same-directory .<cmd>.preinst.*
# backup (parallel arrays — no associative arrays, macOS ships bash 3.2);
# installed_cmds tracks which commands were actually committed. cleanup()
# rolls the exact pre-run set back unless install_complete=true, in which
# case the backups are removed instead.
installed_cmds=()
backup_cmds=()
backup_paths=()
snapshot_taken=false
install_complete=false

usage() {
  cat <<'EOF'
SnowFastULP installer

Usage:
  install.sh [--dry-run]

Environment:
  SNOWFAST_VERSION       Install a specific version, e.g. 0.2 or v0.2
  SNOWFAST_INSTALL_DIR   Install into this directory instead of auto-detecting
  SNOWFAST_UPDATE_URL    Update manifest URL (default: https://sfu-update.snowx.dev/)
  SNOWFAST_RELEASE_BASE  Releases base URL override (test hooks; default derives from REPO/owner + tag)
  SNOWFAST_REF           Raw GitHub ref for config.toml.example
                         (default: the v<version> tag being installed)
  SNOWFAST_RAW_BASE      Full raw base URL; overrides SNOWFAST_REF (test hooks)

Supported shell profiles:
  bash, zsh, fish, or ~/.profile fallback
EOF
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --dry-run)
      DRY_RUN=true
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "unknown argument: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
  shift
done

if [ -t 1 ] && [ -z "${NO_COLOR:-}" ] && [ "${TERM:-}" != "dumb" ]; then
  C_RESET="$(printf '\033[0m')"
  C_BLUE="$(printf '\033[34m')"
  C_GREEN="$(printf '\033[32m')"
  C_YELLOW="$(printf '\033[33m')"
  C_BOLD="$(printf '\033[1m')"
else
  C_RESET=""
  C_BLUE=""
  C_GREEN=""
  C_YELLOW=""
  C_BOLD=""
fi

say() {
  printf '%s\n' "$*"
}

section() {
  say ""
  say "${C_BOLD}${C_BLUE}==>${C_RESET} ${C_BOLD}$*${C_RESET}"
}

ok() {
  say "${C_GREEN}[ok]${C_RESET} $*"
}

skip() {
  say "${C_YELLOW}[skip]${C_RESET} $*"
}

warn() {
  say "${C_YELLOW}[warn]${C_RESET} $*"
}

fail() {
  say "[error] $*" >&2
  exit 1
}

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || fail "missing required command: $1"
}

need_cmd curl
need_cmd mktemp
need_cmd chmod

if command -v sha256sum >/dev/null 2>&1; then
  sha256_file() { sha256sum "$1" | awk '{print $1}'; }
elif command -v shasum >/dev/null 2>&1; then
  sha256_file() { shasum -a 256 "$1" | awk '{print $1}'; }
else
  fail "missing checksum command: sha256sum or shasum"
fi

detect_platform() {
  local os arch
  os="$(uname -s)"
  arch="$(uname -m)"

  case "$os" in
    Linux) os="linux" ;;
    Darwin) os="macos" ;;
    *) fail "unsupported OS: $os. Download release assets manually from GitHub." ;;
  esac

  case "$arch" in
    x86_64|amd64) arch="amd64" ;;
    arm64|aarch64) arch="arm64" ;;
    *) fail "unsupported architecture: $arch" ;;
  esac

  case "$os-$arch" in
    linux-amd64|macos-arm64) printf '%s-%s\n' "$os" "$arch" ;;
    linux-arm64) printf 'android-arm64\n' ;;
    *) fail "no published binary for $os-$arch yet" ;;
  esac
}

path_has_dir() {
  case ":${PATH:-}:" in
    *":$1:"*) return 0 ;;
    *) return 1 ;;
  esac
}

is_under_home() {
  case "$1" in
    "$HOME"/*) return 0 ;;
    *) return 1 ;;
  esac
}

choose_install_dir() {
  if [ -n "${SNOWFAST_INSTALL_DIR:-}" ]; then
    printf '%s\n' "$SNOWFAST_INSTALL_DIR"
    return
  fi

  local candidate path_dir
  for candidate in "$HOME/.local/bin" "$HOME/bin"; do
    if [ -d "$candidate" ] && [ -w "$candidate" ]; then
      printf '%s\n' "$candidate"
      return
    fi
  done

  IFS=':' read -r -a path_parts <<< "${PATH:-}"
  for path_dir in "${path_parts[@]}"; do
    [ -n "$path_dir" ] || continue
    if is_under_home "$path_dir" && [ -d "$path_dir" ] && [ -w "$path_dir" ]; then
      printf '%s\n' "$path_dir"
      return
    fi
  done

  printf '%s\n' "$HOME/.local/bin"
}

default_config_path() {
  local base
  if [ -n "${XDG_CONFIG_HOME:-}" ]; then
    base="$XDG_CONFIG_HOME"
  else
    base="$HOME/.config"
  fi
  printf '%s\n' "$base/snowfast/config.toml"
}

shell_profile() {
  local shell_name
  shell_name="$(basename "${SHELL:-}")"
  case "$shell_name" in
    zsh)
      printf '%s\n' "$HOME/.zshrc"
      ;;
    bash)
      if [ -f "$HOME/.bashrc" ] || [ "$(uname -s)" = "Linux" ]; then
        printf '%s\n' "$HOME/.bashrc"
      else
        printf '%s\n' "$HOME/.bash_profile"
      fi
      ;;
    fish)
      printf '%s\n' "$HOME/.config/fish/config.fish"
      ;;
    *)
      printf '%s\n' "$HOME/.profile"
      ;;
  esac
}

path_expr_for_profile() {
  local dir="$1"
  case "$dir" in
    "$HOME"/*)
      # shellcheck disable=SC2016  # literal $HOME is wanted in the profile snippet
      printf '$HOME/%s\n' "${dir#"$HOME"/}"
      ;;
    *)
      printf '%s\n' "$dir"
      ;;
  esac
}

append_path_block() {
  local dir="$1" profile="$2" expr shell_name
  [ -n "$profile" ] || return 1
  expr="$(path_expr_for_profile "$dir")"
  if [ -f "$profile" ] && grep -F "SnowFastULP installer" "$profile" >/dev/null 2>&1; then
    return 0
  fi
  shell_name="$(basename "${SHELL:-}")"
  if [ "$shell_name" = "fish" ]; then
    cat >> "$profile" <<EOF

# SnowFastULP installer
if not contains -- "${expr}" \$PATH
    fish_add_path "${expr}"
end
EOF
    return 0
  fi
  cat >> "$profile" <<EOF

# SnowFastULP installer
case ":\$PATH:" in
  *":${expr}:"*) ;;
  *) export PATH="${expr}:\$PATH" ;;
esac
EOF
}

download_file() {
  local url="$1" out="$2"
  curl -fsSL "$url" -o "$out"
}

normalize_version() {
  local v="$1"
  v="${v#v}"
  printf '%s\n' "$v"
}

# W3: the manifest only needs the "version" field now (checksums come from
# SHA256SUMS), but it must survive minified one-line JSON. Normalize the
# body and extract with a POSIX grep — no awk line-splitting. The grep
# groups swallow "no match" (empty result) so `set -o pipefail` never
# aborts the caller on absence.
manifest_version() {
  tr -d '\n\r\t' < "$1" \
    | sed 's/[[:space:]]\{1,\}/ /g' \
    | { grep -o '"version"[ ]\{0,\}:[ ]\{0,\}"[^"]*"' || true; } \
    | head -n 1 \
    | sed 's/.*:[ ]\{0,\}"//; s/"$//'
}

# checksum_for <sums-file> <asset-name>: extract an asset's SHA256 from a
# SHA256SUMS file (`<64-hex>  <name>` per line — hash first, name at line
# end). Match the trailing name and take field one, tolerant of any
# whitespace/separators. Missing entry yields "" (caller reports it).
checksum_for() {
  sed 's/[[:space:]]\{1,\}/ /g; s/ *$//' "$2" \
    | { grep " $1\$" || true; } \
    | sed 's/ .*//' \
    | head -n 1
}

# manifest_asset_url <asset-name> <manifest>: optional per-asset "url"
# (mirror support). The assets map is advisory now, so a missing entry or
# url simply yields "" and the caller falls back to releases/download.
manifest_asset_url() {
  local asset="$1" manifest="$2" blob
  blob="$(tr -d '\n\r\t' < "$manifest" | sed 's/[[:space:]]\{1,\}/ /g')"
  printf '%s' "$blob" \
    | { grep -o "\"$asset\"[ ]\{0,\}:[ ]\{0,\}{[^}]*}" || true; } \
    | { grep -o '"url"[ ]\{0,\}:[ ]\{0,\}"[^"]*"' || true; } \
    | head -n 1 \
    | sed 's/.*:[ ]\{0,\}"//; s/"$//'
}

verify_asset() {
  local expected="$1" path="$2" asset="$3" actual
  [ -n "$expected" ] || fail "SHA256SUMS has no checksum entry for $asset"
  [ "${#expected}" -eq 64 ] || fail "SHA256SUMS checksum for $asset is not a SHA256 hex digest"
  actual="$(sha256_file "$path")"
  [ "$expected" = "$actual" ] || fail "checksum mismatch for $asset"
}

created_dirs=()
mkdir_track_created() {
  local path="$1" current parent
  local -a missing=()
  if [ ! -d "$path" ]; then
    current="$path"
    while [ ! -d "$current" ]; do
      missing+=("$current")
      parent="$(dirname "$current")"
      [ "$parent" = "$current" ] && break
      current="$parent"
    done
    mkdir -p "$path"
    created_dirs+=("${missing[@]}")
  fi
}

install_binary() {
  local src="$1" dest="$2" tmp
  # H-06: `mv tmp dest` treats an existing destination DIRECTORY as a
  # container and moves the payload inside it, reporting success. Refuse
  # any non-regular destination loudly (-d also catches a symlink to a
  # directory); the pre-flight check below normally fires first, this is
  # the last line of defense against a destination changing mid-run.
  if [ -d "$dest" ]; then
    fail "destination is a directory, refusing to install over it: $dest"
  fi
  # W9: mktemp (O_EXCL) in the DESTINATION dir, not "${dest}.tmp.$$" — a
  # predictable name is a symlink-plant target in shared dirs. Atomic mv
  # onto dest; install_tmp keeps the trap from leaving droppings on failure.
  tmp="$(mktemp "${dest}.tmp.XXXXXX")" || fail "cannot create temp file next to $dest"
  install_tmp="$tmp"
  cp "$src" "$tmp"
  chmod 0755 "$tmp"
  mv "$tmp" "$dest"
  install_tmp=""
}

section "SnowFastULP installer"

platform="$(detect_platform)"
install_dir="$(choose_install_dir)"
config_path="$(default_config_path)"
tmp_dir="$(mktemp -d)"
cleanup() {
  if [ -n "$install_tmp" ]; then
    rm -f "$install_tmp"
  fi
  # H-06: a hard failure after one or more replacements must leave the
  # exact pre-run set behind. Remove everything this run committed, then
  # move every snapshot back (covers destinations snapshotted but not yet
  # re-installed — they are sitting in their backups right now).
  if [ "$snapshot_taken" = true ] && [ "$install_complete" = false ]; then
    if [ "${#installed_cmds[@]}" -gt 0 ]; then
      local i
      for i in "${!installed_cmds[@]}"; do
        rm -f "$install_dir/${installed_cmds[$i]}"
      done
    fi
    if [ "${#backup_cmds[@]}" -gt 0 ]; then
      local j
      for j in "${!backup_cmds[@]}"; do
        mv "${backup_paths[$j]}" "$install_dir/${backup_cmds[$j]}"
      done
    fi
  fi
  # Full success: the snapshots are now obsolete.
  if [ "$install_complete" = true ] && [ "${#backup_cmds[@]}" -gt 0 ]; then
    local k
    for k in "${!backup_cmds[@]}"; do
      rm -f "${backup_paths[$k]}"
    done
  fi
  rm -rf "$tmp_dir"
}
trap cleanup EXIT
manifest_path="$tmp_dir/update-manifest.json"
download_file "$UPDATE_URL" "$manifest_path"

if [ -n "${SNOWFAST_VERSION:-}" ]; then
  version="$(normalize_version "$SNOWFAST_VERSION")"
else
  version="$(normalize_version "$(manifest_version "$manifest_path")")"
fi
[ -n "$version" ] || fail "update manifest has no version"

release_tag="v${version}"
if [ -n "$RELEASE_BASE" ]; then
  # Test hooks: override the releases base; the tag segment stays composed.
  release_base="${RELEASE_BASE%/}/${release_tag}"
else
  release_base="https://github.com/${REPO_OWNER}/${REPO_NAME}/releases/download/${release_tag}"
fi

# W11: config example must match the release being installed, not main.
# SNOWFAST_REF wins for users who explicitly want main or a branch;
# SNOWFAST_RAW_BASE overrides the raw base with the ref still composed
# (test hooks, mirrors SNOWFAST_RELEASE_BASE semantics).
raw_ref="$RAW_REF"
[ -n "$raw_ref" ] || raw_ref="$release_tag"
if [ -n "$RAW_BASE" ]; then
  raw_base="${RAW_BASE%/}/${raw_ref}"
else
  raw_base="https://raw.githubusercontent.com/${REPO_OWNER}/${REPO_NAME}/${raw_ref}"
fi

# W2: checksums come from the release's SHA256SUMS artifact, not the
# manifest — a pinned SNOWFAST_VERSION must never be validated against the
# LATEST manifest's asset map. Resolving per tag also makes pinning work
# structurally: each tag ships its own SHA256SUMS. Fetched after the
# dry-run block so --dry-run performs no extra downloads.
sums_path="$tmp_dir/SHA256SUMS"

say "Repository : ${REPO_OWNER}/${REPO_NAME}"
say "Version    : ${version}"
say "Platform   : ${platform}"
say "Install dir: ${install_dir}"
say "Config     : ${config_path}"
say "Manifest   : ${UPDATE_URL}"
say "Checksums  : ${release_base}/SHA256SUMS"

assets=(
  "SnowFastULP-${version}-${platform}:sfu"
  "SnowFastSearch-${version}-${platform}:sfs"
  "SnowFastLog-${version}-${platform}:sfl"
)

if [ "$DRY_RUN" = true ]; then
  section "Dry run"
  say "Checksums  : ${release_base}/SHA256SUMS"
  say "Would download:"
  for item in "${assets[@]}"; do
    asset="${item%%:*}"
    # url in the manifest assets map is optional (mirror support); the
    # releases/download URL is always valid as a fallback.
    url="$(manifest_asset_url "$asset" "$manifest_path")"
    [ -n "$url" ] || url="${release_base}/${asset}"
    say "  ${url}"
  done
  say "Would install:"
  say "  ${install_dir}/sfu"
  say "  ${install_dir}/sfs"
  say "  ${install_dir}/sfl"
  say "Would create config if missing:"
  say "  ${config_path}"
  if path_has_dir "$install_dir"; then
    say "PATH already contains install dir."
  else
    profile="$(shell_profile)"
    if [ -n "$profile" ]; then
      say "Would append PATH setup to:"
      say "  ${profile}"
    else
      say "Would print manual PATH instructions for this shell."
    fi
  fi
  say ""
  ok "dry run complete"
  exit 0
fi
if ! download_file "${release_base}/SHA256SUMS" "$sums_path"; then
  fail "could not download SHA256SUMS for tag ${release_tag} (does release ${release_tag} exist and ship the SHA256SUMS artifact?)"
fi

section "Downloading release assets"

for item in "${assets[@]}"; do
  asset="${item%%:*}"
  cmd="${item##*:}"
  url="$(manifest_asset_url "$asset" "$manifest_path")"
  [ -n "$url" ] || url="${release_base}/${asset}"
  sha="$(checksum_for "$asset" "$sums_path")"
  download_file "$url" "$tmp_dir/$asset"
  verify_asset "$sha" "$tmp_dir/$asset" "$asset"
  ok "verified $asset"
done

# H-06: fetch config.toml.example BEFORE any binary is committed. The old
# order downloaded it after the installs, so a raw-host outage aborted
# with the binaries half-installed. Soft-fail: creation is skipped later
# with a warning; the install itself proceeds.
config_example_fetched=""
if [ ! -f "$config_path" ]; then
  if download_file "${raw_base}/config.toml.example" "$tmp_dir/config.toml.example"; then
    config_example_fetched=1
  else
    warn "could not download config example (skipping config creation)"
  fi
fi

section "Installing commands"

mkdir_track_created "$install_dir"
[ -w "$install_dir" ] || fail "install dir is not writable: $install_dir"

# H-06 (1/3): refuse non-regular destinations up front, before any
# destination is touched — `mv tmp dest` treats an existing directory as
# a container and moves the payload inside it.
for item in "${assets[@]}"; do
  cmd="${item##*:}"
  if [ -d "$install_dir/$cmd" ]; then
    fail "destination is a directory, refusing to install over it: $install_dir/$cmd"
  fi
done

# H-06 (2/3): snapshot every existing destination before the first
# replacement so a mid-loop hard failure can restore the exact pre-run
# set. Backups are same-directory with unique names (atomic rename);
# parallel arrays instead of associative arrays (macOS ships bash 3.2).
# NOTE: there is no crash journal in the shell installer — SIGKILL
# mid-loop can still leave mixed versions on disk. The Go updater
# (internal/selfupdate journal) is the crash-recovery path; this rollback
# only covers in-process hard failures.
snapshot_taken=true
for item in "${assets[@]}"; do
  cmd="${item##*:}"
  if [ -e "$install_dir/$cmd" ] || [ -L "$install_dir/$cmd" ]; then
    backup="$(mktemp "$install_dir/.${cmd}.preinst.XXXXXX")" \
      || fail "cannot create pre-install backup next to $install_dir/$cmd"
    mv "$install_dir/$cmd" "$backup"
    backup_cmds+=("$cmd")
    backup_paths+=("$backup")
  fi
done

give_to_invoking_user() {
  local path="$1"
  if [ -n "${SUDO_USER:-}" ] && [ "$(id -u)" -eq 0 ]; then
    chown "$SUDO_USER" "$path" 2>/dev/null || warn "could not chown $path to $SUDO_USER"
  fi
}

give_created_dirs() {
  local dir
  for dir in "${created_dirs[@]}"; do
    give_to_invoking_user "$dir"
  done
}


for item in "${assets[@]}"; do
  asset="${item%%:*}"
  cmd="${item##*:}"
  install_binary "$tmp_dir/$asset" "$install_dir/$cmd"
  installed_cmds+=("$cmd")
  give_to_invoking_user "$install_dir/$cmd"
  ok "installed $cmd -> $install_dir/$cmd"
done

section "Writing config"

config_status="preserved existing"
if [ -f "$config_path" ]; then
  skip "config already exists: $config_path"
elif [ -n "$config_example_fetched" ]; then
  mkdir_track_created "$(dirname "$config_path")"
  cp "$tmp_dir/config.toml.example" "$config_path"
  # W10: a config created under sudo must stay writable by the invoking
  # user (the Go side writes config keys back at runtime).
  give_to_invoking_user "$config_path"
  config_status="created"
  ok "created config: $config_path"
else
  # H-06: the example could not be fetched during prefetch (warning
  # already emitted there); never abort the install over it.
  config_status="skipped (example unavailable)"
  skip "config example unavailable, skipping config creation"
fi

section "Checking PATH"

path_status="already configured"
if path_has_dir "$install_dir"; then
  ok "$install_dir is already on PATH"
else
  profile="$(shell_profile)"
  if [ -n "$profile" ]; then
    mkdir_track_created "$(dirname "$profile")"
    touch "$profile"
    append_path_block "$install_dir" "$profile"
    path_status="updated $profile"
    ok "added $install_dir to PATH in $profile"
    warn "restart your shell or run: source \"$profile\""
  fi
fi

give_created_dirs

section "Installed"

say "Commands:"
say "  sfu  clean and deduplicate ULP/LPU text dumps"
say "  sfs  search plain .txt dumps or compressed .zst libraries"
say "  sfl  extract stealer logs into ULP lines or a library"
say "Docs:"
say "  $DOCS_URL"
say ""
say "Config:"
say "  $config_path ($config_status)"
say ""
say "Install dir:"
say "  $install_dir ($path_status)"
say ""
say "Try:"
say "  sfu --version"
say "  sfs --version"
say "  sfl --version"

# H-06: everything succeeded — the EXIT trap now deletes the pre-install
# backups instead of rolling back.
install_complete=true
