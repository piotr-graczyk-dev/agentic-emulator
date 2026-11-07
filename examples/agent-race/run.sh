#!/usr/bin/env bash
# Runs the agent race: the same plan on the standard emulator with adb/uiautomator and on an agemu
# device, alternating, ROUNDS times each; then renders the median run of each side into a
# side-by-side video.
#
#   examples/agent-race/run.sh [out-dir]          default: examples/agent-race/out
#
# Needs: a running standard emulator with gRPC (started as below), an agemu Android device, the
# Android SDK, Go, ffmpeg and uv (for Pillow). Variables:
#   CLASSIC_AVD   standard emulator's AVD (default Pixel_9_Pro), started for example with
#                 emulator -avd Pixel_9_Pro -port 5600 -grpc 8600
#   AGEMU_AVD     agemu's AVD (default agemu-lab; agemu boot android:agemu-lab)
#   ROUNDS        runs per side (default 3)
# While one side runs, the other device is paused (emulator console `avd stop`, agemu park), so
# that it takes no CPU.
set -euo pipefail
root="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$root"
OUT="${1:-examples/agent-race/out}"
CLASSIC_AVD="${CLASSIC_AVD:-Pixel_9_Pro}"
AGEMU_AVD="${AGEMU_AVD:-agemu-lab}"
ROUNDS="${ROUNDS:-3}"
ADB="${ANDROID_HOME:-$HOME/Library/Android/sdk}/platform-tools/adb"
AGEMU="$root/bin/agemu"
mkdir -p "$OUT/runs"

serial_of() { # the adb serial of a running emulator, from its discovery file
  local f
  for f in "$HOME"/Library/Caches/TemporaryItems/avd/running/pid_*.ini; do
    if grep -qx "avd.id=$1" "$f"; then echo "emulator-$(sed -n 's/^port.serial=//p' "$f")"; return; fi
  done
  echo "no running emulator for $1" >&2; exit 1
}
CLASSIC="$(serial_of "$CLASSIC_AVD")"

apk="$(examples/shop/build.sh)"
go build -o "$OUT/agent-race" ./examples/agent-race
"$ADB" -s "$CLASSIC" install -r "$apk" >/dev/null
"$AGEMU" install "$apk" -d "android:$AGEMU_AVD" >/dev/null

run_classic() {
  "$AGEMU" park -d "android:$AGEMU_AVD" >/dev/null
  "$ADB" -s "$CLASSIC" emu avd start >/dev/null || true
  sleep 15 # let the device settle after its resume
  "$OUT/agent-race" run --arm classic --avd "$CLASSIC_AVD" --out "$OUT/runs/classic-$1" || true
}
run_agemu() {
  "$ADB" -s "$CLASSIC" emu avd stop >/dev/null || true
  "$AGEMU" resume -d "android:$AGEMU_AVD" >/dev/null
  sleep 15 # let the device settle after its resume
  "$OUT/agent-race" run --arm agemu --avd "$AGEMU_AVD" --agemu "$AGEMU" --out "$OUT/runs/agemu-$1" || true
}
for i in $(seq 1 "$ROUNDS"); do
  run_classic "$i"
  run_agemu "$i"
done
"$ADB" -s "$CLASSIC" emu avd start >/dev/null || true

# The median successful run of each side, and a results table.
python3 - "$OUT" <<'EOF'
import json, os, statistics, sys
out = sys.argv[1]
rows, pick = [], {}
for arm in ("classic", "agemu"):
    runs = []
    for d in sorted(os.listdir(os.path.join(out, "runs"))):
        if d.startswith(arm + "-"):
            s = json.load(open(os.path.join(out, "runs", d, "summary.json")))
            ev = [json.loads(l) for l in open(os.path.join(out, "runs", d, "events.jsonl"))]
            s["dir"], s["median_call"] = d, statistics.median(e["end"] - e["start"] for e in ev)
            runs.append(s)
    ok = sorted((r for r in runs if r["ok"]), key=lambda r: r["total_s"])
    if not ok:
        sys.exit(f"no successful {arm} run")
    pick[arm] = ok[(len(ok) - 1) // 2]["dir"]
    rows += runs
with open(os.path.join(out, "results.md"), "w") as f:
    f.write("| run | ok | total | tool calls | median call | host load (1, 5, 15 min) |\n|---|---|---|---|---|---|\n")
    for r in rows:
        f.write(f"| {r['dir']}{' (video)' if r['dir'] in pick.values() else ''} | {'yes' if r['ok'] else 'no: ' + r['error']} | "
                f"{r['total_s']:.2f} s | {r['tool_calls']} | {r['median_call'] * 1000:.0f} ms | {r['loadavg'].strip('{} ')} |\n")
json.dump(pick, open(os.path.join(out, "picked.json"), "w"))
print(open(os.path.join(out, "results.md")).read())
EOF
left="$OUT/runs/$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["classic"])' "$OUT/picked.json")"
right="$OUT/runs/$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["agemu"])' "$OUT/picked.json")"
uv run -q --with pillow examples/agent-race/compose.py --left "$left" --right "$right" \
  --out "$OUT/agent-race.mp4" --poster "$OUT/agent-race.png"
