#!/usr/bin/env bash
# The agent race: one 18-step task in the Agemu Shop app, driven by several agent tool stacks,
# ROUNDS runs each, the arms' order rotated every round; then the median run of each arm is
# rendered side by side per platform, and both platforms into one montage.
#
#   examples/agent-race/run.sh [android|ios|all] [out-dir]     default: all examples/agent-race/out
#
# Arms. Android: classic (adb + uiautomator), agent-device and Argent on the standard emulator, agemu
# on an agemu device. iOS: agent-device and Argent on a standard Simulator, agemu on an agemu one.
#
# Needs: Go, ffmpeg, uv (Pillow), the agent-device and Argent CLIs in $TOOLS (installed there with
#   npm install --prefix ~/.cache/agemu-race-tools @swmansion/argent@0.32.0 agent-device@0.21.36),
# and running devices:
#   STD_AVD        standard emulator (default Pixel_9_Pro), started with -grpc for the recording:
#                  emulator -avd Pixel_9_Pro -port 5600 -grpc 8600
#   AGEMU_AVD      agemu Android device (default agemu-lab)
#   STD_SIM        standard Simulator's UDID (default: the simulator named race-iphone-17-pro)
#   AGEMU_SIM      agemu iOS device's UDID (default: the simulator named agemu-iphone-17-pro)
#   ROUNDS         runs per arm (default 3)
# While an arm runs on one device, the other platform's devices are expected to be off, and the
# other device of the same platform is paused where that is possible (emulator console
# `avd stop`, agemu park); a standard Simulator cannot be paused and stays idle.
set -euo pipefail
root="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$root"
WHICH="${1:-all}"
OUT="${2:-examples/agent-race/out}"
ROUNDS="${ROUNDS:-3}"
FIRST_ROUND="${FIRST_ROUND:-1}" # to continue an interrupted series
TOOLS="${TOOLS:-$HOME/.cache/agemu-race-tools/node_modules/.bin}"
STD_AVD="${STD_AVD:-Pixel_9_Pro}"
AGEMU_AVD="${AGEMU_AVD:-agemu-lab}"
ADB="${ANDROID_HOME:-$HOME/Library/Android/sdk}/platform-tools/adb"
AGEMU="$root/bin/agemu"
AD="$TOOLS/agent-device"
AR="$TOOLS/argent"
RACE="$OUT/agent-race"
mkdir -p "$OUT"
go build -o "$RACE" ./examples/agent-race

sim_udid() { xcrun simctl list devices -j | python3 -c '
import json, sys
for rt, devs in json.load(sys.stdin)["devices"].items():
    for d in devs:
        if d["name"] == sys.argv[1] and d["isAvailable"]:
            print(d["udid"]); raise SystemExit
' "$1"; }

serial_of() {
  local f
  for f in "$HOME"/Library/Caches/TemporaryItems/avd/running/pid_*.ini; do
    if grep -qx "avd.id=$1" "$f"; then echo "emulator-$(sed -n 's/^port.serial=//p' "$f")"; return; fi
  done
  echo "no running emulator for $1" >&2; exit 1
}

# t runs a command with a time limit (macOS has no timeout(1)).
t() { local s="$1"; shift; perl -e 'alarm shift; exec @ARGV' "$s" "$@"; }

# release stops the agent-device and Argent daemons and their on-device helpers, which hold
# Android's one UiAutomation connection (and the iOS runners), so the next arm starts clean.
release() {
  t 30 "$AD" close >/dev/null 2>&1 || true
  t 30 "$AD" daemon stop >/dev/null 2>&1 || true
  t 30 "$AR" server stop >/dev/null 2>&1 || true
  if [[ -n "${STD_SERIAL:-}" ]]; then
    for p in com.callstack.agentdevice.snapshothelper com.callstack.agentdevice.imehelper com.argent.androiddevtools; do
      t 10 "$ADB" -s "$STD_SERIAL" shell am force-stop "$p" >/dev/null 2>&1 || true
    done
  fi
  sleep 2
}

# argent_up starts Argent's tool-server and waits until it answers (its autospawn from a CLI
# call can time out on a cold start).
argent_up() {
  ("$AR" server start >/dev/null 2>&1 &)
  for _ in $(seq 1 180); do
    "$AR" server status 2>/dev/null | grep -q "health: *ok" && return 0
    sleep 1
  done
  echo "argent tool-server did not come up" >&2
}

race_android() {
  local dir="$OUT/android"
  STD_SERIAL="$(serial_of "$STD_AVD")"
  local apk; apk="$(examples/shop/build.sh)"
  "$ADB" -s "$STD_SERIAL" install -r "$apk" >/dev/null
  "$AGEMU" install "$apk" -d "android:$AGEMU_AVD" >/dev/null
  # Gboard's stylus handwriting mode (and its tutorial sheet) is triggered by some tools'
  # injected touches and then covers the screen for every later arm; it is turned off once.
  "$ADB" -s "$STD_SERIAL" emu avd start >/dev/null 2>&1 || true
  "$ADB" -s "$STD_SERIAL" shell settings put secure stylus_handwriting_enabled 0
  release
  local arms=(classic agent-device argent agemu)
  for i in $(seq "$FIRST_ROUND" "$ROUNDS"); do
    for k in 0 1 2 3; do
      local arm="${arms[$(( (k + i - 1) % 4 ))]}"
      if [[ "$arm" == agemu ]]; then
        "$ADB" -s "$STD_SERIAL" emu avd stop >/dev/null 2>&1 || true
        "$AGEMU" resume -d "android:$AGEMU_AVD" >/dev/null
        sleep 15
        "$RACE" run --platform android --arm agemu --device "$AGEMU_AVD" --agemu "$AGEMU" --out "$dir/runs/agemu-$i" || true
      else
        "$AGEMU" park -d "android:$AGEMU_AVD" >/dev/null
        "$ADB" -s "$STD_SERIAL" emu avd start >/dev/null 2>&1 || true
        sleep 15
        [[ "$arm" == argent ]] && argent_up
        "$RACE" run --platform android --arm "$arm" --device "$STD_AVD" --out "$dir/runs/$arm-$i" || true
        release # while the emulator still runs: a paused one does not answer adb
      fi
    done
  done
  "$ADB" -s "$STD_SERIAL" emu avd start >/dev/null 2>&1 || true
  STD_SERIAL=""
  pick "$dir" classic agent-device argent agemu
  python3 examples/agent-race/stats.py "$dir"
  uv run -q --with pillow examples/agent-race/compose.py $(runs_args "$dir" classic agent-device argent agemu) \
    --title "Android · one agent task, four tool stacks" --out "$dir/race.mp4" --poster "$dir/race.png"
}

race_ios() {
  local dir="$OUT/ios"
  local std="${STD_SIM:-$(sim_udid race-iphone-17-pro)}" agm="${AGEMU_SIM:-$(sim_udid agemu-iphone-17-pro)}"
  [[ -n "$std" && -n "$agm" ]] || { echo "need STD_SIM and AGEMU_SIM" >&2; exit 1; }
  local app; app="$(examples/shop/ios/build.sh)"
  xcrun simctl bootstatus "$std" -b >/dev/null
  xcrun simctl install "$std" "$app"
  "$AGEMU" boot "ios:$agm" >/dev/null
  "$AGEMU" install "$app" -d "ios:$agm" >/dev/null
  release
  local arms=(agent-device argent agemu)
  for i in $(seq "$FIRST_ROUND" "$ROUNDS"); do
    for k in 0 1 2; do
      local arm="${arms[$(( (k + i - 1) % 3 ))]}"
      if [[ "$arm" == agemu ]]; then
        "$AGEMU" resume -d "ios:$agm" >/dev/null
        sleep 15
        "$RACE" run --platform ios --arm agemu --device "$agm" --agemu "$AGEMU" --out "$dir/runs/agemu-$i" || true
      else
        "$AGEMU" park -d "ios:$agm" >/dev/null
        sleep 15
        [[ "$arm" == argent ]] && argent_up
        "$RACE" run --platform ios --arm "$arm" --device "$std" --out "$dir/runs/$arm-$i" || true
        release
      fi
    done
  done
  release
  pick "$dir" agent-device argent agemu
  python3 examples/agent-race/stats.py "$dir"
  uv run -q --with pillow examples/agent-race/compose.py $(runs_args "$dir" agent-device argent agemu) \
    --title "iOS · one agent task, three tool stacks" --out "$dir/race.mp4" --poster "$dir/race.png"
}

# pick writes results.md (every run) and picked.json (the median successful run of each arm).
pick() {
  local dir="$1"; shift
  python3 - "$dir" "$@" <<'EOF'
import json, os, statistics, sys
out, arms = sys.argv[1], sys.argv[2:]
rows, pick = [], {}
for arm in arms:
    runs = []
    for d in sorted(os.listdir(os.path.join(out, "runs"))):
        if d.rsplit("-", 1)[0] == arm and os.path.exists(os.path.join(out, "runs", d, "summary.json")):
            s = json.load(open(os.path.join(out, "runs", d, "summary.json")))
            ev = [json.loads(l) for l in open(os.path.join(out, "runs", d, "events.jsonl"))]
            s["dir"] = d
            s["median_call"] = statistics.median(e["end"] - e["start"] for e in ev) if ev else 0
            runs.append(s)
    ok = sorted((r for r in runs if r["ok"]), key=lambda r: r["total_s"])
    if ok:
        pick[arm] = ok[(len(ok) - 1) // 2]["dir"]
    rows += runs
with open(os.path.join(out, "results.md"), "w") as f:
    f.write("| run | ok | total | tool calls | median call | host load (1, 5, 15 min) |\n|---|---|---|---|---|---|\n")
    for r in rows:
        err = r["error"].replace("|", "/")[:160]
        f.write(f"| {r['dir']}{' (video)' if r['dir'] in pick.values() else ''} | {'yes' if r['ok'] else 'no: ' + err} | "
                f"{r['total_s']:.2f} s | {r['tool_calls']} | {r['median_call'] * 1000:.0f} ms | {r['loadavg'].strip('{} ')} |\n")
json.dump(pick, open(os.path.join(out, "picked.json"), "w"), indent=1)
print(open(os.path.join(out, "results.md")).read())
EOF
}

# runs_args prints --run arm=dir for each arm that has a picked run.
runs_args() {
  local dir="$1"; shift
  python3 - "$dir" "$@" <<'EOF'
import json, os, sys
d, arms = sys.argv[1], sys.argv[2:]
p = json.load(open(os.path.join(d, "picked.json")))
print(" ".join(f"--run {a}={os.path.join(d, 'runs', p[a])}" for a in arms if a in p))
EOF
}

race_spec() { # Name=arm:dir,... for montage.py
  local name="$1" dir="$2"; shift 2
  python3 - "$name" "$dir" "$@" <<'EOF'
import json, os, sys
name, d, arms = sys.argv[1], sys.argv[2], sys.argv[3:]
p = json.load(open(os.path.join(d, "picked.json")))
print(name + "=" + ",".join(f"{a}:{os.path.join(d, 'runs', p[a])}" for a in arms if a in p))
EOF
}

case "$WHICH" in
  android) race_android ;;
  ios) race_ios ;;
  all) race_android; race_ios ;;
  montage) ;;
  *) echo "usage: $0 [android|ios|all|montage] [out-dir]" >&2; exit 2 ;;
esac

if [[ -f "$OUT/android/picked.json" && -f "$OUT/ios/picked.json" ]]; then
  uv run -q --with pillow examples/agent-race/montage.py \
    --race "$(race_spec Android "$OUT/android" classic agent-device argent agemu)" \
    --race "$(race_spec iOS "$OUT/ios" agent-device argent agemu)" \
    --out "$OUT/montage.mp4" --gif "$OUT/teaser.gif"
fi
