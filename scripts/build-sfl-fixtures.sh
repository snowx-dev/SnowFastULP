#!/usr/bin/env bash
#
# Build small SnowFastLog test fixtures by cherry-picking REAL loose logs from
# the analyst data tree, one archive per supported type (zip / 7z / rar).
#
# Output lands in  $SFL_REALDATA_DIR/fullz/sfl-test-fixtures  and is NEVER
# committed: it is derived from real leak data and lives outside the repo, so
# the gated real-data tests skip cleanly on machines without it.
#
# Usage:
#   SFL_REALDATA_DIR=/path/to/ulp scripts/build-sfl-fixtures.sh
#
# Optional:
#   SFL_FIXTURE_VICTIMS=3   how many victim log folders to cherry-pick (default 3)
#
# zip + 7z are always built. The rar fixture is built only if a `rar` packer is
# on PATH (RAR creation is proprietary); otherwise rar is covered by the heavy
# tier against the real archive. The good password is shared with the existing
# fixtures so the Go tests' goodPass constant stays valid.
set -euo pipefail

PASS="fullz-ice-2026"
N_VICTIMS="${SFL_FIXTURE_VICTIMS:-3}"

err()  { printf '\033[31m%s\033[0m\n' "$*" >&2; }
ok()   { printf '\033[32m%s\033[0m\n' "$*"; }
info() { printf '\033[36m%s\033[0m\n' "$*"; }

root="${SFL_REALDATA_DIR:-}"
[ -n "$root" ] || { err "set SFL_REALDATA_DIR to the ulp data dir (the one holding fullz/)"; exit 2; }
victims_parent="$root/fullz/1200_130526_extracted/1200_130526"
[ -d "$victims_parent" ] || { err "missing $victims_parent (real-data layout changed?)"; exit 2; }

fx="$root/fullz/sfl-test-fixtures"
mkdir -p "$fx"

# Stage only the password-like files (mirroring internal/sflog isPasswordFile)
# from N victim folders, preserving a per-victim subdir so same-named logs don't
# collide. This keeps fixtures tiny and the credential set deterministic.
stage="$(mktemp -d)"
trap 'rm -rf "$stage"' EXIT
picked=0
while IFS= read -r victim; do
	[ -d "$victim" ] || continue
	base="$(basename "$victim")"
	found=0
	while IFS= read -r -d '' f; do
		case "$(basename "$f" | tr '[:upper:]' '[:lower:]')" in
			*passwordcracker*) continue ;;
		esac
		rel="${f#"$victim"/}"
		mkdir -p "$stage/cherry/$base/$(dirname "$rel")"
		cp "$f" "$stage/cherry/$base/$rel"
		found=1
	done < <(find "$victim" -type f \( -iname '*password*.txt' -o -iname '*password*.log' \) -print0)
	[ "$found" -eq 1 ] && picked=$((picked + 1))
	[ "$picked" -ge "$N_VICTIMS" ] && break
done < <(find "$victims_parent" -mindepth 1 -maxdepth 1 -type d | sort)

[ "$picked" -ge 1 ] || { err "no victim folders with password files under $victims_parent"; exit 2; }
info "staged password logs from $picked victim folder(s)"

(
	cd "$stage/cherry"

	rm -f "$fx/cherry-plain.zip"
	zip -q -r -X "$fx/cherry-plain.zip" .
	ok "built cherry-plain.zip"

	# traditional ZipCrypto (-P); yeka/zip reads both ZipCrypto and WinZip AES
	rm -f "$fx/cherry-encrypted.zip"
	zip -q -r -X -P "$PASS" "$fx/cherry-encrypted.zip" .
	ok "built cherry-encrypted.zip"

	# header-encrypted 7z so even the listing needs the password
	rm -f "$fx/cherry-encrypted.7z"
	7z a -bso0 -bsp0 -p"$PASS" -mhe=on "$fx/cherry-encrypted.7z" . >/dev/null
	ok "built cherry-encrypted.7z"

	if command -v rar >/dev/null 2>&1; then
		rm -f "$fx/cherry-encrypted.rar"
		# -r recurse, -hp encrypt headers+data, -ep1 drop the leading ./; failure
		# is non-fatal so a flaky packer never blocks the zip/7z fixtures.
		if rar a -r -ep1 -idq -hp"$PASS" "$fx/cherry-encrypted.rar" . >/dev/null 2>&1; then
			ok "built cherry-encrypted.rar"
		else
			err "rar packing failed -> skipping cherry-encrypted.rar (heavy tier covers rar via the real archive)"
		fi
	else
		err "no 'rar' packer on PATH -> skipping cherry-encrypted.rar (heavy tier covers rar via the real archive)"
	fi
)

# password list: the good password buried among non-working candidates, to
# prove bad guesses are tried and skipped gracefully regardless of order.
printf 'nope-one\nnope-two\n%s\nnope-three\n' "$PASS" >"$fx/passwords-many.txt"
chmod 600 "$fx/passwords-many.txt"
ok "wrote passwords-many.txt (good password buried among bad)"

# expected unique-credential count: baseline from the plain fixture via sfl, so
# the per-type tests can assert all archive types agree with it.
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
out="$(mktemp -d)"
if (cd "$repo_root" && go run ./cmd/sfl "$fx/cherry-plain.zip" -o "$out" -no-tui -no-update-check >/dev/null 2>&1); then
	result="$(find "$out" -name 'sfl_*.txt' | head -1)"
	if [ -n "$result" ]; then
		uniq_count="$(sort -u "$result" | grep -c .)"
		printf '%s\n' "$uniq_count" >"$fx/cherry-expected.txt"
		ok "cherry-expected.txt = $uniq_count unique credentials"
	else
		err "sfl produced no output; cherry-expected.txt not written"
	fi
fi
rm -rf "$out"

ok "fixtures ready in $fx"
