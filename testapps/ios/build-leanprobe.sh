#!/usr/bin/env bash
# Builds testapps/ios/LeanProbe (lean simulator feature probe) into an iOS Simulator .app with
# swiftc, like build.sh. Prints the path of the .app.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
src="$here/LeanProbe"
app="$src/.build/LeanProbe.app"
mkdir -p "$app"
if [[ ! -x "$app/LeanProbe" || "$src/App.swift" -nt "$app/LeanProbe" || "$src/Info.plist" -nt "$app/Info.plist" ||
  "$src/Entitlements.plist" -nt "$app/LeanProbe" ]]; then
  xcrun -sdk iphonesimulator swiftc \
    -target arm64-apple-ios26.0-simulator \
    -parse-as-library -O \
    -Xlinker -sectcreate -Xlinker __TEXT -Xlinker __entitlements -Xlinker "$src/Entitlements.plist" \
    "$src/App.swift" -o "$app/LeanProbe" >&2
  cp "$src/Info.plist" "$app/Info.plist"
  # Restricted entitlements (HealthKit) go only into the __entitlements section, which the
  # simulator reads; in an ad-hoc signature they make the launch fail (POSIX 163).
  codesign --force --sign - "$app" >&2
fi
echo "$app"
