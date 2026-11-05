#!/usr/bin/env bash
# Builds testapps/ios/AgemuTestApp into an iOS Simulator .app with swiftc (no Xcode project).
# Prints the path of the .app.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
src="$here/AgemuTestApp"
app="$src/.build/AgemuTestApp.app"
mkdir -p "$app"
if [[ ! -x "$app/AgemuTestApp" || "$src/App.swift" -nt "$app/AgemuTestApp" || "$src/Info.plist" -nt "$app/Info.plist" ]]; then
  xcrun -sdk iphonesimulator swiftc \
    -target arm64-apple-ios17.0-simulator \
    -parse-as-library -O \
    "$src/App.swift" -o "$app/AgemuTestApp" >&2
  cp "$src/Info.plist" "$app/Info.plist"
  codesign --force --sign - "$app" >&2
fi
echo "$app"
