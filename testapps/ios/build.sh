#!/usr/bin/env bash
# Builds testapps/ios/AgemuTestApp into an iOS Simulator .app with swiftc (no Xcode project).
# Prints the path of the .app.
#
# The entitlements (an app group, for clear-data checks) are embedded the way Xcode does for
# simulator builds: in the binary's __TEXT,__entitlements section, and in the ad-hoc signature.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
src="$here/AgemuTestApp"
app="$src/.build/AgemuTestApp.app"
mkdir -p "$app"
if [[ ! -x "$app/AgemuTestApp" || "$src/App.swift" -nt "$app/AgemuTestApp" || "$src/Info.plist" -nt "$app/Info.plist" ||
  "$src/Entitlements.plist" -nt "$app/AgemuTestApp" ]]; then
  xcrun -sdk iphonesimulator swiftc \
    -target arm64-apple-ios17.0-simulator \
    -parse-as-library -O \
    -Xlinker -sectcreate -Xlinker __TEXT -Xlinker __entitlements -Xlinker "$src/Entitlements.plist" \
    "$src/App.swift" -o "$app/AgemuTestApp" >&2
  cp "$src/Info.plist" "$app/Info.plist"
  codesign --force --sign - --entitlements "$src/Entitlements.plist" "$app" >&2
fi
echo "$app"
