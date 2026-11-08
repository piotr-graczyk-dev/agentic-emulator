#!/usr/bin/env bash
# Packages a release from bin/ (build it first with `make build VERSION=<version>`):
#
#   scripts/package.sh <version> [out-dir]     e.g. scripts/package.sh v0.1.0 dist
#
# writes to out-dir (default dist/):
#   agemu-<version>-darwin-arm64.tar.gz   the binaries, for GitHub Releases and install.sh
#   agentic-emulator-<semver>.tgz         the npm package (npm publish <file>)
#   SHA256SUMS
set -euo pipefail
cd "$(dirname "$0")/.."

version="${1:?usage: scripts/package.sh <version> [out-dir]}"
semver="${version#v}"
out="${2:-dist}"
bins=(agemu agemud agemu-ios-helper)

[[ "$semver" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] || { echo "not a version: $version" >&2; exit 2; }
for b in "${bins[@]}"; do
	[ -x "bin/$b" ] || { echo "bin/$b missing: make build VERSION=$version" >&2; exit 1; }
	file "bin/$b" | grep -q 'arm64' || { echo "bin/$b is not an arm64 binary" >&2; exit 1; }
done
built="$(AGEMU_HOME="$(mktemp -d)" bin/agemu version | sed -n 's/^agemu  *//p')"
[ "$built" = "$version" ] || { echo "bin/agemu is version $built, not $version: make build VERSION=$version" >&2; exit 1; }

rm -rf "$out" && mkdir -p "$out" && out="$(cd "$out" && pwd)"
stage="$(mktemp -d)"
trap 'rm -rf "$stage"' EXIT

# Tarball: one directory with the binaries and the licenses.
name="agemu-$version-darwin-arm64"
mkdir -p "$stage/$name"
cp "${bins[@]/#/bin/}" LICENSE THIRD_PARTY_NOTICES.md "$stage/$name/"
tar -C "$stage" -czf "$out/$name.tar.gz" "$name"

# npm package: the binaries under bin/, agemu as the command.
pkg="$stage/npm"
mkdir -p "$pkg/bin"
cp "${bins[@]/#/bin/}" "$pkg/bin/"
chmod 755 "$pkg"/bin/*
cp npm/README.md LICENSE THIRD_PARTY_NOTICES.md "$pkg/"
sed "s/\"version\": \"0.0.0\"/\"version\": \"$semver\"/" npm/package.json > "$pkg/package.json"
(cd "$pkg" && npm pack --silent --pack-destination "$out" >/dev/null)

(cd "$out" && shasum -a 256 ./*.tar.gz ./*.tgz | sed 's| \./| |' > SHA256SUMS)
ls -lh "$out"
