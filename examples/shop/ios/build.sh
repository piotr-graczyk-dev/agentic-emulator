#!/usr/bin/env bash
# Builds the iOS Agemu Shop (examples/shop/ios) into a Simulator .app with swiftc, no Xcode
# project. Prints the path of the .app.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
app="$here/../build/AgemuShop.app"
mkdir -p "$app"
if [[ ! -x "$app/AgemuShop" || "$here/ShopApp.swift" -nt "$app/AgemuShop" || "$here/Info.plist" -nt "$app/Info.plist" ]]; then
  xcrun -sdk iphonesimulator swiftc -target arm64-apple-ios17.0-simulator -parse-as-library -O \
    "$here/ShopApp.swift" -o "$app/AgemuShop" >&2
  cp "$here/Info.plist" "$app/Info.plist"
  codesign --force --sign - "$app" >&2
fi
cd "$(dirname "$app")" && echo "$PWD/AgemuShop.app"
