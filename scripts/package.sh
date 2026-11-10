#!/usr/bin/env bash
# Packages a release. The macOS binaries come from bin/ (build them first with
# `make build VERSION=<version>` on an Apple Silicon Mac: the iOS helper needs Xcode's Swift);
# the Linux and Windows binaries are cross-compiled here (pure Go, Android only).
#
#   scripts/package.sh <version> [out-dir]     e.g. scripts/package.sh v0.1.0 dist
#
# writes to out-dir (default dist/):
#   agemu-<version>-darwin-arm64.tar.gz   agemu, agemud, agemu-ios-helper   (GitHub Releases, install.sh)
#   agemu-<version>-linux-amd64.tar.gz    agemu, agemud                     (install.sh)
#   agemu-<version>-linux-arm64.tar.gz    agemu, agemud                     (install.sh)
#   agemu-<version>-windows-amd64.zip     agemu.exe, agemud.exe             (install.ps1)
#   agentic-emulator-<semver>.tgz         the npm package with all of them (npm publish <file>)
#   SHA256SUMS
set -euo pipefail
cd "$(dirname "$0")/.."

version="${1:?usage: scripts/package.sh <version> [out-dir]}"
semver="${version#v}"
out="${2:-dist}"
mac_bins=(agemu agemud agemu-ios-helper)
# GOOS/GOARCH, and the platform key npm's launcher (npm/agemu.js) looks for.
cross=("linux amd64 linux-x64" "linux arm64 linux-arm64" "windows amd64 win32-x64")

[[ "$semver" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] || { echo "not a version: $version" >&2; exit 2; }
for b in "${mac_bins[@]}"; do
	[ -x "bin/$b" ] || { echo "bin/$b missing: make build VERSION=$version" >&2; exit 1; }
	file "bin/$b" | grep -q 'arm64' || { echo "bin/$b is not an arm64 binary" >&2; exit 1; }
done
built="$(AGEMU_HOME="$(mktemp -d)" bin/agemu version | sed -n 's/^agemu  *//p')"
[ "$built" = "$version" ] || { echo "bin/agemu is version $built, not $version: make build VERSION=$version" >&2; exit 1; }

rm -rf "$out" && mkdir -p "$out" && out="$(cd "$out" && pwd)"
stage="$(mktemp -d)"
trap 'rm -rf "$stage"' EXIT

# Linux and Windows: the same sources, without the iOS backend (it builds only for darwin).
ldflags="-s -w -X github.com/piotr-graczyk-dev/agentic-emulator/internal/daemon.Version=$version"
for c in "${cross[@]}"; do
	read -r goos goarch _ <<<"$c"
	ext=""; [ "$goos" = windows ] && ext=".exe"
	for cmd in agemu agemud; do
		CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build -trimpath -ldflags "$ldflags" \
			-o "$stage/bin/$goos-$goarch/$cmd$ext" "./cmd/$cmd"
	done
done

# Archives: one directory with the binaries and the licenses.
archive() { # name, then the binaries
	local name="$1"; shift
	mkdir -p "$stage/$name"
	cp "$@" LICENSE THIRD_PARTY_NOTICES.md "$stage/$name/"
	case "$name" in
	*windows*) (cd "$stage" && zip -qr "$out/$name.zip" "$name") ;;
	*) tar -C "$stage" -czf "$out/$name.tar.gz" "$name" ;;
	esac
}
archive "agemu-$version-darwin-arm64" "${mac_bins[@]/#/bin/}"
archive "agemu-$version-linux-amd64" "$stage/bin/linux-amd64/agemu" "$stage/bin/linux-amd64/agemud"
archive "agemu-$version-linux-arm64" "$stage/bin/linux-arm64/agemu" "$stage/bin/linux-arm64/agemud"
archive "agemu-$version-windows-amd64" "$stage/bin/windows-amd64/agemu.exe" "$stage/bin/windows-amd64/agemud.exe"

# npm package: a launcher as the agemu command, and every platform's binaries under platforms/.
pkg="$stage/npm"
mkdir -p "$pkg/bin" "$pkg/platforms/darwin-arm64"
cp npm/agemu.js "$pkg/bin/agemu.js"
cp "${mac_bins[@]/#/bin/}" "$pkg/platforms/darwin-arm64/"
for c in "${cross[@]}"; do
	read -r goos goarch key <<<"$c"
	mkdir -p "$pkg/platforms/$key"
	cp "$stage/bin/$goos-$goarch"/* "$pkg/platforms/$key/"
done
chmod 755 "$pkg/bin/agemu.js" "$pkg"/platforms/*/*
cp npm/README.md LICENSE THIRD_PARTY_NOTICES.md "$pkg/"
sed "s/\"version\": \"0.0.0\"/\"version\": \"$semver\"/" npm/package.json > "$pkg/package.json"
(cd "$pkg" && npm pack --silent --pack-destination "$out" >/dev/null)

(cd "$out" && shasum -a 256 ./*.tar.gz ./*.zip ./*.tgz | sed 's| \./| |' > SHA256SUMS)
ls -lh "$out"
