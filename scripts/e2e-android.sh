#!/usr/bin/env bash
# End-to-end proof for the Android backend. Everything goes through the agemu CLI
# ($AGEMU_BIN, default bin/android/agemu; agemud must sit next to it)
# (no adb, no emulator commands); python3 reads PNG sizes and times steps on the host
# ($PYTHON names another interpreter, e.g. python on Windows). It runs on macOS, Linux and in
# Git Bash on Windows. $AGEMU_E2E_PROBE_APK uses a prebuilt probe app instead of building it.
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
AG="${AGEMU_BIN:-$PWD/bin/android/agemu}"
OUT="$(mktemp -d /tmp/agemu-e2e-android.XXXXXX)"
PASS=0

[[ -x "$AG" ]] || { echo "build first: make android-build (or make build and AGEMU_BIN=bin/agemu)" >&2; exit 2; }

step() { printf '\n== %s\n' "$*"; }
ok() { PASS=$((PASS + 1)); printf '   ok: %s\n' "$*"; }
fail() { printf '   FAIL: %s\n' "$*" >&2; exit 1; }
ag() { "$AG" "$@" -d "$DEV"; }
# timed runs a command and prints its wall time.
timed() {
  local t0 t1 out
  t0=$("${PYTHON:-python3}" -c 'import time; print(time.time())')
  out=$("$@") || { echo "$out"; return 1; }
  t1=$("${PYTHON:-python3}" -c 'import time; print(time.time())')
  printf '   (%s: %.0f ms)\n' "$*" "$("${PYTHON:-python3}" -c "print(($t1-$t0)*1000)")" >&2
  printf '%s' "$out"
}
# png_size reads a PNG's width and height from its IHDR chunk (sips on macOS is not needed).
png_size() { "${PYTHON:-python3}" -c 'import struct,sys; d=open(sys.argv[1],"rb").read(24); print("%dx%d" % struct.unpack(">II", d[16:24]))' "$1"; }

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
# Settings' search screen belongs to another package and survives a force-stop of Settings.
ag terminate com.android.settings >/dev/null
ag terminate com.google.android.settings.intelligence >/dev/null
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

# Test capabilities (docs/CAPABILITIES.md), checked as the probe app sees them. The probe
# (testapps/android) is built with the SDK build tools. Without them the run fails, unless
# AGEMU_E2E_SKIP_CAPS=1 asks to skip this part (then it does not claim that all checks passed).
PROBE=dev.agemu.probe
if [[ -n "${AGEMU_E2E_PROBE_APK:-}" ]]; then
  cp "$AGEMU_E2E_PROBE_APK" "$OUT/agemu-probe.apk" || fail "no probe app at $AGEMU_E2E_PROBE_APK"
elif ! testapps/android/build.sh "$OUT/agemu-probe.apk" >"$OUT/probe-build.log" 2>&1; then
  if [[ "${AGEMU_E2E_SKIP_CAPS:-0}" == 1 ]]; then
    printf '\n== capabilities: SKIPPED (could not build the probe app, see %s)\n' "$OUT/probe-build.log"
    printf '\n%d CHECKS PASSED, CAPABILITY CHECKS SKIPPED (artifacts in %s)\n' "$PASS" "$OUT"
    exit 0
  fi
  fail "could not build the probe app for the capability checks (see $OUT/probe-build.log; AGEMU_E2E_SKIP_CAPS=1 skips them)"
fi
probe() { ag launch "$PROBE" >/dev/null; ag wait --contains "pid:" --timeout-ms 8000 >/dev/null || fail "probe did not start"; }
ui_has() { ag wait --contains "$1" --timeout-ms "${2:-5000}" >/dev/null; }

step "capabilities: install the probe app; apps lists it"
ag install "$OUT/agemu-probe.apk" >/dev/null
ag apps | grep -q "\"id\":\"$PROBE\",\"name\":\"Agemu Probe\",\"version\":\"1.3\"" || fail "apps: $(ag apps)"
ok "apps lists $PROBE with its name and version"

step "capabilities: open a deep link as a tapped link (BROWSABLE)"
ag open "agemuprobe://orders/7" --wait-contains "url: agemuprobe://orders/7 (browsable)" >"$OUT/open.json" || fail "open: $(cat "$OUT/open.json")"
grep -q "\"app\":\"$PROBE\"" "$OUT/open.json" || fail "open did not report the handler: $(cat "$OUT/open.json")"
ok "open delivered the link to $PROBE"
ag open "agemunb://x" >/dev/null 2>&1 && fail "an activity without CATEGORY_BROWSABLE opened from a link"
ok "an activity without CATEGORY_BROWSABLE does not open (as from a real link)"

step "capabilities: permissions (the app sees them; revoke kills it, as Settings does)"
probe
ag permission grant "$PROBE" camera >/dev/null
ui_has "camera: granted" || fail "app does not see camera granted"
pid=$(ag ui | grep -o '"pid: [0-9]*' | head -1)
ag permission revoke "$PROBE" camera >/dev/null
probe
[[ "$(ag ui | grep -o '"pid: [0-9]*' | head -1)" != "$pid" ]] || fail "the app's process survived the revoke"
ag tap --text "Ask camera" --wait-contains "ask camera: denied" >/dev/null || fail "after revoke the app could ask again"
ok "grant seen; revoke killed the process and the app cannot ask again"
ag permission reset "$PROBE" camera >/dev/null
probe
ag tap --text "Ask camera" --wait-contains "While using the app" >/dev/null || fail "after reset the system dialog did not show"
ag key back >/dev/null
ok "after reset the app's request shows the system dialog"
set +e
ag permission grant "$PROBE" motion >/dev/null 2>&1; rc1=$?
ag permission grant "$PROBE" photos-add >/dev/null 2>&1; rc2=$?
set -e
[[ $rc1 == 1 && $rc2 == 3 ]] || fail "refusals: grant motion exit $rc1 (want 1), photos-add exit $rc2 (want 3)"
ok "a permission the app does not request is refused (exit 1); photos-add is not implemented (exit 3)"

step "capabilities: location, clipboard, appearance"
ag permission grant "$PROBE" location >/dev/null
probe
ag location set 52.2297 21.0122 >/dev/null
ui_has "location: 52.22970,21.01220" || fail "app does not see the location"
ok "app's GPS listener sees 52.2297,21.0122"
ag clipboard set "agemu e2e clip" >/dev/null
[[ "$(ag clipboard)" == "agemu e2e clip" ]] || fail "clipboard get: $(ag clipboard)"
ag tap --text Refresh >/dev/null
ui_has 'clipboard: "agemu e2e clip"' || fail "app does not read the clip"
ok "clipboard set/get, and the app reads the clip"
ag appearance dark >/dev/null
ui_has "appearance: dark" 2000 || fail "app is not dark right after appearance dark returned"
ok "appearance dark: the app is recreated dark when the call returns"

step "park: the device stops in place and the next call resumes it with the app as it was"
probe
pid=$(ag ui | grep -o '"pid: [0-9]*' | head -1)
ag park >/dev/null
"$AG" devices | grep -q "\"id\":\"$DEV\"[^}]*\"state\":\"parked\"" || fail "devices does not list $DEV as parked: $("$AG" devices)"
ok "devices lists $DEV as parked"
[[ "$(ag ui | grep -o '"pid: [0-9]*' | head -1)" == "$pid" ]] || fail "ui after park does not show the same app process ($pid)"
"$AG" devices | grep -q "\"id\":\"$DEV\"[^}]*\"state\":\"booted\"" || fail "not booted after the call that resumed it: $("$AG" devices)"
ok "ui resumed it, with the same app process ($pid) on screen"
ag resume | grep -q '"resumed":false' || fail "resume of a running device: $(ag resume)"
ok "resume of a running device does nothing"

step "capabilities: reset restores light, the default location, an empty clipboard, no probe"
timed ag reset >/dev/null
[[ "$(ag appearance)" == '{"appearance":"light"}' ]] || fail "appearance after reset: $(ag appearance)"
[[ "$(ag clipboard)" == "" ]] || fail "clipboard after reset: $(ag clipboard)"
[[ "$(ag apps)" == "[]" ]] || fail "apps after reset: $(ag apps)"
ag install "$OUT/agemu-probe.apk" >/dev/null
ag permission grant "$PROBE" location >/dev/null
probe
ui_has "location: 39.23726,-123.15003" 8000 || fail "location after reset is not the emulator's default fix"
ok "after reset: light, empty clipboard, no user apps, default GPS fix"

step "capabilities: clear-data and uninstall"
probe
ui_has "launches: 1" || fail "fresh install did not start at 1 launch"
ag permission grant "$PROBE" camera >/dev/null
ag clear-data "$PROBE" >/dev/null
probe
ui_has "launches: 1" || fail "clear-data kept the app's data"
ui_has "camera: denied" || fail "clear-data kept the app's permissions"
ok "clear-data: data and permissions gone"
ag uninstall "$PROBE" >/dev/null
[[ "$(ag apps)" == "[]" ]] || fail "uninstall: $(ag apps)"
ok "uninstall removed $PROBE"
set +e
ag push "$PROBE" '{"aps":{"alert":"x"}}' >/dev/null 2>&1; rc=$?
set -e
[[ $rc == 3 ]] || fail "push exit $rc, want 3 (not implemented on android)"
ok "push answers not implemented (exit 3)"

# Logs and crashes (docs/CAPABILITIES.md, "Logs and crashes"), with the probe's lab screen:
# agemulab://<action> logs at each level, crashes in Java or native code, exits, or logs from
# its :remote process.
LAB="$PROBE/.LabActivity"
# entries prints "level process tag message" per entry of a logs --json result on stdin.
entries() { "${PYTHON:-python3}" -c 'import json,sys; [print(e["level"], e.get("process",""), e.get("tag",""), (e["message"].splitlines() or [""])[0]) for e in json.load(sys.stdin)["entries"]]'; }
cursor() { "${PYTHON:-python3}" -c 'import json,sys; print(json.load(sys.stdin)["cursor"])'; }
lab() { ag launch "$LAB" >/dev/null; ag wait --contains "lab: ready" --timeout-ms 8000 >/dev/null || fail "lab screen did not start"; }

step "logs: levels, the app's processes only, cursor, since launch"
ag install "$OUT/agemu-probe.apk" >/dev/null
lab
# since launch: from the start of the launch (an open of a link counts as one too, below). While
# the device logs over 100 records/s (a cold start does) the capture reads logd every 250 ms.
for _ in 1 2 3 4 5 6 7 8 9 10; do
  ag logs "$PROBE" --since launch --contains "lab: created" --json | entries | grep -q "lab: created" && break
  sleep 0.2
done
ag logs "$PROBE" --since launch --contains "lab: created" --json | entries | grep -q "lab: created" || fail "since launch lacks the app's startup line"
ag open "agemulab://log-levels" >/dev/null
sleep 1
ag logs "$PROBE" --since launch --contains "level " --json | entries >"$OUT/levels.txt"
for l in "verbose dev.agemu.probe AgemuLab level V" "debug dev.agemu.probe AgemuLab level D" "info dev.agemu.probe AgemuLab level I" \
         "warn dev.agemu.probe AgemuLab level W" "error dev.agemu.probe AgemuLab level E"; do
  grep -q "^$l " "$OUT/levels.txt" || fail "logs: no \"$l\" line: $(cat "$OUT/levels.txt")"
done
ok "a line at each level with its level, process and tag"
ag logs "$PROBE" --since launch --limit 10000 --max-bytes 1048576 --json | entries | awk '{print $2}' | sort -u >"$OUT/procs.txt"
[[ "$(cat "$OUT/procs.txt")" == "dev.agemu.probe" ]] || fail "logs of $PROBE holds other processes: $(cat "$OUT/procs.txt")"
ok "logs <app> holds only the app's lines, from the start of its launch"
cur=$(ag logs "$PROBE" --limit 1 --json | cursor)
ag open "agemulab://remote-log" >/dev/null
sleep 1
ag logs "$PROBE" --since "$cur" --json | entries >"$OUT/after-cursor.txt"
grep -q "^info dev.agemu.probe:remote AgemuLabRemote remote: started" "$OUT/after-cursor.txt" || fail "no line from the :remote process: $(cat "$OUT/after-cursor.txt")"
grep -q "level " "$OUT/after-cursor.txt" && fail "the cursor returned older lines"
ok "a cursor returns only newer lines; the app's :remote process is in its logs"

step "crashes: the notice, a failing action's exit 4, the report's frames"
ag open "agemulab://crash-java" >/dev/null 2>"$OUT/crash-open.err" || true
sleep 1
set +e
ag tap --text "Log levels" >"$OUT/crash-tap.out" 2>&1; rc=$?
set -e
# A crash is told once, with the first call that ends after it. When the app crashed before
# `open` returned, `open` carried the notice and the tap fails like any tap on a missing element.
if grep -q "crashed at" "$OUT/crash-open.err"; then
  [[ $rc == 1 ]] || fail "tap after a crash open already told: exit $rc, want 1: $(cat "$OUT/crash-tap.out")"
  grep -q "crashed at" "$OUT/crash-tap.out" && fail "the crash was told twice: $(cat "$OUT/crash-open.err" "$OUT/crash-tap.out")"
else
  [[ $rc == 4 ]] || fail "tap after the crash: exit $rc, want 4: $(cat "$OUT/crash-tap.out")"
fi
grep -q "crashed at .*java.lang.IllegalStateException: probe crash" "$OUT/crash-open.err" "$OUT/crash-tap.out" || fail "no crash notice: $(cat "$OUT/crash-open.err" "$OUT/crash-tap.out")"
ag crashes "$PROBE" --limit 1 >"$OUT/crash-java.txt"
grep -q "java.lang.IllegalStateException: probe crash" "$OUT/crash-java.txt" || fail "crashes: $(cat "$OUT/crash-java.txt")"
grep -q "at dev.agemu.probe.LabActivity.crashJava" "$OUT/crash-java.txt" || fail "crashes lacks the app's frame: $(cat "$OUT/crash-java.txt")"
ok "Java crash: one notice (with open, or with the tap, which then exits 4), crashes has the exception and the app's frame"
lab
ag open "agemulab://crash-native" >/dev/null 2>&1 || true
for _ in 1 2 3 4 5 6; do
  ag crashes "$PROBE" --limit 1 >"$OUT/crash-native.txt"
  grep -q "libprobe.so" "$OUT/crash-native.txt" && break
  sleep 0.5
done
grep -q "SIGSEGV (SEGV_MAPERR)" "$OUT/crash-native.txt" && grep -q "probe_null_write" "$OUT/crash-native.txt" \
  || fail "native crash: $(cat "$OUT/crash-native.txt")"
ok "native crash: SIGSEGV (SEGV_MAPERR) with the tombstone's frame in libprobe.so"
ag terminate "$PROBE" >/dev/null # no "keeps stopping" dialog left over
# For this second crash within a minute Android may already show "keeps stopping" (in one run
# 24 ms after ActivityManager handled the crash, before the terminate, which does not close it).
if ag wait --contains "keeps stopping" --timeout-ms 1000 >/dev/null 2>&1; then
  ag tap --text "Close app" >/dev/null
fi
lab
ag open "agemulab://exit" >/dev/null 2>&1 || true
set +e
ag wait --text "Log levels" --timeout-ms 9000 >"$OUT/exit-wait.out" 2>&1; rc=$?
set -e
[[ $rc == 4 ]] && grep -q "exited unexpectedly" "$OUT/exit-wait.out" || fail "wait after System.exit: exit $rc: $(cat "$OUT/exit-wait.out")"
ok "System.exit: a wait stops with exit 4 (exited unexpectedly)"
lab
ag terminate "$PROBE" >/dev/null 2>"$OUT/term.err"
ag ui >/dev/null 2>>"$OUT/term.err"
grep -q "note:" "$OUT/term.err" && fail "terminate produced a notice: $(cat "$OUT/term.err")"
ok "terminate is not reported"

step "logs and crashes: reset empties both"
ag reset >/dev/null
ag logs "$PROBE" | grep -q "no log lines match" || fail "logs after reset: $(ag logs "$PROBE" | head -3)"
ag crashes | grep -q "no crashes" || fail "crashes after reset: $(ag crashes | head -3)"
ok "after reset: no lines and no crash reports from before"
ag key home >/dev/null

printf '\nALL %d CHECKS PASSED (artifacts in %s)\n' "$PASS" "$OUT"
