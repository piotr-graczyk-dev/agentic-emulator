#!/usr/bin/env bash
# End-to-end proof of agemu's iOS backend, driven ONLY through the agemu CLI.
#
#   scripts/e2e-ios.sh                      # uses bin/ios/agemu, state in ~/.agemu-dev-ios
#   AGEMU_BIN=bin/agemu scripts/e2e-ios.sh  # (make ios-e2e)
#
# It creates (if missing) and boots agemu-* simulators only, and exits non-zero if any
# check fails. Screenshots and UI dumps go to $E2E_OUT (a temp dir by default).
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
AGEMU="${AGEMU_BIN:-$ROOT/bin/ios/agemu}"
export AGEMU_HOME="${AGEMU_HOME:-$HOME/.agemu-dev-ios}"
OUT="${E2E_OUT:-$(mktemp -d -t agemu-e2e-ios)}"
mkdir -p "$OUT"
unset AGEMU_DEVICE

CHECKS=0
FAILS=0
LAST=""

now_ms() { python3 -c 'import time; print(int(time.time() * 1000))'; }
step() { printf '\n== %s\n' "$*"; }
ok() { CHECKS=$((CHECKS + 1)); printf '   ok    %s\n' "$*"; }
bad() { CHECKS=$((CHECKS + 1)); FAILS=$((FAILS + 1)); printf '   FAIL  %s\n' "$*"; }

# ag ARGS...: runs agemu, prints the command and its wall time, keeps stdout in $LAST.
ag() {
  local start end rc
  start=$(now_ms)
  LAST=$("$AGEMU" "$@" 2>&1)
  rc=$?
  end=$(now_ms)
  printf '   $ agemu %s  (%d ms)\n' "$*" $((end - start))
  if [[ $rc -ne 0 ]]; then
    printf '%s\n' "$LAST" | sed 's/^/     | /' | tail -5
  fi
  return $rc
}

# expect DESC ARGS...: an agemu command that must succeed.
expect() {
  local desc="$1"
  shift
  if ag "$@"; then ok "$desc"; else bad "$desc"; fi
}

# has DESC PATTERN: $LAST must contain PATTERN (fixed string).
has() {
  if grep -qF -- "$2" <<<"$LAST"; then ok "$1"; else bad "$1 (missing: $2)"; fi
}

json() { python3 -c 'import json,sys; d=json.load(sys.stdin); print(eval("d" + sys.argv[1]))' "$1" <<<"$LAST"; }

png_size() { sips -g pixelWidth -g pixelHeight "$1" 2>/dev/null | awk '/pixelWidth/ {w=$2} /pixelHeight/ {h=$2} END {print w "x" h}'; }

# wait_focused: polls until the text field has focus (keyboard focus lands asynchronously).
wait_focused() {
  local i
  for i in $(seq 1 50); do
    LAST=$("$AGEMU" find --role textfield --json 2>/dev/null)
    if grep -q '"focused":true' <<<"$LAST"; then return 0; fi
    sleep 0.1
  done
  return 1
}

[[ -x "$AGEMU" ]] || { echo "agemu binary not found at $AGEMU (run: make build BIN=bin/ios)"; exit 2; }
echo "agemu: $AGEMU"
echo "AGEMU_HOME: $AGEMU_HOME"
echo "output: $OUT"

step "build the SwiftUI test app (testapps/ios)"
TESTAPP=""
if TESTAPP=$("$ROOT/testapps/ios/build.sh" 2>"$OUT/testapp-build.log"); then
  ok "built $TESTAPP"
else
  bad "test app build failed (see $OUT/testapp-build.log)"
  TESTAPP=""
fi

step "create (if missing) and boot agemu-iphone-17-pro"
expect "create" create --platform ios --profile iphone-17-pro
ID=$(json '["id"]')
expect "boot to agent-ready" boot "$ID"
[[ "$(json '["state"]')" == booted ]] && ok "state is booted" || bad "state: $LAST"
[[ "$(json '["screen"]["width"]')x$(json '["screen"]["height"]')" == 1206x2622 ]] && ok "screen 1206x2622" || bad "screen: $LAST"
export AGEMU_DEVICE="$ID"
expect "reset to a clean state" reset

step "launch Settings; the UI tree shows Settings elements"
expect "launch com.apple.Preferences" launch com.apple.Preferences
expect "wait for the General row" wait --id com.apple.settings.general --timeout-ms 30000
ag ui
printf '%s\n' "$LAST" >"$OUT/settings-ui.txt"
has "frontmost app is Settings" "app=com.apple.Preferences"
has "General row" 'label="General" id=com.apple.settings.general'
has "Accessibility row" 'label="Accessibility"'
has "search field" "textfield"
expect "screenshot Settings" screenshot -o "$OUT/settings.png"

step "tap by selector into General, wait for About"
expect "tap --id com.apple.settings.general" tap --id com.apple.settings.general
has "tap resolved the General element" '"id":"com.apple.settings.general"'
expect "wait for About" wait --id About --timeout-ms 10000
ag ui
printf '%s\n' "$LAST" >"$OUT/general-ui.txt"
has "General screen has About" 'label="About"'
has "General screen has a back button" "id=BackButton"

step "go back (key back: nav-bar back button, else left-edge swipe)"
expect "key back" key back
expect "About is gone" wait --id About --gone --timeout-ms 10000
expect "back on the Settings root" wait --id com.apple.settings.general --timeout-ms 10000

step "type into the Settings search field"
expect "tap the search field" tap --role textfield
if wait_focused; then ok "search field focused"; else bad "search field never got focus"; fi
expect "type Wallpaper" type Wallpaper
expect "typed text appears in the UI tree" wait --role textfield --text Wallpaper --timeout-ms 5000
ag find --role textfield --json
[[ "$(json '[0]["value"]')" == Wallpaper ]] && ok "search field value is Wallpaper" || bad "field: $LAST"
expect "screenshot search" screenshot -o "$OUT/search.png"
expect "close search" tap --text close
expect "back on the Settings root" wait --id com.apple.settings.general --timeout-ms 10000

if [[ -n "$TESTAPP" ]]; then
  step "third-party app: install, launch, tap a button, type"
  expect "install AgemuTestApp.app" install "$TESTAPP"
  expect "launch dev.agemu.testapp" launch dev.agemu.testapp
  expect "wait for the Increment button" wait --id increment --timeout-ms 20000
  expect "count starts at 0" wait --text "Count: 0" --timeout-ms 5000
  expect "tap Increment" tap --id increment
  expect "label changed to Count: 1" wait --text "Count: 1" --timeout-ms 5000
  expect "tap Increment again" tap --text Increment
  expect "label changed to Count: 2" wait --text "Count: 2" --timeout-ms 5000
  expect "tap the name field" tap --id name
  if wait_focused; then ok "name field focused"; else bad "name field never got focus"; fi
  expect "type agemu" type agemu
  expect "greeting shows the typed text" wait --text "Hello, agemu!" --timeout-ms 5000
  expect "screenshot test app" screenshot -o "$OUT/testapp.png"
fi

step "screen profiles: screenshot pixels equal the profile size"
ag profiles --platform ios
PROFILES="$LAST"
profile_size() {
  python3 -c 'import json,sys; [print("%dx%d" % (p["width"], p["height"])) for p in json.load(sys.stdin) if p["name"] == sys.argv[1]]' "$1" <<<"$PROFILES"
}
for P in iphone-se-3 ipad-mini iphone-17-pro; do
  want=$(profile_size "$P")
  if ! ag profile "$P"; then
    bad "switch to profile $P"
    continue
  fi
  ok "switch to profile $P"
  NID=$(json '["id"]')
  [[ "$(json '["profile"]')" == "$P" ]] && ok "device $(json '["name"]') has profile $P" || bad "profile: $LAST"
  [[ -n "$NID" ]] && export AGEMU_DEVICE="$NID"
  expect "screenshot in $P" screenshot -o "$OUT/$P.png"
  got=$(png_size "$OUT/$P.png")
  [[ "$got" == "$want" ]] && ok "$P.png is $got (profile $want)" || bad "$P.png is $got, profile says $want"
  if [[ "$P" == iphone-se-3 ]]; then
    # Tap by selector in a non-default profile (2x scale, 750x1334 pixels).
    expect "SE: launch Settings" launch com.apple.Preferences
    expect "SE: wait for General" wait --id com.apple.settings.general --timeout-ms 30000
    expect "SE: tap --text General" tap --text General
    x=$(json '["x"]')
    y=$(json '["y"]')
    ((x > 0 && x < 750 && y > 0 && y < 1334)) && ok "SE: tap point ($x,$y) is inside 750x1334" || bad "SE: tap point ($x,$y)"
    expect "SE: General screen (About) appears" wait --id About --timeout-ms 10000
    expect "SE: screenshot General" screenshot -o "$OUT/iphone-se-3-general.png"
    got=$(png_size "$OUT/iphone-se-3-general.png")
    [[ "$got" == 750x1334 ]] && ok "SE General screenshot is $got" || bad "SE General screenshot is $got"
    expect "SE: back" key back
    expect "SE: back on the root" wait --id About --gone --timeout-ms 10000
  fi
done

step "reset and verify the clean state (agemu-iphone-17-pro)"
export AGEMU_DEVICE="$ID"
if [[ -n "$TESTAPP" ]]; then
  expect "install the test app (dirty state)" install "$TESTAPP"
  expect "launch the test app" launch dev.agemu.testapp
  expect "wait for the test app" wait --id increment --timeout-ms 20000
fi
expect "launch Settings" launch com.apple.Preferences
expect "wait for General" wait --id com.apple.settings.general --timeout-ms 30000
expect "open General (dirty navigation state)" tap --id com.apple.settings.general
expect "wait for About" wait --id About --timeout-ms 10000
expect "reset" reset
ag ui
printf '%s\n' "$LAST" >"$OUT/after-reset-ui.txt"
has "home screen (SpringBoard) is frontmost after reset" "app=com.apple.springboard"
expect "screenshot after reset" screenshot -o "$OUT/after-reset.png"
if [[ -n "$TESTAPP" ]]; then
  if ag launch dev.agemu.testapp; then bad "test app still installed after reset"; else ok "test app was uninstalled by reset"; fi
fi
expect "Settings relaunches" launch com.apple.Preferences
expect "Settings starts on its root screen" wait --id com.apple.settings.general --timeout-ms 30000
ag find --id About
[[ -z "$LAST" ]] && ok "no leftover General screen" || bad "General screen survived reset: $LAST"

echo
echo "artifacts: $OUT"
echo "$((CHECKS - FAILS))/$CHECKS checks passed"
if ((FAILS > 0)); then
  echo "E2E FAILED"
  exit 1
fi
echo "E2E PASSED"
