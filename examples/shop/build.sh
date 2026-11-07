#!/usr/bin/env bash
# Builds the Agemu Shop example app without Gradle: aapt2 link, javac, d8, zip, zipalign,
# apksigner (debug key).
#
#   examples/shop/build.sh [out.apk]      default: examples/shop/build/agemu-shop.apk
#
# Needs: a JDK, Android SDK platform android-36 and build-tools, ~/.android/debug.keystore
# (or one is generated in build/).
set -euo pipefail
cd "$(dirname "$0")"
SDK="${ANDROID_HOME:-${ANDROID_SDK_ROOT:-$HOME/Library/Android/sdk}}"
PLATFORM="${AGEMU_ANDROID_PLATFORM:-android-36}"
ANDROID_JAR="$SDK/platforms/$PLATFORM/android.jar"
BT="$SDK/build-tools/${AGEMU_BUILD_TOOLS:-$(ls -1 "$SDK/build-tools" | sort -V | tail -1)}"
OUT_APK="${1:-$PWD/build/agemu-shop.apk}"
B=build/shop
KS="${AGEMU_DEBUG_KEYSTORE:-$HOME/.android/debug.keystore}"

[[ -f "$ANDROID_JAR" ]] || { echo "missing $ANDROID_JAR" >&2; exit 1; }
if [[ ! -f "$KS" ]]; then
  KS=build/debug.keystore
  mkdir -p build
  [[ -f "$KS" ]] || keytool -genkeypair -keystore "$KS" -storepass android -keypass android \
    -alias androiddebugkey -dname "CN=Android Debug,O=Android,C=US" -keyalg RSA -validity 10000 >/dev/null
fi

rm -rf "$B"
mkdir -p "$B/classes" "$B/dex" "$(dirname "$OUT_APK")"
"$BT/aapt2" link --manifest AndroidManifest.xml -I "$ANDROID_JAR" \
  --min-sdk-version 30 --target-sdk-version 36 -o "$B/base.apk"
javac -nowarn -Xlint:-options -source 8 -target 8 -encoding UTF-8 \
  -bootclasspath "$ANDROID_JAR" -classpath "$ANDROID_JAR" \
  -d "$B/classes" $(find src -name '*.java')
"$BT/d8" --release --min-api 30 --lib "$ANDROID_JAR" --output "$B/dex" $(find "$B/classes" -name '*.class')
cp "$B/base.apk" "$B/unaligned.apk"
(cd "$B/dex" && zip -q -X ../unaligned.apk classes.dex)
"$BT/zipalign" -f -p 4 "$B/unaligned.apk" "$B/aligned.apk"
"$BT/apksigner" sign --ks "$KS" --ks-pass pass:android --key-pass pass:android \
  --ks-key-alias androiddebugkey --out "$OUT_APK" "$B/aligned.apk"
rm -rf "$B"
echo "$OUT_APK"
