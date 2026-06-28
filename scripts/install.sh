#!/usr/bin/env bash
set -euo pipefail

REPO_OWNER="${SNOWFAST_REPO_OWNER:-snowx-dev}"
REPO_NAME="${SNOWFAST_REPO_NAME:-SnowFastULP}"
DOCS_URL="${SNOWFAST_DOCS_URL:-https://snowfast.todo/docs}"
RAW_REF="${SNOWFAST_REF:-main}"
UPDATE_URL="${SNOWFAST_UPDATE_URL:-https://sfu-update.snowx.dev/}"
DRY_RUN=false

usage() {
  cat <<'EOF'
SnowFastULP installer

Usage:
  install.sh [--dry-run]

Environment:
  SNOWFAST_VERSION       Install a specific version, e.g. 0.1.1 or v0.1.1
  SNOWFAST_INSTALL_DIR   Install into this directory instead of auto-detecting
  SNOWFAST_UPDATE_URL    Update manifest URL (default: https://sfu-update.snowx.dev/)
  SNOWFAST_REF           Raw GitHub ref for config.toml.example (default: main)

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

manifest_version() {
  awk -F'"' '/"version"[[:space:]]*:/ {print $4; exit}' "$1"
}

manifest_asset_field() {
  local manifest="$1" asset="$2" field="$3"
  awk -v asset="$asset" -v field="$field" '
    index($0, "\"" asset "\"") { in_asset=1; next }
    in_asset && index($0, "\"" field "\"") {
      line=$0
      sub(/^[^:]*:[[:space:]]*"/, "", line)
      sub(/".*$/, "", line)
      print line
      exit
    }
    in_asset && $0 ~ /^[[:space:]]*}/ { in_asset=0 }
  ' "$manifest"
}

verify_asset() {
  local expected="$1" path="$2" asset="$3" actual
  [ -n "$expected" ] || fail "manifest missing checksum for $asset"
  [ "${#expected}" -eq 64 ] || fail "manifest checksum for $asset is not a SHA256 hex digest"
  actual="$(sha256_file "$path")"
  [ "$expected" = "$actual" ] || fail "checksum mismatch for $asset"
}

install_binary() {
  local src="$1" dest="$2" tmp
  tmp="${dest}.tmp.$$"
  cp "$src" "$tmp"
  chmod 0755 "$tmp"
  mv "$tmp" "$dest"
}

section "SnowFastULP installer"

platform="$(detect_platform)"
install_dir="$(choose_install_dir)"
config_path="$(default_config_path)"
raw_base="https://raw.githubusercontent.com/${REPO_OWNER}/${REPO_NAME}/${RAW_REF}"
tmp_dir="$(mktemp -d)"
cleanup() {
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

release_asset_version="$version"
release_tag="v${version}"
release_base="https://github.com/${REPO_OWNER}/${REPO_NAME}/releases/download/${release_tag}"

say "Repository : ${REPO_OWNER}/${REPO_NAME}"
say "Version    : ${version}"
say "Platform   : ${platform}"
say "Install dir: ${install_dir}"
say "Config     : ${config_path}"
say "Manifest   : ${UPDATE_URL}"

assets=(
  "SnowFastULP-${version}-${platform}:sfu"
  "SnowFastSearch-${version}-${platform}:sfs"
  "SnowFastLog-${version}-${platform}:sfl"
)

if [ "$DRY_RUN" = true ]; then
  section "Dry run"
  say "Would download:"
  for item in "${assets[@]}"; do
    asset="${item%%:*}"
    url="$(manifest_asset_field "$manifest_path" "$asset" "url")"
    [ -n "$url" ] || url="${release_base}/${asset}"
    sha="$(manifest_asset_field "$manifest_path" "$asset" "sha256")"
    [ -n "$sha" ] || fail "manifest missing checksum for $asset"
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

section "Downloading release assets"

for item in "${assets[@]}"; do
  asset="${item%%:*}"
  cmd="${item##*:}"
  url="$(manifest_asset_field "$manifest_path" "$asset" "url")"
  [ -n "$url" ] || url="${release_base}/${asset}"
  sha="$(manifest_asset_field "$manifest_path" "$asset" "sha256")"
  download_file "$url" "$tmp_dir/$asset"
  verify_asset "$sha" "$tmp_dir/$asset" "$asset"
  ok "verified $asset"
done

section "Installing commands"

mkdir -p "$install_dir"
[ -w "$install_dir" ] || fail "install dir is not writable: $install_dir"

for item in "${assets[@]}"; do
  asset="${item%%:*}"
  cmd="${item##*:}"
  install_binary "$tmp_dir/$asset" "$install_dir/$cmd"
  ok "installed $cmd -> $install_dir/$cmd"
done

section "Writing config"

config_status="preserved existing"
if [ -f "$config_path" ]; then
  skip "config already exists: $config_path"
else
  mkdir -p "$(dirname "$config_path")"
  download_file "${raw_base}/config.toml.example" "$tmp_dir/config.toml.example"
  cp "$tmp_dir/config.toml.example" "$config_path"
  config_status="created"
  ok "created config: $config_path"
fi

section "Checking PATH"

path_status="already configured"
if path_has_dir "$install_dir"; then
  ok "$install_dir is already on PATH"
else
  profile="$(shell_profile)"
  if [ -n "$profile" ]; then
    mkdir -p "$(dirname "$profile")"
    touch "$profile"
    append_path_block "$install_dir" "$profile"
    path_status="updated $profile"
    ok "added $install_dir to PATH in $profile"
    warn "restart your shell or run: source \"$profile\""
  fi
fi

section "Installed"

say "Commands:"
say "  sfu  clean and deduplicate ULP/LPU text dumps"
say "  sfs  search plain .txt dumps or compressed .zst libraries"
say "  sfl  extract stealer logs into ULP lines or a library"
say ""
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
