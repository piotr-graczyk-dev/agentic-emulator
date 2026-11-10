#!/usr/bin/env bash
# Builds the Android probe app (testapps/android/AgemuProbe) without Gradle:
# aapt2 link, javac, d8, the NDK's clang for libprobe.so, zip, zipalign, apksigner (debug key).
#
#   testapps/android/build.sh [out.apk]      default: testapps/android/build/agemu-probe.apk
#
# Needs: a JDK, Android SDK platform android-36 and build-tools, ~/.android/debug.keystore
# (created by any Android Studio / Gradle build; or keytool -genkeypair, see below). The NDK
# (newest in $SDK/ndk, or $ANDROID_NDK_HOME) builds the native crash library for the host's ABI
# (arm64-v8a on an arm64 host, x86_64 on an x86-64 one, as agemu's system image; AGEMU_PROBE_ABI
# overrides); without it the app builds without the library and its native crash buttons say so.
set -euo pipefail
cd "$(dirname "$0")"
case "$(uname -s)" in
Darwin) default_sdk="$HOME/Library/Android/sdk" ndk_host=darwin-x86_64 ;;
*) default_sdk="$HOME/Android/Sdk" ndk_host=linux-x86_64 ;;
esac
SDK="${ANDROID_HOME:-${ANDROID_SDK_ROOT:-$default_sdk}}"
case "${AGEMU_PROBE_ABI:-$(uname -m)}" in
arm64 | aarch64 | arm64-v8a) ABI=arm64-v8a TRIPLE=aarch64-linux-android ;;
*) ABI=x86_64 TRIPLE=x86_64-linux-android ;;
esac
PLATFORM="${AGEMU_ANDROID_PLATFORM:-android-36}"
ANDROID_JAR="$SDK/platforms/$PLATFORM/android.jar"
BT="$SDK/build-tools/${AGEMU_BUILD_TOOLS:-$(ls -1 "$SDK/build-tools" | sort -V | tail -1)}"
OUT_APK="${1:-$PWD/build/agemu-probe.apk}"
B=build/probe
KS="${AGEMU_DEBUG_KEYSTORE:-$HOME/.android/debug.keystore}"

[[ -f "$ANDROID_JAR" ]] || { echo "missing $ANDROID_JAR" >&2; exit 1; }
if [[ ! -f "$KS" ]]; then
  KS=build/debug.keystore
  [[ -f "$KS" ]] || keytool -genkeypair -keystore "$KS" -storepass android -keypass android \
    -alias androiddebugkey -dname "CN=Android Debug,O=Android,C=US" -keyalg RSA -validity 10000 >/dev/null
fi

rm -rf "$B"
mkdir -p "$B/classes" "$B/dex" "$(dirname "$OUT_APK")"
"$BT/aapt2" link --manifest AgemuProbe/AndroidManifest.xml -I "$ANDROID_JAR" \
  --min-sdk-version 30 --target-sdk-version 36 -o "$B/base.apk"
javac -nowarn -Xlint:-options -source 8 -target 8 -encoding UTF-8 \
  -bootclasspath "$ANDROID_JAR" -classpath "$ANDROID_JAR" \
  -d "$B/classes" $(find AgemuProbe/src -name '*.java')
"$BT/d8" --release --min-api 30 --lib "$ANDROID_JAR" --output "$B/dex" $(find "$B/classes" -name '*.class')
cp "$B/base.apk" "$B/unaligned.apk"
(cd "$B/dex" && zip -q -X ../unaligned.apk classes.dex)
NDK="${ANDROID_NDK_HOME:-$(ls -d "$SDK"/ndk/* 2>/dev/null | sort -V | tail -1)}"
CC="$NDK/toolchains/llvm/prebuilt/$ndk_host/bin/${TRIPLE}30-clang"
if [[ -n "$NDK" && -x "$CC" ]]; then
  # 16 KB segment alignment: the system images agemu uses (ps16k) have 16 KB pages.
  mkdir -p "$B/lib/lib/$ABI"
  "$CC" -shared -fPIC -O1 -Wl,-z,max-page-size=16384 -o "$B/lib/lib/$ABI/libprobe.so" AgemuProbe/jni/probe.c -llog
  (cd "$B/lib" && zip -q -X ../unaligned.apk "lib/$ABI/libprobe.so")
else
  echo "build.sh: no NDK found; building without libprobe.so (native crash buttons disabled)" >&2
fi
"$BT/zipalign" -f -p 4 "$B/unaligned.apk" "$B/aligned.apk"
"$BT/apksigner" sign --ks "$KS" --ks-pass pass:android --key-pass pass:android \
  --ks-key-alias androiddebugkey --out "$OUT_APK" "$B/aligned.apk"
rm -rf "$B"
echo "$OUT_APK"
