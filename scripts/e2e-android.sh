#!/usr/bin/env bash
# End-to-end proof for the Android backend. Everything goes through bin/android/agemu
# (no adb, no emulator commands); `sips` only reads PNG sizes on the host.
#
#   scripts/e2e-android.sh [avd-name]        default: agemu-pixel
#
# Uses $AGEMU_HOME (default ~/.agemu-dev-android) for the daemon state. Exits non-zero on
# the first failed check.
set -euo pipefail
cd "$(dirname "$0")/.."

AVD="${1:-agemu-pixel}"
DEV="android:$AVD"
export AGEMU_HOME="${AGEMU_HOME:-$HOME/.agemu-dev-android}"
AG="$PWD/bin/android/agemu"
OUT="$(mktemp -d /tmp/agemu-e2e-android.XXXXXX)"
PASS=0

[[ -x "$AG" ]] || { echo "build first: go build -tags noios -o bin/android/agemu ./cmd/agemu" >&2; exit 2; }

step() { printf '\n== %s\n' "$*"; }
ok() { PASS=$((PASS + 1)); printf '   ok: %s\n' "$*"; }
fail() { printf '   FAIL: %s\n' "$*" >&2; exit 1; }
ag() { "$AG" "$@" -d "$DEV"; }
# timed runs a command and prints its wall time.
timed() {
  local t0 t1 out
  t0=$(python3 -c 'import time; print(time.time())')
  out=$("$@") || { echo "$out"; return 1; }
  t1=$(python3 -c 'import time; print(time.time())')
  printf '   (%s: %.0f ms)\n' "$*" "$(python3 -c "print(($t1-$t0)*1000)")" >&2
  printf '%s' "$out"
}
png_size() { sips -g pixelWidth -g pixelHeight "$1" | awk '/pixelWidth/{w=$2} /pixelHeight/{h=$2} END{print w "x" h}'; }

step "create (if missing) and boot $DEV"
if "$AG" devices | grep -q "\"id\":\"$DEV\""; then
  ok "$DEV exists"
else
  "$AG" create --platform android --name "$AVD" >/dev/null
  "$AG" devices | grep -q "\"id\":\"$DEV\"" || fail "create did not produce $DEV"
  ok "created $DEV"
fi
timed "$AG" boot "$DEV" >"$OUT/boot.json"
grep -q '"state":"booted"' "$OUT/boot.json" || fail "boot: $(cat "$OUT/boot.json")"
ok "booted: $(cut -c1-160 "$OUT/boot.json")"
ag profile pixel-9 >/dev/null
ok "profile pixel-9 (native)"

step "launch Settings and read the UI tree"
ag terminate com.android.settings >/dev/null
timed ag launch com.android.settings >/dev/null
ag ui >"$OUT/ui-settings.txt"
head -1 "$OUT/ui-settings.txt"
grep -q 'app=com.android.settings' "$OUT/ui-settings.txt" || fail "foreground app is not Settings: $(head -1 "$OUT/ui-settings.txt")"
grep -q '"Network & internet"' "$OUT/ui-settings.txt" || fail "Settings home not in UI tree"
ok "ui shows Settings ($(wc -l <"$OUT/ui-settings.txt" | tr -d ' ') outline lines)"

step "tap by selector into a sub-screen, then wait"
timed ag tap --text "Network & internet" >"$OUT/tap.json"
ok "tapped $(grep -o '"frame":{[^}]*}' "$OUT/tap.json") at $(grep -o '"x":[0-9]*,"y":[0-9]*' "$OUT/tap.json" | tail -1)"
timed ag wait --text "Internet" --timeout-ms 8000 >/dev/null || fail "sub-screen element 'Internet' did not appear"
ok "sub-screen 'Network & internet' shows 'Internet'"

step "key back (returns once queued; wait shows its effect)"
timed ag key back >/dev/null
ag wait --text "Search Settings" --timeout-ms 8000 >/dev/null || fail "back did not return to Settings home"
ok "back returned to Settings home"

step "type into the Settings search field (sets the field's text; the app must react)"
ag tap --text "Search Settings" >/dev/null
ag wait --id open_search_view_edit_text --timeout-ms 8000 >/dev/null || fail "search field did not appear"
timed ag type "net" >/dev/null
timed ag type "work" >/dev/null
ag find --id open_search_view_edit_text --json >"$OUT/field.json"
cat "$OUT/field.json"; echo
grep -q '"text":"network"' "$OUT/field.json" || fail "typed text not in the UI tree (want \"network\": the second type appends)"
ok "search field contains \"network\" (two type calls, appended at the cursor)"
ag wait --id breadcrumb --timeout-ms 15000 >"$OUT/result.json" || fail "Settings search showed no results for \"network\""
ag find --id breadcrumb >"$OUT/results.txt"
head -3 "$OUT/results.txt"
ok "Settings search lists $(wc -l <"$OUT/results.txt" | tr -d ' ') results for \"network\" (TextWatcher ran the query)"
ag screenshot -o "$OUT/shot-search.png" >/dev/null

step "type --keys (one key event per character through the keyboard)"
ag tap --text "Clear text" >/dev/null || fail "no Clear text button"
ag wait --id breadcrumb --gone --timeout-ms 8000 >/dev/null || fail "results did not clear"
timed ag type --keys "wifi" >/dev/null
ag find --id open_search_view_edit_text --json >"$OUT/field-keys.json"
grep -q '"text":"wifi"' "$OUT/field-keys.json" || fail "type --keys: field is $(cat "$OUT/field-keys.json")"
ag wait --id breadcrumb --timeout-ms 15000 >/dev/null || fail "Settings search showed no results for \"wifi\""
ok "type --keys typed \"wifi\" and Settings lists results"
ag key back >/dev/null
ag key back >/dev/null
ag wait --text "Search Settings" --timeout-ms 8000 >/dev/null || ag key back >/dev/null

step "screenshots in profiles (PNG size must equal the profile size)"
for p in pixel-9:1080x2424 small-phone:720x1600 pixel-fold-inner:2076x2152 pixel-tablet:1600x2560 galaxy-s24:1080x2340; do
  name=${p%%:*} want=${p##*:}
  timed ag profile "$name" >/dev/null
  timed ag screenshot -o "$OUT/shot-$name.png" >/dev/null
  got=$(png_size "$OUT/shot-$name.png")
  [[ "$got" == "$want" ]] || fail "$name: screenshot is $got, want $want"
  ag ui >"$OUT/ui-$name.txt"
  tree=$(head -1 "$OUT/ui-$name.txt" | grep -o 'screen [0-9]*x[0-9]*' | cut -d' ' -f2)
  [[ "$tree" == "$want" ]] || fail "$name: UI tree screen is $tree, want $want"
  ok "$name: screenshot $got, UI tree $tree"
done
ag profile --width 900 --height 1800 --density 360 >/dev/null
ag screenshot -o "$OUT/shot-custom.png" >/dev/null
[[ "$(png_size "$OUT/shot-custom.png")" == "900x1800" ]] || fail "custom 900x1800@360 screenshot is $(png_size "$OUT/shot-custom.png")"
ok "custom 900x1800@360: screenshot 900x1800"

step "tap by selector in non-default profiles"
for name in small-phone pixel-fold-inner; do
  ag profile "$name" >/dev/null
  ag terminate com.android.settings >/dev/null
  ag launch com.android.settings >/dev/null
  ag wait --text "Search Settings" --timeout-ms 8000 >/dev/null || fail "$name: Settings home not shown"
  ag find --text "Internet" --json | grep -q '"ref"' && fail "$name: already on the sub-screen before the tap"
  ag tap --text "Network & internet" >"$OUT/tap-$name.json"
  ag wait --text "Internet" --timeout-ms 8000 >/dev/null || fail "$name: tap did not open the sub-screen"
  ok "$name: tap at $(grep -o '"x":[0-9]*,"y":[0-9]*' "$OUT/tap-$name.json" | tail -1) opened 'Network & internet'"
done

step "reset to the clean baseline"
# Leave dirty state behind: Settings open in a sub-screen, in the small-phone profile.
ag profile small-phone >/dev/null
ag wait --text "Internet" --timeout-ms 8000 >/dev/null || fail "dirty state not set up"
timed ag reset >/dev/null
ag ui >"$OUT/ui-reset.txt"
head -1 "$OUT/ui-reset.txt"
grep -q 'app=com.android.settings' "$OUT/ui-reset.txt" && fail "Settings still in front after reset"
grep -q 'screen 720x1600' "$OUT/ui-reset.txt" || fail "profile not kept across reset: $(head -1 "$OUT/ui-reset.txt")"
ok "after reset: launcher in front, small-phone profile kept"
ag launch com.android.settings >/dev/null
ag wait --text "Search Settings" --timeout-ms 8000 >/dev/null || fail "Settings did not start fresh"
ag find --text "Internet" --json | grep -q '"ref"' && fail "Settings resumed its old sub-screen: state survived reset"
ok "Settings starts on its home screen (its old task is gone)"
ag profile pixel-9 >/dev/null
ag key home >/dev/null

printf '\nALL %d CHECKS PASSED (artifacts in %s)\n' "$PASS" "$OUT"
