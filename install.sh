#!/bin/sh
# Installs agemu from the latest GitHub release:
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

[ "$(uname -s)" = Darwin ] || die "agemu runs on macOS only"
[ "$(sysctl -n hw.optional.arm64 2>/dev/null || echo 0)" = 1 ] || die "agemu needs an Apple Silicon Mac"

if [ "$version" = latest ]; then
	# The latest release's page redirects to .../releases/tag/<version>.
	url="$(curl -fsLI -o /dev/null -w '%{url_effective}' "https://github.com/$repo/releases/latest")" ||
		die "no release found at https://github.com/$repo/releases (or GitHub is unreachable)"
	version="${url##*/}"
	case "$version" in v*) ;; *) die "no release found at https://github.com/$repo/releases" ;; esac
fi

name="agemu-$version-darwin-arm64"
base="${AGEMU_DOWNLOAD_BASE:-https://github.com/$repo/releases/download/$version}" # override: tests
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

echo "Downloading agemu $version"
curl -fsSL -o "$tmp/$name.tar.gz" "$base/$name.tar.gz" || die "no $name.tar.gz in release $version"
curl -fsSL -o "$tmp/SHA256SUMS" "$base/SHA256SUMS" || die "no SHA256SUMS in release $version"
(cd "$tmp" && grep " $name.tar.gz\$" SHA256SUMS | shasum -a 256 -c - >/dev/null) || die "checksum mismatch"
tar -xzf "$tmp/$name.tar.gz" -C "$tmp"

mkdir -p "$prefix/bin"
for b in agemu agemud agemu-ios-helper; do
	# Replace, never overwrite in place: macOS kills a signed binary rewritten in place.
	rm -f "$prefix/bin/$b"
	cp "$tmp/$name/$b" "$prefix/bin/$b"
done
echo "Installed agemu, agemud and agemu-ios-helper $version to $prefix/bin"
"$prefix/bin/agemu" daemon restart --if-running

case ":$PATH:" in
*":$prefix/bin:"*) ;;
*)
	echo
	echo "$prefix/bin is not on PATH. Add it:"
	echo "  echo 'export PATH=\"$prefix/bin:\$PATH\"' >> ~/.zshrc && exec zsh"
	;;
esac
echo
"$prefix/bin/agemu" doctor || true
