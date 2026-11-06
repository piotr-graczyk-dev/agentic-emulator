#!/usr/bin/env bash
# Builds the agemu on-device agent (android-agent/src) into a dex jar and copies it to
# internal/android/assets/agemu-agent.jar, which the Go backend embeds with go:embed.
# The built jar is committed, so `go build` never needs the Android build tools.
#
# Needs: a JDK (javac), Android SDK platform android-36 and build-tools (d8).
set -euo pipefail
cd "$(dirname "$0")"
SDK="${ANDROID_HOME:-${ANDROID_SDK_ROOT:-$HOME/Library/Android/sdk}}"
PLATFORM="${AGEMU_ANDROID_PLATFORM:-android-36}"
ANDROID_JAR="$SDK/platforms/$PLATFORM/android.jar"
BT="${AGEMU_BUILD_TOOLS:-$(ls -1 "$SDK/build-tools" | sort -V | tail -1)}"
D8="$SDK/build-tools/$BT/d8"
OUT=build
DEST=../internal/android/assets/agemu-agent.jar

[[ -f "$ANDROID_JAR" ]] || { echo "missing $ANDROID_JAR" >&2; exit 1; }
[[ -x "$D8" ]] || { echo "missing $D8" >&2; exit 1; }

rm -rf "$OUT"
mkdir -p "$OUT/classes"
# -source/-target 8 with the Android bootclasspath: compile against android.jar, not the JDK.
javac -nowarn -Xlint:-options -source 8 -target 8 -encoding UTF-8 \
  -bootclasspath "$ANDROID_JAR" -classpath "$ANDROID_JAR" \
  -d "$OUT/classes" $(find src -name '*.java')
"$D8" --release --min-api 30 --lib "$ANDROID_JAR" --output "$OUT/agemu-agent.jar" $(find "$OUT/classes" -name '*.class')
mkdir -p "$(dirname "$DEST")"
cp "$OUT/agemu-agent.jar" "$DEST"
shasum -a 256 "$DEST"
