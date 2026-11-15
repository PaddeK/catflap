#!/bin/sh
# catflap installer for Linux and macOS.
#
#   curl -fsSL https://github.com/PaddeK/catflap/releases/latest/download/install.sh | sh
#
# Downloads the release binary for this machine, checks it against the release's
# checksums.txt and installs it to ~/.local/bin (as root: /usr/local/bin).
# Running it again upgrades catflap in place.
#
# Options (environment variables):
#   CATFLAP_VERSION      release to install, e.g. v1.0.0 (default: the latest release)
#   CATFLAP_INSTALL_DIR  where to put catflap (default: see above)
#   CATFLAP_REPO         GitHub repository to download from (default: PaddeK/catflap)
#   CATFLAP_BASE_URL     download from this URL instead of GitHub (mirrors, tests)
#
# Everything is inside main(), so a download cut short runs nothing.
set -eu

die() {
	echo "catflap install: $*" >&2
	exit 1
}

fetch() { # URL FILE
	if command -v curl >/dev/null 2>&1; then
		if [ -n "${CATFLAP_BASE_URL:-}" ]; then
			curl -fsSL --retry 3 -o "$2" "$1"
		else
			curl -fsSL --retry 3 --proto '=https' --tlsv1.2 -o "$2" "$1"
		fi
	elif command -v wget >/dev/null 2>&1; then
		wget -q -O "$2" "$1"
	else
		die "needs curl or wget"
	fi || die "download failed: $1"
}

sha256() { # FILE
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -d' ' -f1
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | cut -d' ' -f1
	else
		die "needs sha256sum or shasum to verify the download"
	fi
}

main() {
	repo=${CATFLAP_REPO:-PaddeK/catflap}
	version=${CATFLAP_VERSION:-latest}

	os=$(uname -s)
	arch=$(uname -m)
	case $os in
	Linux) os=linux ;;
	Darwin) os=darwin ;;
	*) die "no build for $os (catflap runs on Linux, macOS and Windows; on Windows use install.ps1)" ;;
	esac
	case $arch in
	x86_64 | amd64) arch=amd64 ;;
	aarch64 | arm64) arch=arm64 ;;
	# 32-bit ARM (e.g. Raspberry Pi 1/Zero: armv6l, Pi 2: armv7l). The build needs ARMv6+.
	armv6* | armv7* | armv8l) arch=arm ;;
	*) die "no build for $arch (catflap has amd64, arm64 and 32-bit ARM (ARMv6+) builds)" ;;
	esac
	if [ "$os" = darwin ] && [ "$arch" = amd64 ]; then
		# A shell under Rosetta reports x86_64 on Apple silicon: take the native build.
		if [ "$(sysctl -n sysctl.proc_translated 2>/dev/null || true)" = 1 ]; then
			arch=arm64
		fi
	fi
	asset="catflap_${os}_${arch}"

	if [ -n "${CATFLAP_BASE_URL:-}" ]; then
		base=${CATFLAP_BASE_URL%/}
	else
		if [ "$version" = latest ]; then
			base="https://github.com/$repo/releases/latest/download"
		else
			base="https://github.com/$repo/releases/download/$version"
		fi
	fi

	if [ -n "${CATFLAP_INSTALL_DIR:-}" ]; then
		dir=$CATFLAP_INSTALL_DIR
	elif [ "$(id -u)" = 0 ]; then
		dir=/usr/local/bin
	else
		[ -n "${HOME:-}" ] || die "HOME is not set; set CATFLAP_INSTALL_DIR"
		dir=$HOME/.local/bin
	fi

	tmp=$(mktemp -d 2>/dev/null || mktemp -d -t catflap)
	trap 'rm -rf "$tmp"' EXIT
	trap 'exit 1' HUP INT TERM

	echo "downloading $asset ($version)"
	fetch "$base/checksums.txt" "$tmp/checksums.txt"
	fetch "$base/$asset" "$tmp/$asset"

	want=$(awk -v f="$asset" '$2 == f || $2 == "*" f { print $1; exit }' "$tmp/checksums.txt")
	[ -n "$want" ] || die "checksums.txt has no entry for $asset"
	got=$(sha256 "$tmp/$asset")
	[ "$got" = "$want" ] || die "checksum mismatch for $asset (expected $want, got $got); nothing was installed"
	echo "checksum ok"

	chmod 755 "$tmp/$asset"
	new=$("$tmp/$asset" version 2>/dev/null | head -n 1 || true)
	[ -n "$new" ] || die "the downloaded binary does not run on this machine"

	mkdir -p "$dir" || die "cannot create $dir"
	# Copy next to the target, then rename: a running catflap keeps its old file.
	cp "$tmp/$asset" "$dir/.catflap.new.$$" || die "cannot write to $dir"
	mv -f "$dir/.catflap.new.$$" "$dir/catflap" || {
		rm -f "$dir/.catflap.new.$$"
		die "cannot replace $dir/catflap"
	}
	echo "installed $new to $dir/catflap"

	case ":$PATH:" in
	*":$dir:"*) ;;
	*) echo "note: $dir is not on your PATH; add it, or run $dir/catflap" ;;
	esac

	# Upgrades: running services keep the old binary until they restart.
	if command -v systemctl >/dev/null 2>&1; then
		if [ "$(id -u)" = 0 ] && systemctl is-active --quiet catflap-relay 2>/dev/null; then
			echo "the relay still runs the old version; restart it with: catflap restart"
		fi
		if [ "$(id -u)" != 0 ] && systemctl --user is-active --quiet catflap 2>/dev/null; then
			echo "the homelab still runs the old version; restart it with: catflap restart"
			echo "(this cuts open sessions through catflap, including the one you may be using now)"
		fi
	fi
}

main "$@"
