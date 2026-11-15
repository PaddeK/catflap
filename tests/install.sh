#!/usr/bin/env bash
# Test install.sh against a release built by goreleaser:
#   goreleaser release --snapshot --clean && tests/install.sh dist
#
# Serves the release files on 127.0.0.1 and checks that install.sh installs
# and upgrades catflap, and refuses tampered or unlisted downloads.
set -euo pipefail
cd "$(dirname "$0")/.."
DIST=${1:?usage: tests/install.sh DIST_DIR}
[ -f "$DIST/checksums.txt" ] || { echo "no checksums.txt in $DIST" >&2; exit 1; }

T=$(mktemp -d /tmp/cfinst.XXXX)
SRV=
cleanup() {
	[ -n "$SRV" ] && kill "$SRV" 2>/dev/null || true
	rm -rf "$T"
}
trap cleanup EXIT

pass=0 fail=0
ok()   { echo "  PASS  $*"; pass=$((pass+1)); }
bad()  { echo "  FAIL  $*"; fail=$((fail+1)); }
check(){ local d=$1; shift; if "$@"; then ok "$d"; else bad "$d"; fi; }

case $(uname -s)-$(uname -m) in
Linux-x86_64) ASSET=catflap_linux_amd64 ;;
Linux-aarch64) ASSET=catflap_linux_arm64 ;;
Linux-armv6*|Linux-armv7*|Linux-armv8l) ASSET=catflap_linux_arm ;;
Darwin-arm64) ASSET=catflap_darwin_arm64 ;;
*) echo "no release build for this machine" >&2; exit 1 ;;
esac

# goreleaser leaves plain binaries in their build dirs and renames them on upload;
# artifacts.json says which file goes up under which name.
BIN=$(python3 -I - "$DIST/artifacts.json" "$ASSET" <<'PY'
import json, sys
print(next(a["path"] for a in json.load(open(sys.argv[1])) if a["name"] == sys.argv[2]))
PY
)

# Three release copies: good, tampered binary, binary missing from checksums.txt.
mkdir -p "$T/www/good" "$T/www/tampered" "$T/www/unlisted"
for d in good tampered unlisted; do cp "$BIN" "$T/www/$d/$ASSET"; done
cp "$DIST/checksums.txt" "$T/www/good/"
cp "$DIST/checksums.txt" "$T/www/tampered/"
printf 'x' >> "$T/www/tampered/$ASSET"
grep -v " $ASSET\$" "$DIST/checksums.txt" > "$T/www/unlisted/checksums.txt" || true

python3 -I - "$T/www" "$T/port" <<'PY' &
import functools, http.server, sys
h = functools.partial(http.server.SimpleHTTPRequestHandler, directory=sys.argv[1])
h.func.log_message = lambda *a: None
s = http.server.ThreadingHTTPServer(("127.0.0.1", 0), h)
open(sys.argv[2], "w").write(str(s.server_address[1]))
s.serve_forever()
PY
SRV=$!
for _ in $(seq 50); do [ -s "$T/port" ] && break; sleep 0.1; done
URL="http://127.0.0.1:$(cat "$T/port")"

inst() { # NAME: run install.sh from release NAME into $T/bin; output in $T/out
	env -u CATFLAP_REPO -u CATFLAP_VERSION CATFLAP_INSTALL_DIR="$T/bin" CATFLAP_BASE_URL="$URL/$1" \
		sh ./install.sh > "$T/out" 2>&1
}
refused() { ! inst "$1"; }

echo "== install.sh"
check "installs the release binary" inst good
check "says the checksum is ok" grep -q "checksum ok" "$T/out"
runs() { "$T/bin/catflap" version > /dev/null; }
check "installed binary runs" runs
WANT=$(awk -v f="$ASSET" '$2 == f { print $1 }' "$DIST/checksums.txt")
if command -v sha256sum >/dev/null; then GOT=$(sha256sum "$T/bin/catflap"); else GOT=$(shasum -a 256 "$T/bin/catflap"); fi
check "installed file matches checksums.txt" test "${GOT%% *}" = "$WANT"
check "no temp files left in the install dir" test "$(ls -A "$T/bin")" = catflap

# Upgrade in place while the old binary is running.
CATFLAP_DIR="$T/cfg" "$T/bin/catflap" agent >/dev/null 2>&1 & RUNNING=$!
check "upgrades over a running catflap" inst good
kill "$RUNNING" 2>/dev/null || true; wait "$RUNNING" 2>/dev/null || true

echo marker > "$T/bin/catflap"
check "tampered binary is refused" refused tampered
check "reports the checksum mismatch" grep -q "checksum mismatch" "$T/out"
check "tampered: existing file untouched" grep -qx marker "$T/bin/catflap"

check "binary without checksum is refused" refused unlisted
check "unlisted: existing file untouched" grep -qx marker "$T/bin/catflap"

check "missing release fails" refused missing


echo
echo "$pass passed, $fail failed"
[ "$fail" = 0 ]
