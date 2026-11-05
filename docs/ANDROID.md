# Android backend

The Android backend runs the **stock** Android emulator (SDK QEMU with Apple
Hypervisor.framework) and the **stock** SDK system image
(`system-images;android-37.1;google_apis_playstore_ps16k;arm64-v8a`). Nothing in the guest
is replaced or patched. agemu only replaces the control plane, which is where the standard
tools spend their time.

```
agemu daemon (Go, internal/android)
 ├─ emulator gRPC (EmulatorController, SnapshotService)   screenshots, touch, snapshots, VM state
 │    token from ~/Library/Caches/TemporaryItems/avd/running/pid_*.ini, sent as a Bearer header
 ├─ adb server smart-socket protocol (127.0.0.1:5037)     no adb process per call
 │    └─ "localabstract:agemu-agent" ──► on-device agent
 └─ on-device agent (android-agent/, Java, app_process as the shell user)
      one long-lived UiAutomation connection: UI tree, launch/stop, display profiles,
      key and text injection
```

## Run it

```sh
go build -tags noios -o bin/android/agemu ./cmd/agemu          # or: make android-build
export AGEMU_HOME=$HOME/.agemu-dev-android                      # optional, default ~/.agemu
bin/android/agemu create --platform android --name pixel        # -> android:agemu-pixel (no download)
bin/android/agemu boot android:agemu-pixel                      # first boot also builds the baseline (~1 min)
bin/android/agemu launch com.android.settings
bin/android/agemu ui                                            # outline; --json for the full tree
bin/android/agemu tap --text "Network & internet"
bin/android/agemu profile pixel-fold-inner                      # or --width 900 --height 1800 --density 360
bin/android/agemu screenshot -o shot.png                        # 2076x2152 PNG
bin/android/agemu reset                                         # clean baseline, profile kept
bin/android/agemu shutdown android:agemu-pixel                  # standby; a second shutdown powers off
scripts/e2e-android.sh                                          # end-to-end proof (CLI only)
bin/android/agemu-bench --platform android --device android:agemu-pixel --agemu bin/android/agemu
```

Environment variables (read by the daemon):

| Variable | Default | Meaning |
|---|---|---|
| `AGEMU_ANDROID_AVDS` | `agemu-` | Comma-separated AVD name prefixes agemu manages (`*` = all). |
| `AGEMU_ANDROID_GPU` | `auto` | Emulator `-gpu` mode. `host` is faster but was unstable here. |
| `AGEMU_ANDROID_RAM_MB` | `2048` | Guest RAM for new AVDs. Below 4096 the emulator needs `-lowram`. |
| `AGEMU_ANDROID_STANDBY` | `1` | `0`: `shutdown` always powers off. |
| `AGEMU_ANDROID_CONSOLE_PORT` / `AGEMU_ANDROID_GRPC_PORT` | `5580` / `8580` | First ports to try. |
| `AGEMU_ANDROID_SHM` | `1` | `0`: frames travel in the gRPC reply instead of a shared mmap. |
| `AGEMU_ANDROID_BASELINE_SETTLE_S` | `20` | Settle time before the baseline snapshot. |

## How each operation works

| Operation | Standard tool | agemu |
|---|---|---|
| boot | cold boot | `shutdown` parks the device in **standby**: the baseline snapshot is restored in place and the VM is paused. `boot` resumes it. When the device is powered off, `boot` starts the emulator with `-snapshot agemu-baseline`. |
| reset | cold boot | gRPC `LoadSnapshot(agemu-baseline)` in the running emulator, then wait for the agent. The active screen profile is re-applied. |
| screenshot | `adb exec-out screencap -p` | gRPC `getScreenshot` (RGBA). The emulator writes into a shared, file-backed mmap, so a 10 MB frame skips protobuf and HTTP/2. agemu crops and resamples to the profile size, then encodes with a parallel PNG encoder (about 5 ms for 1080x2424). |
| tap / swipe | `adb shell input tap/swipe` | gRPC `sendTouch` on the emulator's virtio multitouch screen, the same device a finger would use. |
| type / key | `adb shell input text/keyevent` | The agent injects KeyEvents through `IInputManager.injectInputEvent`, the call `input` makes, from a long-lived process. The last event waits until the app has handled it. |
| UI tree | `uiautomator dump` + reading the XML | The agent walks `AccessibilityNodeInfo` of every on-screen window, top-most first, over its warm UiAutomation connection, and returns compact JSON. The Go side maps it to `api.Element`. |
| launch | `am start -W` | The agent calls `IActivityTaskManager.startActivityAndWait`, the call `am start -W` makes, then waits until the app's window is the active accessibility window. |

### Device lifecycle

- **`create`** runs `avdmanager create avd` on the system image that is already installed, and never downloads one. It then patches `config.ini`:
  - display = the profile (default `pixel-9`, 1080x2424@420), `hw.ramSize=2048`;
  - phone hardware: `hw.keyboard=no`, `hw.mainKeys=no` (gesture navigation), no dpad, trackball or SD card;
  - quickboot auto-snapshots off.
- **`boot`** runs `emulator -avd <name> -port 5580 -grpc 8580 -no-window -no-audio -no-boot-anim -gpu auto -memory 2048 -lowram -no-snapshot-save` plus either `-snapshot agemu-baseline` or `-no-snapshot-load` for a cold boot.
  - The process is detached from the daemon.
  - Ports are taken from 5580/8580 upward and skip ports used by any running emulator.
- **Agent-ready** means `sys.boot_completed=1` **and** the on-device agent answers.
- **Baseline.** On the first boot agemu waits 20 s for the device to settle, then:
  - warms up the keyboard (see Caveats);
  - clears recent tasks and goes home;
  - saves the one snapshot it keeps, `agemu-baseline`, with the agent running inside it. Every boot or reset from it is agent-ready at once.
  - If agemu is upgraded and the snapshot holds an older agent, it is retaken once.
- **`boot --cold`** is a real cold boot with `-no-snapshot-load`. It keeps the existing baseline.
- **`shutdown`** parks the device in standby. A second `shutdown` (or `AGEMU_ANDROID_STANDBY=0`) powers the emulator off.
- Only AVDs named `agemu-*` are managed (`AGEMU_ANDROID_AVDS` changes this), so agemu never drives emulators other tools use.

### Screen profiles

A profile is applied at runtime as a display size and density override, the same thing
`wm size` and `wm density` do. The agent calls `IWindowManager.setForcedDisplaySize` and
`setForcedDisplayDensityForUser`, then waits until the foreground app's window has
re-laid out at the new size and the display-change animation has finished. No reboot is needed. A switch takes about 1–3 s on this host, most of
it the app relaunching for the new configuration.

The emulator's framebuffer keeps its native size (the AVD's create-time profile). Android
scales the logical display into it, keeping the aspect ratio and centring it. agemu
mirrors that mapping exactly; `screenMap` uses the integer math of
`LogicalDisplay.configureDisplayLocked`, which is unit-tested and was checked against
`dumpsys display`:

- **Screenshots** crop the display rectangle out of the framebuffer and resample it to the
  profile's pixel size (box filter when shrinking, bilinear when enlarging). PNG size equals
  the profile size.
- **UI-tree frames** are logical display pixels, the same grid as the screenshot.
- **Taps** map a logical pixel to the framebuffer pixel that Android's InputReader maps back
  onto it.

The native profile is pixel-exact. `wm scaling off` would make every profile pixel-exact,
but on API 37 it has no effect: WindowManager shows `noscale`, while DisplayManager keeps
`mDisplayScalingDisabled=false` and still scales.

## Benchmark

`bin/android/agemu-bench --platform android --device android:agemu-pixel --agemu bin/android/agemu --n 20`

Every row runs on the same `agemu-pixel` emulator:

- **Baseline:** the standard tool, called directly against `emulator-5580`.
- **agemu CLI:** one `agemu` process per call.
- **daemon/MCP:** the same call over a persistent daemon connection (what `agemu mcp` uses).

Boot and reset use N=3. The cold-boot baseline uses the same emulator flags as agemu, minus
the snapshot.

### Final table (default configuration: `-gpu auto`, software rendering)

Two runs on 2026-11-05, both pasted unchanged from `agemu-bench`.
- **Rows screenshot … launch:** full run at 22:30–22:37, host load average 30–315.
- **Rows profile switch, boot and reset:** re-run at 22:54–23:06, load 210–260. It came after fixing a reconnect bug that made the first run's reset rows fail, and after the profile-switch change.

| Operation | Baseline tool | Baseline | agemu CLI | agemu daemon/MCP | Speedup (CLI) | Speedup (daemon) |
|---|---|---|---|---|---|---|
| screenshot | `adb exec-out screencap -p > f.png` | 214.2 ms (p95 1465.1, n=20) | 44.8 ms (p95 160.2, n=20) | 31.9 ms (p95 83.4, n=20) | **4.8×** | **6.7×** |
| screenshot (small-phone profile) | `adb exec-out screencap -p > f.png` | 272.8 ms (p95 1080.3, n=20) | 92.0 ms (p95 2124.6, n=20) | 110.9 ms (p95 481.5, n=20) | **3.0×** | **2.5×** |
| ui tree | `adb shell uiautomator dump + adb exec-out cat` | 2046.1 ms (p95 3534.1, n=20) | 11.1 ms (p95 14.0, n=20) | 2.8 ms (p95 4.3, n=20) | **183.5×** | **735.5×** |
| tap | `adb shell input tap 800 1100` | 63.5 ms (p95 606.9, n=20) | 13.4 ms (p95 31.3, n=20) | 1.0 ms (p95 13.3, n=20) | **4.8×** | **64.8×** |
| swipe (300 ms gesture) | `adb shell input swipe 540 1800 540 1000 300` | 614.8 ms (p95 1248.2, n=20) | 338.8 ms (p95 465.0, n=20) | 307.2 ms (p95 330.2, n=20) | **1.8×** | **2.0×** |
| swipe (50 ms gesture) | `adb shell input swipe 540 1800 540 1000 50` | 250.2 ms (p95 637.6, n=20) | 78.2 ms (p95 109.4, n=20) | 55.5 ms (p95 88.8, n=20) | **3.2×** | **4.5×** |
| type text (plain field) | `adb shell input text hello` | 206.8 ms (p95 1806.1, n=20) | 314.5 ms (p95 726.5, n=20) | 323.8 ms (p95 846.4, n=20) | **0.7×** | **0.6×** |
| type text (Settings search) | `adb shell input text hello` | 558.1 ms (p95 1660.5, n=20) | 260.7 ms (p95 515.4, n=20) | 213.9 ms (p95 529.4, n=20) | **2.1×** | **2.6×** |
| key home | `adb shell input keyevent KEYCODE_HOME` | 152.9 ms (p95 537.1, n=20) | 30.4 ms (p95 188.0, n=20) | 17.0 ms (p95 87.1, n=20) | **5.0×** | **9.0×** |
| launch app (cold start) | `adb shell am start -W -n com.android.settings/.Settings` | 3066.1 ms (p95 11368.1, n=10) | 3204.8 ms (p95 7148.7, n=10) | 3037.5 ms (p95 13671.6, n=10) | **1.0×** | **1.0×** |
| launch app (warm, in background) | `adb shell am start -W -n com.android.settings/.Settings` | 567.7 ms (p95 1857.1, n=10) | 583.5 ms (p95 1352.0, n=10) | 942.3 ms (p95 1471.5, n=10) | **1.0×** | **0.6×** |
| profile switch | `adb shell wm size WxH && adb shell wm density D` | 1001.4 ms (p95 1489.0, n=10) | 2922.3 ms (p95 5513.5, n=10) | 2069.8 ms (p95 2618.5, n=10) | **0.3×** | **0.5×** |
| boot to agent-ready (from standby) | `emulator cold boot (same flags, -no-snapshot-load) until sys.boot_completed=1` | 19733.1 ms (p95 19858.6, n=3) | 89.0 ms (p95 166.2, n=3) | 53.0 ms (p95 68.2, n=3) | **221.8×** | **372.5×** |
| boot to agent-ready (powered off) | `emulator cold boot (same flags, -no-snapshot-load) until sys.boot_completed=1` | 33160.1 ms (p95 52491.0, n=3) | 7572.2 ms (p95 8577.3, n=3) | 5924.1 ms (p95 17630.5, n=3) | **4.4×** | **5.6×** |
| reset to clean state | `adb emu kill + emulator cold boot until sys.boot_completed=1` | 41584.0 ms (p95 149463.0, n=3) | 6934.4 ms (p95 7209.2, n=3) | 6001.7 ms (p95 6430.5, n=3) | **6.0×** | **6.9×** |
| reset vs emulator snapshot load | `adb emu avd snapshot load agemu-baseline until sys.boot_completed=1` | 6444.8 ms (p95 6716.3, n=3) | 7900.3 ms (p95 8605.1, n=3) | 9683.4 ms (p95 12742.1, n=3) | **0.8×** | **0.7×** |

Raw output of the first run (its last three rows are superseded by the re-run above):

```
Host: Apple M2 Pro, macOS 26.6, arm64. Device: `android:agemu-pixel`. Median of n runs (p95 and n in parentheses).
| Operation | Baseline tool | Baseline | agemu CLI | agemu daemon/MCP | Speedup (CLI) | Speedup (daemon) |
|---|---|---|---|---|---|---|
| screenshot | `adb exec-out screencap -p > f.png` | 214.2 ms (p95 1465.1, n=20) | 44.8 ms (p95 160.2, n=20) | 31.9 ms (p95 83.4, n=20) | **4.8×** | **6.7×** |
| screenshot (small-phone profile) | `adb exec-out screencap -p > f.png` | 272.8 ms (p95 1080.3, n=20) | 92.0 ms (p95 2124.6, n=20) | 110.9 ms (p95 481.5, n=20) | **3.0×** | **2.5×** |
| ui tree | `adb shell uiautomator dump + adb exec-out cat` | 2046.1 ms (p95 3534.1, n=20) | 11.1 ms (p95 14.0, n=20) | 2.8 ms (p95 4.3, n=20) | **183.5×** | **735.5×** |
| tap | `adb shell input tap 800 1100` | 63.5 ms (p95 606.9, n=20) | 13.4 ms (p95 31.3, n=20) | 1.0 ms (p95 13.3, n=20) | **4.8×** | **64.8×** |
| swipe (300 ms gesture) | `adb shell input swipe 540 1800 540 1000 300` | 614.8 ms (p95 1248.2, n=20) | 338.8 ms (p95 465.0, n=20) | 307.2 ms (p95 330.2, n=20) | **1.8×** | **2.0×** |
| swipe (50 ms gesture) | `adb shell input swipe 540 1800 540 1000 50` | 250.2 ms (p95 637.6, n=20) | 78.2 ms (p95 109.4, n=20) | 55.5 ms (p95 88.8, n=20) | **3.2×** | **4.5×** |
| type text (plain field) | `adb shell input text hello` | 206.8 ms (p95 1806.1, n=20) | 314.5 ms (p95 726.5, n=20) | 323.8 ms (p95 846.4, n=20) | **0.7×** | **0.6×** |
| type text (Settings search) | `adb shell input text hello` | 558.1 ms (p95 1660.5, n=20) | 260.7 ms (p95 515.4, n=20) | 213.9 ms (p95 529.4, n=20) | **2.1×** | **2.6×** |
| key home | `adb shell input keyevent KEYCODE_HOME` | 152.9 ms (p95 537.1, n=20) | 30.4 ms (p95 188.0, n=20) | 17.0 ms (p95 87.1, n=20) | **5.0×** | **9.0×** |
| launch app (cold start) | `adb shell am start -W -n com.android.settings/.Settings` | 3066.1 ms (p95 11368.1, n=10) | 3204.8 ms (p95 7148.7, n=10) | 3037.5 ms (p95 13671.6, n=10) | **1.0×** | **1.0×** |
| launch app (warm, in background) | `adb shell am start -W -n com.android.settings/.Settings` | 567.7 ms (p95 1857.1, n=10) | 583.5 ms (p95 1352.0, n=10) | 942.3 ms (p95 1471.5, n=10) | **1.0×** | **0.6×** |
| profile switch | `adb shell wm size WxH && adb shell wm density D` | 394.0 ms (p95 708.0, n=10) | 2076.5 ms (p95 2827.6, n=10) | 2075.2 ms (p95 3131.4, n=10) | **0.2×** | **0.2×** |
| boot to agent-ready (from standby) | `emulator cold boot (same flags, -no-snapshot-load) until sys.boot_completed=1` | 21294.0 ms (p95 27889.8, n=3) | 1549.5 ms (p95 1828.1, n=3) | 326.5 ms (p95 671.5, n=3) | **13.7×** | **65.2×** |
| boot to agent-ready (powered off) | `emulator cold boot (same flags, -no-snapshot-load) until sys.boot_completed=1` | 23106.1 ms (p95 30022.3, n=3) | 49960.1 ms (p95 71481.2, n=3) | 78005.8 ms (p95 90038.3, n=3) | **0.5×** | **0.3×** |
| reset to clean state | `adb emu kill + emulator cold boot until sys.boot_completed=1` | 31085.3 ms (p95 40519.6, n=3) | ERR: /Users/piotrgraczyk/.t3/scratch/2026-11-05-czy-jest-mo-liwe-aby-470a7eab/bin/android/agemu reset -d android:agemu-pixel: exit status 1: error: rpc error: code = Canceled desc = grpc: the client connection is closing | ERR: rpc error: code = Canceled desc = grpc: the client connection is closing | - | - |
| reset vs emulator snapshot load | `adb emu avd snapshot load agemu-baseline until sys.boot_completed=1` | 5782.3 ms (p95 6135.3, n=3) | ERR: /Users/piotrgraczyk/.t3/scratch/2026-11-05-czy-jest-mo-liwe-aby-470a7eab/bin/android/agemu reset -d android:agemu-pixel: exit status 1: error: rpc error: code = Canceled desc = grpc: the client connection is closing | ERR: rpc error: code = Canceled desc = grpc: the client connection is closing | - | - |
```

Raw output of the re-run:

```
Host: Apple M2 Pro, macOS 26.6, arm64. Device: `android:agemu-pixel`. Median of n runs (p95 and n in parentheses).
| Operation | Baseline tool | Baseline | agemu CLI | agemu daemon/MCP | Speedup (CLI) | Speedup (daemon) |
|---|---|---|---|---|---|---|
| profile switch | `adb shell wm size WxH && adb shell wm density D` | 1001.4 ms (p95 1489.0, n=10) | 2922.3 ms (p95 5513.5, n=10) | 2069.8 ms (p95 2618.5, n=10) | **0.3×** | **0.5×** |
| boot to agent-ready (from standby) | `emulator cold boot (same flags, -no-snapshot-load) until sys.boot_completed=1` | 19733.1 ms (p95 19858.6, n=3) | 89.0 ms (p95 166.2, n=3) | 53.0 ms (p95 68.2, n=3) | **221.8×** | **372.5×** |
| boot to agent-ready (powered off) | `emulator cold boot (same flags, -no-snapshot-load) until sys.boot_completed=1` | 33160.1 ms (p95 52491.0, n=3) | 7572.2 ms (p95 8577.3, n=3) | 5924.1 ms (p95 17630.5, n=3) | **4.4×** | **5.6×** |
| reset to clean state | `adb emu kill + emulator cold boot until sys.boot_completed=1` | 41584.0 ms (p95 149463.0, n=3) | 6934.4 ms (p95 7209.2, n=3) | 6001.7 ms (p95 6430.5, n=3) | **6.0×** | **6.9×** |
| reset vs emulator snapshot load | `adb emu avd snapshot load agemu-baseline until sys.boot_completed=1` | 6444.8 ms (p95 6716.3, n=3) | 7900.3 ms (p95 8605.1, n=3) | 9683.4 ms (p95 12742.1, n=3) | **0.8×** | **0.7×** |
```

### Against the 10x target

**Meets 10x:**

- **UI tree:** 184x via the CLI, 736x via the daemon. A warm UiAutomation connection plus the accessibility cache, against `uiautomator dump`, which starts a process, waits for idle and writes XML.
- **Boot from standby:** 222x CLI, 373x daemon. Resuming a paused VM that already sits at the baseline.
- **Tap, via daemon/MCP:** 65x. One gRPC call.
- **Screenshot, via daemon/MCP:** 14.7x with the host GPU; **6.7x** with the default software rendering, where the framebuffer readback takes most of the 32 ms.

**Misses 10x, and why:**

- **CLI calls.** Every `agemu` call starts a 17 MB Go process: 10–25 ms on this loaded host (`agemu --help` measured 18.8 ms median, `/usr/bin/true` 3.7 ms). Android 17's `adb shell input tap` costs only 40–65 ms, because `input` is now a thin `cmd input` client rather than a new ART runtime. So the CLI tops out around 5x for tap and key. MCP and other persistent clients don't pay this.
- **Swipe.** The gesture duration is part of the operation for both tools: 300 ms of the 307 ms, so 2x; 50 ms of 56 ms, so 4.5x.
- **Type text.** agemu waits until the app has handled every key, as `input text` does, and each key goes through the IME. The app and IME work dominates: 1.5–2.6x when the host was quieter, 0.6x on the plain field in the final run under load (software-rendered IME). An asynchronous burst was faster but raced the keyboard binding and duplicated characters, so it was dropped.
- **Key.** 8.6–9.0x via the daemon, for the same reason.
- **Launch.** The app's own process start and first frame dominate (cold start about 0.4–3 s depending on host load). agemu also waits until the app's window is the active accessibility window, which `am start -W` does not. Expect parity: 0.6–1.1x.
- **Reset.** A full in-place VM restore: 2 GB of RAM, GPU state, disk, then adb reconnecting. The snapshot load alone is 1.7–5 s by the emulator's own log. That gives 6–7x against a cold boot (agemu reset 6–7 s), and parity with the emulator's own `avd snapshot load` command. No control-plane change can avoid the restore; a pre-reset second VM would need a second emulator, more than this host's RAM allows.
- **Boot without standby** (new emulator process loading the snapshot): 4.4–5.6x, for the same reason as reset.
- **Profile switch.** Not one of the agent-loop operations, and below 1x by design. agemu returns once the foreground app has re-laid out and the display-change animation has finished, so the next screenshot is settled. `wm size` / `wm density` return before any of that happens.

### Open issues seen during testing

- **Guest restarts.** The guest restarted several times inside a running emulator during about three hours of testing (uptime reset, same qemu PID, boot reason `reboot`), mostly within a minute after a snapshot load, at host load averages of 100–340.
  - No cause was found: no system_server crash in dropbox, no pstore on this image.
  - agemu now recovers. An agent call that finds the device offline waits for `sys.boot_completed` and restarts the agent, but that call takes 30–60 s.
  - After switching to `-gpu auto`, the final e2e and benchmark re-runs had no restarts.
- **Host load** made absolute times noisy. Compare the p95 columns.

### Supplementary run: `AGEMU_ANDROID_GPU=host`

These are the cheap operations measured earlier (22:11–22:16, load 40–230, from a fresh reset) with the host GPU (gfxstream). They are faster, mostly because
screenshots read back from the host GPU in about 9 ms. With the host GPU this emulator was
not stable on this machine: gfxstream `error null ctx` / `Failed to create EGL fence sync`, a
guest that stopped answering adb, and repeated guest restarts. So the default is `-gpu auto`. The
boot and reset rows of this run did not complete; the cold boot hung.

```
== screenshot (n=20)
   baseline 199.1 ms (p95 869.7, n=20) | agemu cli 26.6 ms (p95 174.1, n=20) | daemon 13.6 ms (p95 16.5, n=20) | speedup cli 7.5x daemon 14.7x
== screenshot (small-phone profile) (n=20)
   baseline 192.8 ms (p95 623.6, n=20) | agemu cli 26.2 ms (p95 52.4, n=20) | daemon 15.9 ms (p95 30.9, n=20) | speedup cli 7.4x daemon 12.1x
== ui tree (n=20)
   baseline 2252.2 ms (p95 8284.7, n=20) | agemu cli 12.8 ms (p95 39.7, n=20) | daemon 4.3 ms (p95 16.6, n=20) | speedup cli 176.0x daemon 522.1x
== tap (n=20)
   baseline 40.8 ms (p95 384.7, n=20) | agemu cli 8.9 ms (p95 21.3, n=20) | daemon 0.8 ms (p95 4.5, n=20) | speedup cli 4.6x daemon 53.4x
== swipe (300 ms gesture) (n=20)
   baseline 349.8 ms (p95 368.8, n=20) | agemu cli 314.4 ms (p95 364.8, n=20) | daemon 302.4 ms (p95 311.0, n=20) | speedup cli 1.1x daemon 1.2x
== swipe (50 ms gesture) (n=20)
   baseline 112.9 ms (p95 138.4, n=20) | agemu cli 64.1 ms (p95 76.1, n=20) | daemon 52.5 ms (p95 70.5, n=20) | speedup cli 1.8x daemon 2.2x
== type text (plain field) (n=20)
   baseline 58.8 ms (p95 720.3, n=20) | agemu cli 72.3 ms (p95 174.0, n=20) | daemon 38.4 ms (p95 89.8, n=20) | speedup cli 0.8x daemon 1.5x
== type text (Settings search) (n=20)
   baseline 99.8 ms (p95 401.7, n=20) | agemu cli 63.6 ms (p95 184.4, n=20) | daemon 37.9 ms (p95 79.6, n=20) | speedup cli 1.6x daemon 2.6x
== key home (n=20)
   baseline 51.1 ms (p95 134.2, n=20) | agemu cli 19.2 ms (p95 62.8, n=20) | daemon 5.9 ms (p95 32.2, n=20) | speedup cli 2.7x daemon 8.6x
== launch app (cold start) (n=10)
   baseline 445.1 ms (p95 675.7, n=10) | agemu cli 394.1 ms (p95 540.7, n=10) | daemon 455.6 ms (p95 3051.1, n=10) | speedup cli 1.1x daemon 1.0x
== launch app (warm, in background) (n=10)
   baseline 89.7 ms (p95 188.1, n=10) | agemu cli 152.7 ms (p95 323.3, n=10) | daemon 155.8 ms (p95 276.7, n=10) | speedup cli 0.6x daemon 0.6x
== profile switch (n=10)
   baseline 361.2 ms (p95 528.5, n=10) | agemu cli 1188.1 ms (p95 1532.6, n=10) | daemon 874.5 ms (p95 1252.0, n=10) | speedup cli 0.3x daemon 0.4x
```

## Caveats

- **2 GB guest RAM needs `-lowram`.** Emulator 37.1 raises guest RAM to 4096 MB for every API > 36 image.
  - Disassembling its `main-common.c` logic shows `-lowram` is the only switch that removes this lower bound.
  - On this emulator `-lowram` does **not** set `ro.config.low_ram` in the guest. Verified: the property is empty, and `dumpsys meminfo` reports `Total RAM: 2,041,792K` with `Tuning: ... (high-end-gfx)`, not low-ram. So the device behaves like a normal 2 GB phone.
  - agemu re-checks `ro.config.low_ram` after every cold boot and logs a warning if it is ever set.
  - `AGEMU_ANDROID_RAM_MB=4096` drops the flag. The snapshot then grows from about 2 GB to about 4 GB.
- **Non-native profiles are resampled.** Only the AVD's native profile is pixel-exact; see Screen profiles.
  - For larger profiles (fold, tablet, pixel-9-pro on a 1080x2424 AVD) the framebuffer has fewer pixels than the profile, so screenshots are upscaled and slightly soft.
  - Taps land within one framebuffer pixel, which is up to 2 profile pixels for the fold profile.
  - Create the AVD with `--profile <name>` to make that profile native.
- **Keys and text are injected, like `adb shell input`.** A phone AVD (`hw.keyboard=no`, so the soft keyboard behaves as on a real phone) has no keyboard device, so the emulator's gRPC `sendKey` has nothing to drive. Touches do go through the emulator's virtual touchscreen.
- **One UiAutomation client at a time.** Android allows only one UiAutomation connection per device, and the agemu agent holds it.
  - While the agent runs, `uiautomator dump` and UiAutomation-based instrumentation tests (UiDevice, Espresso with UiAutomator) cannot connect.
  - Stop the agent with `adb -s <serial> shell 'kill $(pidof agemu-agent)'`. agemu restarts it on its next call.
- **Stylus tutorial.** The emulator's virtio touchscreen advertises a stylus (`BTN_STYLUS`, source `TOUCHSCREEN | STYLUS`). On first text input Gboard therefore opens a one-time "Try out your stylus" sheet that takes focus from the text field. agemu triggers and dismisses it once while it builds the baseline, so it does not come back after every reset.
- **Tap returns when the touch is queued.** `agemu tap` returns once the emulator has queued the touch on its virtual touchscreen. `adb shell input tap` injects and waits until the app has handled the event. Agents should follow a tap with `wait` or `ui`; both run in milliseconds. `type`, `key` and `launch` do wait until the app has handled the input or shown its window.
- **Standby keeps guest RAM allocated.** A device in standby uses no CPU, but the paused emulator keeps its guest RAM (about 2 GB) on the host. Run `shutdown` twice, or set `AGEMU_ANDROID_STANDBY=0`, to free it.
- **Disk.** One baseline snapshot is about 2 GB (`ram.bin` is the full guest RAM). The AVD's userdata qcow2 grows to about 3 GB. `ANDROID_SNAPSHOT_COMPRESS=1` (via `AGEMU_ANDROID_SNAPSHOT_COMPRESS=1`) cuts the snapshot to about 40 % at a similar load time.
- **Rotation is not handled.** Profiles are portrait; a landscape app or device rotation is not mapped.
- **The measurements ran on a saturated host.** The M2 Pro with 16 GB shared the machine with another emulator, iOS simulators, a browser and other sessions, at load averages of 50–340. Absolute numbers are noisy (see the p95 columns); both sides of each row ran back to back under the same conditions.
