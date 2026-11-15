#!/usr/bin/env bash
# Platform tests in Docker against a release built by goreleaser:
#   goreleaser release --snapshot --clean && tests/docker.sh dist
#
# - install.sh on several distros (busybox/wget, curl, none), amd64 and arm64,
#   as root and as a normal user; checks the right binary was picked.
# - the end-to-end test with the linux amd64, arm64 and 32-bit ARM release binaries.
#
# arm64 runs under emulation (Docker Desktop, or qemu-user-static + binfmt).
# Usage: tests/docker.sh DIST_DIR [install|e2e]   (default: both)
set -euo pipefail
cd "$(dirname "$0")/.."
DIST=${1:?usage: tests/docker.sh DIST_DIR [install|e2e]}
ONLY=${2:-}
[ -f "$DIST/artifacts.json" ] || { echo "no goreleaser output in $DIST" >&2; exit 1; }

T=$(mktemp -d /tmp/cfdock.XXXX)
ID=cfdock-$$
cleanup() {
	docker rm -f "$ID-www" >/dev/null 2>&1 || true
	docker network rm "$ID" >/dev/null 2>&1 || true
	rm -rf "$T"
}
trap cleanup EXIT

pass=0 fail=0
ok()  { echo "  PASS  $*"; pass=$((pass+1)); }
bad() { echo "  FAIL  $*"; fail=$((fail+1)); }
indent() { sed 's/^/        /'; }

# The release as it would be on GitHub: files under their upload names.
mkdir -p "$T/www"
python3 -I - "$DIST" "$T/www" <<'PY'
import json, os, shutil, sys
dist, www = sys.argv[1], sys.argv[2]
names = {l.split()[1].lstrip("*") for l in open(os.path.join(dist, "checksums.txt"))}
for a in json.load(open(os.path.join(dist, "artifacts.json"))):
    if a["name"] in names:
        shutil.copy(a["path"], os.path.join(www, a["name"]))
shutil.copy(os.path.join(dist, "checksums.txt"), www)
PY
for f in install.sh install.ps1; do [ -f "$T/www/$f" ] || cp "$f" "$T/www/"; done
sum() { awk -v f="$1" '$2 == f { print $1 }' "$T/www/checksums.txt"; }

if [ "$ONLY" != e2e ]; then
	echo "== install.sh in containers"
	docker network create "$ID" >/dev/null
	docker run -d --name "$ID-www" --network "$ID" -v "$T/www:/www:ro" \
		python:3.13-alpine python -m http.server -d /www 8000 >/dev/null

	# NAME PLATFORM IMAGE PREPARE USER EXPECT
	#   USER:   root, or "user" (a normal user is created)
	#   EXPECT: ok, or a message install.sh must fail with
	inst() {
		local name=$1 platform=$2 image=$3 prep=$4 user=$5 expect=$6
		local arch=${platform#linux/} dir=/usr/local/bin run='sh /install.sh'
		arch=${arch%%/*}   # linux/arm/v7 -> arm
		if [ "$user" = user ]; then
			dir=/home/u/.local/bin
			run="su u -c 'cd /home/u && HOME=/home/u sh /install.sh'"
			prep="$prep; (useradd -m u 2>/dev/null || adduser -D u)"
		fi
		local out rc=0
		out=$(docker run --rm --network "$ID" --platform "$platform" \
			-e CATFLAP_BASE_URL="http://$ID-www:8000" \
			-v "$PWD/install.sh:/install.sh:ro" "$image" sh -c "
				set -e
				{ $prep; } >/dev/null 2>&1
				$run
				echo \"SUM \$(sha256sum $dir/catflap | cut -d' ' -f1)\"
				echo \"VERSION \$($dir/catflap version | head -n 1)\"
			" 2>&1) || rc=$?
		if [ "$expect" != ok ]; then
			if [ $rc != 0 ] && grep -q "$expect" <<<"$out"; then ok "$name: fails with \"$expect\""
			else bad "$name: expected failure \"$expect\""; indent <<<"$out"; fi
			return
		fi
		if [ $rc = 0 ] && grep -q "checksum ok" <<<"$out" \
			&& grep -q "^SUM $(sum "catflap_linux_$arch")\$" <<<"$out" \
			&& grep -q "^VERSION catflap " <<<"$out"; then
			ok "$name: installed catflap_linux_$arch to $dir"
		else
			bad "$name"; indent <<<"$out"
		fi
	}

	inst "alpine amd64, busybox wget, root"   linux/amd64 alpine:3.22    ':'                                      root ok
	inst "alpine arm64, busybox wget, user"   linux/arm64 alpine:3.22    ':'                                      user ok
	inst "debian amd64, no curl or wget"      linux/amd64 debian:13-slim ':'                                      root "needs curl or wget"
	inst "debian arm64, wget, user"           linux/arm64 debian:13-slim 'apt-get update -qq && apt-get install -y -qq wget' user ok
	inst "ubuntu amd64, curl, user"           linux/amd64 ubuntu:24.04   'apt-get update -qq && apt-get install -y -qq curl ca-certificates' user ok
	inst "fedora amd64, curl, root"           linux/amd64 fedora:43      ':'                                      root ok
	inst "alpine arm/v6 (Pi Zero), root"     linux/arm/v6 alpine:3.22   ':' root ok
	inst "debian arm/v7 (Pi 2), wget, user"   linux/arm/v7 debian:13-slim 'apt-get update -qq && apt-get install -y -qq wget' user ok
fi

if [ "$ONLY" != install ]; then
	# PLATFORM IMAGE: the end-to-end test with that platform's release binary.
	# arm/v6 is the CPU of a Raspberry Pi 1/Zero; arm/v7 of a Pi 2 (and 32-bit OS on newer Pis).
	e2e() {
		local platform=$1 image=$2 arch prep
		arch=${platform#linux/}; arch=${arch%%/*}
		case $image in
		alpine*) prep='apk add -q bash coreutils procps openssl python3 openssh-keygen' ;;
		*) prep='apt-get update -qq && apt-get install -y -qq openssl python3 openssh-client procps' ;;
		esac
		local log="$T/e2e-${platform//\//-}.log"
		echo "== end-to-end test, release binary $platform ($image)"
		if docker run --rm --platform "$platform" \
			-v "$PWD/tests:/work/tests:ro" -v "$T/www/catflap_linux_$arch:/usr/local/bin/catflap:ro" \
			-e CATFLAP_BIN=/usr/local/bin/catflap "$image" sh -c "
				{ $prep; } >/dev/null 2>&1
				cd /work && tests/e2e.sh
			" > "$log" 2>&1; then
			ok "e2e $platform: $(grep -E '^passed' "$log")"
		else
			bad "e2e $platform"; tail -20 "$log" | sed 's/^/        /'
		fi
	}
	e2e linux/amd64 debian:13-slim
	e2e linux/arm64 debian:13-slim
	e2e linux/arm/v7 debian:13-slim
	e2e linux/arm/v6 alpine:3.22
fi

echo
echo "$pass passed, $fail failed"
[ "$fail" = 0 ]
