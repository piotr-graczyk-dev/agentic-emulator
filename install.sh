#!/bin/sh
# Installs agemu from the latest GitHub release, on an Apple Silicon Mac or on Linux (x86-64 or
# arm64; Android only). On Windows use install.ps1.
#
#   curl -fsSL https://raw.githubusercontent.com/piotr-graczyk-dev/agentic-emulator/main/install.sh | sh
#
# AGEMU_VERSION=v0.1.0 picks a release, AGEMU_PREFIX=/usr/local another place (default ~/.local,
# binaries in $AGEMU_PREFIX/bin). It checks the download against the release's SHA256SUMS,
# restarts a running agemu daemon on the new build (devices keep running) and runs `agemu doctor`.
set -eu

repo="piotr-graczyk-dev/agentic-emulator"
prefix="${AGEMU_PREFIX:-$HOME/.local}"
version="${AGEMU_VERSION:-latest}"

die() { echo "install.sh: $*" >&2; exit 1; }

case "$(uname -s)" in
Darwin)
	[ "$(sysctl -n hw.optional.arm64 2>/dev/null || echo 0)" = 1 ] || die "agemu needs an Apple Silicon Mac (or Linux or Windows)"
	platform=darwin-arm64
	bins="agemu agemud agemu-ios-helper"
	;;
Linux)
	case "$(uname -m)" in
	x86_64 | amd64) platform=linux-amd64 ;;
	aarch64 | arm64) platform=linux-arm64 ;;
	*) die "agemu runs on x86-64 and arm64 Linux, not $(uname -m)" ;;
	esac
	bins="agemu agemud"
	;;
*) die "agemu runs on macOS and Linux with this script; on Windows use install.ps1" ;;
esac
sha256() { if command -v sha256sum >/dev/null 2>&1; then sha256sum "$@"; else shasum -a 256 "$@"; fi; }

if [ "$version" = latest ]; then
	# The latest release's page redirects to .../releases/tag/<version>.
	url="$(curl -fsLI -o /dev/null -w '%{url_effective}' "https://github.com/$repo/releases/latest")" ||
		die "no release found at https://github.com/$repo/releases (or GitHub is unreachable)"
	version="${url##*/}"
	case "$version" in v*) ;; *) die "no release found at https://github.com/$repo/releases" ;; esac
fi

name="agemu-$version-$platform"
base="${AGEMU_DOWNLOAD_BASE:-https://github.com/$repo/releases/download/$version}" # override: tests
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

echo "Downloading agemu $version"
curl -fsSL -o "$tmp/$name.tar.gz" "$base/$name.tar.gz" || die "no $name.tar.gz in release $version"
curl -fsSL -o "$tmp/SHA256SUMS" "$base/SHA256SUMS" || die "no SHA256SUMS in release $version"
(cd "$tmp" && grep " $name.tar.gz\$" SHA256SUMS | sha256 -c - >/dev/null) || die "checksum mismatch"
tar -xzf "$tmp/$name.tar.gz" -C "$tmp"

mkdir -p "$prefix/bin"
# Copy all of them next to their targets first, then move them into place: a failed copy leaves
# the installed binaries untouched, and a move replaces a file instead of rewriting it (macOS kills
# a signed binary rewritten in place).
staged=""
for b in $bins; do
	cp "$tmp/$name/$b" "$prefix/bin/.$b.new" || { rm -f $staged "$prefix/bin/.$b.new"; die "could not write to $prefix/bin"; }
	staged="$staged $prefix/bin/.$b.new"
done
for b in $bins; do
	mv -f "$prefix/bin/.$b.new" "$prefix/bin/$b" || die "could not replace $prefix/bin/$b"
done
echo "Installed $bins $version to $prefix/bin"
"$prefix/bin/agemu" daemon restart --if-running

case ":$PATH:" in
*":$prefix/bin:"*) ;;
*)
	echo
	echo "$prefix/bin is not on PATH. Add it:"
	rc="$HOME/.zshrc" shell=zsh
	[ "$platform" = darwin-arm64 ] || { rc="$HOME/.bashrc" shell=bash; }
	echo "  echo 'export PATH=\"$prefix/bin:\$PATH\"' >> $rc && exec $shell"
	;;
esac
echo
"$prefix/bin/agemu" doctor || true
