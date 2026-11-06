# Android backend

The Android backend runs the **stock** Android emulator (SDK QEMU with Apple
Hypervisor.framework) and the **stock** SDK system image
(`system-images;android-37.1;google_apis_playstore_ps16k;arm64-v8a`). Nothing in the guest
is replaced or patched. agemu only replaces the control plane, which is where the standard
tools spend their time.

```
agemu daemon (Go, internal/android)
 ├─ emulator gRPC (EmulatorController, SnapshotService)   touch, snapshots, VM state
 │    ├─ streamScreenshot (MMAP) ──► latest posted frame kept in host memory (stream.go)
 │    token from ~/Library/Caches/TemporaryItems/avd/running/pid_*.ini, sent as a Bearer header
 ├─ emulator console (telnet, port 5580)                   `redir add`: host 127.0.0.1:<port> ──► guest :39271
 │    └─ TCP, token-authenticated ──► on-device agent      works the moment a restored snapshot resumes
 ├─ adb server smart-socket protocol (127.0.0.1:5037)     fallback transport; agent start / install
 │    └─ "localabstract:agemu-agent" ──► on-device agent
 └─ on-device agent (android-agent/, Java, app_process as the shell user)
      one long-lived UiAutomation connection: UI tree, launch/stop, display profiles,
      set-text typing, key and text injection
```

## Run it

```sh
make android-build                                              # bin/android/{agemu,agemud,agemu-bench}
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
| `AGEMU_ANDROID_STREAM` | `1` | `0`: no frame stream; every screenshot reads the framebuffer (`getScreenshot`). |
| `AGEMU_ANDROID_AGENT_TCP` | `1` | `0`: reach the agent through adb only (no console port redirection). |
| `AGEMU_ANDROID_BOOT_LOAD` | `inplace` | `startup`: restore the baseline with `-snapshot` at emulator start. This corrupts guest memory on emulator 37.1 (see Guest restarts). |
| `AGEMU_ANDROID_EMULATOR_FLAGS` | (none) | Extra emulator flags, e.g. `-show-kernel` to log the guest kernel console. Never pass `-feature`: the emulator deletes snapshots whose feature set differs. |
| `AGEMU_ANDROID_TRACE` | `0` | `1`: log whether each screenshot came from the frame stream. |

## How each operation works

| Operation | Standard tool | agemu |
|---|---|---|
| boot | cold boot | `shutdown` parks the device in **standby**: the baseline snapshot is restored in place and the VM is paused. `boot` resumes it. When the device is powered off, `boot` starts the emulator with `-no-snapshot-load` and restores the baseline **in place** (gRPC `LoadSnapshot`) as soon as gRPC answers, then waits for the restored agent over TCP. |
| reset | cold boot | gRPC `LoadSnapshot(agemu-baseline)` in the running emulator. The agent restored with the snapshot answers over TCP 30–110 ms later; agemu does not wait for adb. The active screen profile is re-applied. |
| screenshot | `adb exec-out screencap -p` | The latest frame from a long-lived `streamScreenshot` subscription (MMAP transport), already in host memory: crop, resample, encode. Falls back to `getScreenshot` while the screen keeps changing. See Screenshots. |
| tap / swipe | `adb shell input tap/swipe` | gRPC `sendTouch` on the emulator's virtio multitouch screen, the same device a finger would use. Returns once the touch is queued. |
| type | `adb shell input text` | The agent inserts the text at the cursor of the focused editable node with `AccessibilityNodeInfo.ACTION_SET_TEXT` (what UiAutomator and Appium `setText` do). Returns once the app's UI thread has applied it, so its TextWatchers and listeners have run. `\n` / `\t` become Enter / Tab keys. `type --keys` (MCP `keys: true`) sends one key event per character through the IME, like `input text`. With no focused editable node, `type` also falls back to keys. |
| key | `adb shell input keyevent` | The agent injects the key through `IInputManager.injectInputEvent` (the call `input` makes) in async mode: it returns once InputManager has queued the down and up events, like a tap. Follow with `wait` or `ui` to see the effect. |
| UI tree | `uiautomator dump` + reading the XML | The agent walks `AccessibilityNodeInfo` of every on-screen window, top-most first, over its warm UiAutomation connection, and returns compact JSON. The Go side maps it to `api.Element`. After a `launch`, the first tree read waits (up to 3 s) until the accessibility layer reports the launched app's window. |
| launch | `am start -W` | The agent calls `IActivityTaskManager.startActivityAndWait`, the call `am start -W` makes, and returns when it does (the activity has drawn). The wait for the app's accessibility window moved to the next tree read. |

### Screenshots

The daemon keeps one `streamScreenshot` subscription per emulator, with the MMAP transport
into a file mapping next to the device log (`<avd>.fb.stream`). For every frame the guest
posts, the emulator writes the pixels into the mapping and sends a small message with a
sequence number and the frame's generation time. That readback costs the emulator a few
milliseconds; a `getScreenshot` call costs 25–40 ms on this host.

- **Consistent copies.** The emulator may overwrite the mapping at any time. A frame is
  copied once the display has been quiet for 20 ms, then compared with the mapping again; if
  any byte changed, the emulator was writing and the copy is dropped. A copy is current until
  the next frame notification.
- **Freshness.** A screenshot returns the current copy. After agemu sends input (tap, swipe,
  key, type, launch, profile, reset), it waits until the guest has posted a frame generated
  after that input, or 300 ms without one (the input changed nothing on screen), and until
  that frame has settled (at most 1.5 s after the input). While the screen keeps changing
  with no input of ours (an animation), it reads the framebuffer directly instead.
- **Snapshot loads** (reset, standby, resume) drop the copy: the display is restored without
  a new frame being posted.
- **Emulator quirk.** With width and height 0 (unscaled) in the stream request, emulator
  37.1 acknowledges every frame but never writes pixels into the mapping. agemu passes the
  framebuffer size.
- **Proof** (`TestDeviceScreenshotFresh`, `devicetest` tag). It drives Settings back and forth
  (tap a row or press back, then wait for the new screen in the UI tree) and compares the
  streamed frame with a direct framebuffer read taken right after it. A difference counts as
  stale only if no frame was posted in between and the screen was static. 30 rounds: 28
  byte-identical, 2 inconclusive (a frame was posted between the two reads), 0 stale. A
  streamed frame of a static screen was byte-identical to `getScreenshot` (0 of 10,471,680
  bytes differ). Right after `reset`, the screenshot shows the restored home screen.

### Agent transport

The agent listens on its abstract unix socket (reached through the adb server) and, when
started with a token, on guest TCP port 39271. agemu adds an emulator console redirection
`redir add tcp:<free host port>:39271` (bound to 127.0.0.1) and connects to it. The first
line on a TCP connection must be `{"auth":"<token>"}`. The token is per AVD
(`$AGEMU_HOME/android/<avd>.agent-token`, mode 600) and reaches the agent through its
environment, which other guest apps cannot read.

Unlike adb, which re-handshakes with the guest after every snapshot load (0.7–4 s here),
the redirection lives in the emulator process and the agent's listening socket is part of
the snapshot. Two emulator behaviours shape this:

- **Snapshot loads drop the redirection's host listener**, while `redir list` still shows it.
  A refused connection makes agemu delete and re-add it (a few ms).
- **Snapshots include the user-mode network (slirp) state.** A snapshot saved while the
  redirection exists cannot be loaded again (`invalid sbuf offsets ... Failed to load
  slirp-sbuf ... error while loading state for instance 0x0 of device 'slirp'`, -EINVAL).
  agemu therefore deletes all redirections and closes its agent connection before it saves
  the baseline.

Any TCP failure falls back to adb.

### Device lifecycle

- **`create`** runs `avdmanager create avd` on the system image that is already installed, and never downloads one. It then patches `config.ini`:
  - display = the profile (default `pixel-9`, 1080x2424@420), `hw.ramSize=2048`;
  - phone hardware: `hw.keyboard=no`, `hw.mainKeys=no` (gesture navigation), no dpad, trackball or SD card;
  - quickboot auto-snapshots off.
- **`boot`** runs `emulator -avd <name> -port 5580 -grpc 8580 -no-window -no-audio -no-boot-anim -gpu auto -memory 2048 -lowram -no-snapshot-save -no-snapshot-load`.
  - With a baseline, agemu restores it in place (gRPC `LoadSnapshot`) as soon as gRPC answers, before the cold boot gets anywhere. Restoring it at startup (`-snapshot`) corrupts guest memory on this emulator; see Guest restarts.
  - The process is detached from the daemon.
  - Ports are taken from 5580/8580 upward and skip ports used by any running emulator.
- **Agent-ready** means `sys.boot_completed=1` **and** the on-device agent answers.
- **Baseline.** On the first boot agemu waits 20 s for the device to settle, then:
  - turns stylus handwriting off (see Caveats) and warms up the keyboard;
  - clears recent tasks and goes home;
  - deletes its console redirections and closes its agent connection (see Agent transport);
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
- **agemu CLI:** one `agemu` process per call (`agemu --help` alone takes 7.0 ms here, `/usr/bin/true` 1.9 ms; the binary is 17 MB).
- **daemon/MCP:** the same call over a persistent daemon connection (what `agemu mcp` uses).

Boot and reset use N=3. The cold-boot baseline uses the same emulator flags as agemu, minus
the snapshot. Medians; p95 and n in parentheses.

### Results (2026-11-06, default configuration: `-gpu auto`, software rendering)

The table combines four runs of the same binaries on the same host (Apple M2 Pro, macOS
26.6), pasted from `agemu-bench` output:

- **Rows screenshot … key home, launch (cold), profile switch, boot:** full run, 01:18–01:47, load average 3–92, swap 11–12 of 12 GB used.
  - Its warm-launch baseline hung (`am start -W` did not return for 18 minutes and was killed). Its reset rows stopped on the emulator deadlock described below.
- **Rows reset … snapshot load:** re-run 02:02–02:04, load 8–25, after the fix for that deadlock.
- **Row launch (warm start):** re-run 02:04 (renamed: `--only` splits on commas).
- **Type rows:** re-run 02:11–02:15, load 4–36, after the last set-text change, typing into an empty form. The host was nearly idle, so the baseline was 65–73 ms there, against 130–211 ms in the full run. The full run's type rows (older agent, a form left with 635 characters) were: plain field 165 → 68 ms (2.4×), `--keys` 130 → 51 ms (2.6×), Settings search 211 → 55 ms (3.9×).

| Operation | Baseline tool | Baseline | agemu CLI | agemu daemon/MCP | Speedup (CLI) | Speedup (daemon) |
|---|---|---|---|---|---|---|
| screenshot | `adb exec-out screencap -p > f.png` | 149.7 ms (p95 869.9, n=20) | 12.0 ms (p95 23.4, n=20) | 4.1 ms (p95 4.8, n=20) | **12.5×** | **36.5×** |
| screenshot (small-phone profile) | `adb exec-out screencap -p > f.png` | 115.2 ms (p95 257.2, n=20) | 11.9 ms (p95 14.6, n=20) | 4.8 ms (p95 5.6, n=20) | **9.7×** | **24.0×** |
| screenshot (after a screen change) | `adb exec-out screencap -p > f.png` | 178.9 ms (p95 674.9, n=20) | 27.9 ms (p95 104.7, n=20) | 9.7 ms (p95 104.1, n=20) | **6.4×** | **18.4×** |
| ui tree | `adb shell uiautomator dump + adb exec-out cat` | 2082.4 ms (p95 4527.1, n=20) | 9.7 ms (p95 12.6, n=20) | 2.0 ms (p95 5.2, n=20) | **214.7×** | **1041.2×** |
| tap | `adb shell input tap 800 1100` | 49.3 ms (p95 122.2, n=20) | 8.3 ms (p95 16.9, n=20) | 0.4 ms (p95 0.5, n=20) | **5.9×** | **123.2×** |
| swipe (300 ms gesture) | `adb shell input swipe 540 1800 540 1000 300` | 384.2 ms (p95 614.8, n=20) | 325.7 ms (p95 386.6, n=20) | 309.4 ms (p95 371.6, n=20) | **1.2×** | **1.2×** |
| swipe (50 ms gesture) | `adb shell input swipe 540 1800 540 1000 50` | 136.2 ms (p95 579.6, n=20) | 107.9 ms (p95 225.8, n=20) | 67.7 ms (p95 138.9, n=20) | **1.3×** | **2.0×** |
| type text (plain field) | `adb shell input text hello` | 64.7 ms (p95 225.9, n=20) | 51.5 ms (p95 76.8, n=20) | 48.3 ms (p95 73.4, n=20) | **1.3×** | **1.3×** |
| type text --keys (plain field) | `adb shell input text hello` | 73.3 ms (p95 130.7, n=20) | 55.2 ms (p95 351.0, n=20) | 22.6 ms (p95 91.1, n=20) | **1.3×** | **3.2×** |
| type text (Settings search) | `adb shell input text hello` | 71.9 ms (p95 1042.5, n=20) | 30.0 ms (p95 54.5, n=20) | 23.5 ms (p95 44.3, n=20) | **2.4×** | **3.1×** |
| key home | `adb shell input keyevent KEYCODE_HOME` | 141.7 ms (p95 855.1, n=20) | 13.6 ms (p95 83.9, n=20) | 1.9 ms (p95 12.5, n=20) | **10.4×** | **74.6×** |
| launch app (cold start) | `adb shell am start -W -n com.android.settings/.Settings` | 822.5 ms (p95 8916.3, n=10) | 906.9 ms (p95 3185.7, n=10) | 774.1 ms (p95 1184.7, n=10) | **0.9×** | **1.1×** |
| profile switch | `adb shell wm size WxH && adb shell wm density D` | 552.6 ms (p95 1706.4, n=10) | 1536.5 ms (p95 1775.2, n=10) | 1282.2 ms (p95 1553.2, n=10) | **0.4×** | **0.4×** |
| boot to agent-ready (from standby) | `emulator cold boot (same flags, -no-snapshot-load) until sys.boot_completed=1` | 10262.7 ms (p95 11760.4, n=3) | 56.2 ms (p95 271.2, n=3) | 42.7 ms (p95 43.4, n=3) | **182.6×** | **240.3×** |
| boot to agent-ready (powered off) | `emulator cold boot (same flags, -no-snapshot-load) until sys.boot_completed=1` | 9837.1 ms (p95 10932.7, n=3) | 2631.6 ms (p95 2637.0, n=3) | 2407.7 ms (p95 2487.2, n=3) | **3.7×** | **4.1×** |
| reset to clean state | `adb emu kill + emulator cold boot until sys.boot_completed=1` | 11760.8 ms (p95 11819.1, n=3) | 2361.0 ms (p95 2364.2, n=3) | 2237.1 ms (p95 2402.5, n=3) | **5.0×** | **5.3×** |
| reset vs emulator snapshot load | `adb emu avd snapshot load agemu-baseline until sys.boot_completed=1` | 2261.4 ms (p95 2320.0, n=3) | 2044.4 ms (p95 2091.6, n=3) | 2082.3 ms (p95 2378.9, n=3) | **1.1×** | **1.1×** |
| launch app (warm start) | `adb shell am start -W -n com.android.settings/.Settings` | 709.4 ms (p95 1566.5, n=10) | 585.3 ms (p95 745.3, n=10) | 610.0 ms (p95 781.2, n=10) | **1.2×** | **1.2×** |

- swipe (300 ms gesture): both tools spend 300 ms on the gesture itself. Overhead on top of it: CLI 3.3×, daemon 9.0× faster.
- swipe (50 ms gesture): both tools spend 50 ms on the gesture itself. Overhead on top of it: CLI 1.5×, daemon 4.9× faster.

### Before → after

"Before" is the table this document had at `db290b0` (2026-11-05 22:30–23:06, load 30–315).
Speedups compare against each run's own baseline, and baselines moved with host load: the
cold boot took 33.2 s then and 9.8 s here, so boot and reset ratios fell while agemu itself
got 2.5–2.7× faster.

| Operation | Speedup daemon/MCP: before → after | daemon median: before → after | Speedup CLI: before → after | CLI median: before → after |
|---|---|---|---|---|
| screenshot | 6.7× → **36.5×** | 31.9 ms → 4.1 ms | 4.8× → **12.5×** | 44.8 ms → 12.0 ms |
| screenshot (small-phone profile) | 2.5× → **24.0×** | 111 ms → 4.8 ms | 3.0× → **9.7×** | 92.0 ms → 11.9 ms |
| screenshot (after a screen change) (new row) | **18.4×** | 9.7 ms | **6.4×** | 27.9 ms |
| ui tree | 730.8× → **1041.2×** | 2.8 ms → 2.0 ms | 184.3× → **214.7×** | 11.1 ms → 9.7 ms |
| tap | 63.5× → **123.2×** | 1.0 ms → 0.4 ms | 4.7× → **5.9×** | 13.4 ms → 8.3 ms |
| swipe (300 ms gesture) | 2.0× → **1.2×** | 307 ms → 309 ms | 1.8× → **1.2×** | 339 ms → 326 ms |
| swipe (50 ms gesture) | 4.5× → **2.0×** | 55.5 ms → 67.7 ms | 3.2× → **1.3×** | 78.2 ms → 108 ms |
| type text (plain field) | 0.6× → **1.3×** | 324 ms → 48.3 ms | 0.7× → **1.3×** | 314 ms → 51.5 ms |
| type text --keys (plain field) (new row) | **3.2×** | 22.6 ms | **1.3×** | 55.2 ms |
| type text (Settings search) | 2.6× → **3.1×** | 214 ms → 23.5 ms | 2.1× → **2.4×** | 261 ms → 30.0 ms |
| key home | 9.0× → **74.6×** | 17.0 ms → 1.9 ms | 5.0× → **10.4×** | 30.4 ms → 13.6 ms |
| launch app (cold start) | 1.0× → **1.1×** | 3038 ms → 774 ms | 1.0× → **0.9×** | 3205 ms → 907 ms |
| profile switch | 0.5× → **0.4×** | 2070 ms → 1282 ms | 0.3× → **0.4×** | 2922 ms → 1536 ms |
| boot to agent-ready (from standby) | 372.3× → **240.3×** | 53.0 ms → 42.7 ms | 221.7× → **182.6×** | 89.0 ms → 56.2 ms |
| boot to agent-ready (powered off) | 5.6× → **4.1×** | 5924 ms → 2408 ms | 4.4× → **3.7×** | 7572 ms → 2632 ms |
| reset to clean state | 6.9× → **5.3×** | 6002 ms → 2237 ms | 6.0× → **5.0×** | 6934 ms → 2361 ms |
| reset vs emulator snapshot load | 0.7× → **1.1×** | 9683 ms → 2082 ms | 0.8× → **1.1×** | 7900 ms → 2044 ms |
| launch app (warm start) | 0.6× → **1.2×** | 942 ms → 610 ms | 1.0× → **1.2×** | 584 ms → 585 ms |

### Reset and boot breakdown

From the daemon log (`reset:` and `boot:` lines) during the runs above:

| Phase | Before (6–7 s reset, 5.9 s boot) | After |
|---|---|---|
| Reset: display info (one agent call) | not logged | 2–32 ms |
| Reset: emulator `LoadSnapshot` (2 GB RAM, devices, disk) | 1.7–5 s (emulator's own log); 5.3–12 s measured at load 300–450 | 1.87–2.34 s (12 resets, load 8–25) |
| Reset: wait for adb to re-handshake, then the agent | 0.7–4.2 s, then 0.05–0.4 s | not needed: the restored agent answers over TCP 10–62 ms after the load |
| Reset: re-apply a non-native profile | 1–3 s when one is active | same (unchanged) |
| Boot (powered off): emulator process until gRPC answers | 1.1–1.8 s | 0.72–0.86 s (6 boots; host lighter) |
| Boot (powered off): how the baseline is restored | at startup (`-snapshot`), 1.3–5.2 s by the emulator's log; corrupts guest memory (see Guest restarts) | in place, as soon as gRPC answers |
| Boot (powered off): restore + until the agent answers | adb `sys.boot_completed`, then the agent over adb: 1.5–3.1 s after gRPC (00:18–00:51) | restore 1.54–1.70 s, then the agent 50–70 ms later (TCP) |

What remains is the VM restore itself: the emulator copies the 2 GB `ram.bin` into guest
RAM and restores devices and the disk. `adb emu avd snapshot load` does the same work, and
agemu's reset is now at parity with it (2.08 vs 2.26 s), without waiting for adb.

Explored and rejected:

- **On-demand RAM loading** (`-feature OnDemandSnapshotLoad`, off by default with the comment "issues with MemoryWatch"). The emulator first deleted the baseline because its feature set differed ("The emulator has the feature: 20, which is missing in the snapshot"). Then, with a baseline saved under the feature, the next in-place load hung for 9 minutes with no guest kernel output; the emulator had to be killed.
- **File-backed quickboot RAM** (`QuickbootFileBacked`, which maps `snapshots/default_boot/ram.img`). It needs a `default_boot` snapshot saved on exit. Headless, the emulator refused to save one ("you have configured it not to save on exit" / "Snapshots have been disabled by the user, save request is ignored"), also with a per-AVD `quickbootChoice.ini` (`saveOnExit = true`). The remaining switch is the emulator's global settings, shared with the other session's `Pixel_9_Pro`, so it was not changed. Even working, it maps the RAM file shared, so every boot would need a fresh APFS clone of a pristine copy, and in-place resets would not benefit. Time-boxed and stopped.
- **Agent dialing out to 10.0.2.2.** A guest-initiated connection would be dead after every restore and need reconnect logic. The console redirection does the same job with a listening socket that the snapshot preserves.

### Against the 10x target

**Meets 10x** (daemon/MCP, and CLI where marked):

- **Screenshot:** 36.5× daemon, 12.5× CLI. Small-phone profile: 24.0× daemon, 9.7× CLI. After a screen change: 18.4× daemon, 6.4× CLI. The frame stream moved the 25–40 ms framebuffer readback off the request; what is left is crop/resample (precomputed taps, integer sums: 30 → 9 ms for 1080x2400 → 720x1600 under load) and PNG encoding (5–15 ms for 1080x2424 depending on load).
- **UI tree:** 1041× daemon, 215× CLI.
- **Tap:** 123× daemon.
- **Key:** 74.6× daemon, 10.4× CLI. agemu returns once InputManager has queued the key; `input keyevent` waits until the app handled it.
- **Boot from standby:** 240× daemon, 183× CLI.

**Misses 10x, and why:**

- **CLI path for tap, small-phone and after-change screenshots:** 5.9–9.7×. Each call starts a 17 MB Go process (7 ms idle, 10–25 ms under load), and Android 17's `input tap` costs only 50 ms. The planned thin CLI client removes most of this; MCP and other persistent clients don't pay it.
- **Type text:** 1.3–3.2× daemon. The set-text itself takes 2–9 ms, but it runs on the app's UI thread, and so does reading the field. agemu waits for both: so that `type` then `ui` is consistent, and so that the app's listeners have run.
  - With the soft keyboard up, every text change sends Gboard's InputConnection traffic and a software-rendered redraw through that thread. That is 20–50 ms per change in the Contacts editor here (agent phase timings: `focusMs`, `setMs`, `selMs`).
  - Inserting in the middle of existing text costs one more round trip (`ACTION_SET_SELECTION`, 15–42 ms), because `SET_TEXT` moves the cursor to the end.
  - `input text` returns once its key events are dispatched, before that follow-up work, and on an idle host it costs only 65–73 ms.
  - Returning before the app has applied the text would be ~5 ms, but then `type` followed by `ui` would race.
- **Swipe:** the gesture time is inherent. The overhead on top of it is 9.0× (300 ms) and 4.9× (50 ms) faster on the daemon. The rest is timer slack between the 8 ms touch-move steps on a loaded host, plus one unary `sendTouch` round trip per step.
- **Launch:** the app's own start dominates. agemu now returns where `am start -W` does (1.1–1.2×; warm launch was 0.6× before).
- **Boot from powered off:** 4.1× against a 9.8 s cold boot (5.6× before against a 33 s one). Absolute: 5.9 → 2.4 s.
  - Emulator process start (0.7–1.4 s) plus the 2 GB RAM restore (1.5–2.3 s) are the floor.
  - Lazy and file-backed RAM loading did not work here (see above).
- **Reset:** 5.3× against a cold boot and 1.1× against `adb emu avd snapshot load`: it is the VM restore (1.9–2.3 s). The adb wait is gone.
  - Back-to-back resets wait for the emulator's texture loader (see Emulator deadlock), so an immediate second reset takes ~5 s.
- **Profile switch:** below 1x by design: agemu returns after the app has re-laid out and animations have finished; `wm size` returns before.

### Guest restarts: cause found

The guest restarted repeatedly in the previous round and in this one. The boot reason was
always `reboot`, and `bootstat` found no `persist.sys.boot.reason`, i.e. not a userspace
reboot. Logcat captured continuously from the host stopped mid-stream each time, with no
warning.

Timeline this round: 23:24, 00:03, 00:52 and 01:01. Each came 5–60 s after a **new emulator
process restored a snapshot at startup** (`-snapshot agemu-baseline`), or after a baseline
saved from such a guest.

To see the guest kernel's last words, a baseline was taken from a cold boot with
`-show-kernel` (the console setting lives in the snapshot's kernel memory). Then three
power-off boots from it:

- **All three rebooted** 5–8 s after the restore.
- The kernel console shows corrupted page tables right after the restore:

```
BUG: Bad page map in process HeapTaskDaemon  pte:fdefeec0fdef7b3c pmd:800000042334003
...
Unable to handle kernel paging request at virtual address 00077df1f7714080
Internal error: Oops: 0000000096000004 [#1] PREEMPT SMP
pc : lookup_swap_cgroup_id+0x30/0x50
Kernel panic - not syncing: Oops: Fatal exception
```

In-place loads (gRPC `LoadSnapshot`, as reset does) in the same setup: 5 of 5 without
any `BUG`/`Oops` line or reboot. So emulator 37.1 (HVF, 16 KB-page image) corrupts guest
RAM when it restores a snapshot during process startup.

**Fix:** a powered-off boot now starts the emulator with `-no-snapshot-load` and restores the
baseline in place as soon as gRPC answers (`AGEMU_ANDROID_BOOT_LOAD`). Results:

- 3 of 3 boots with the kernel console on: no errors, no reboot.
- No restart in the benchmark and e2e runs after the change (guest uptime checked).

A baseline saved from a startup-restored guest may itself hold corrupt memory, so it was
rebuilt from a cold boot.

Ruled out on the way:

- A 30 s VM pause followed by resume: the guest clock jumps by the pause, but there was no reboot in 60 s.
- Three rounds of "save a snapshot, then 60 s of rendering", with the kernel console on: no reboot.
- Host memory pressure, the earlier prime suspect: not needed to explain it.

### Emulator deadlock on back-to-back loads

During the full run, a reset hung for 10 minutes; the emulator console stopped answering
too. `sample` of the emulator process:

- The `LoadSnapshot` handler sits in `android::snapshot::Loader::~Loader()` → `Thread::wait` → `pthread_join`.
- gfxstream's `GLBackgroundLoader::main()` → `restoreTexture` → `TextureLoader::loadTexture` → `ferror` → `flockfile` is blocked.

After a load, the emulator restores GPU textures in the background. A load that starts
before that finishes deadlocks. It hit a reset started 0.1 s after the previous one; resets
2.2 s apart did not hang.

agemu now keeps in-place loads at least 3 s apart (it logs the wait) and gives up on a load
after 2 minutes with an error. Twelve spaced resets and three back-to-back ones (each waited
~2.8 s) did not hang. The bench's reset rows pause 3 s between iterations (untimed, both
tools) to measure the reset rather than the spacing.

### Earlier supplementary run: `AGEMU_ANDROID_GPU=host` (previous version, 2026-11-05)

These are the cheap operations the previous version measured (22:11–22:16, load 40–230, from a fresh reset) with the host GPU (gfxstream). They are faster, mostly because
screenshots read back from the host GPU in about 9 ms. With the host GPU this emulator was
not stable on this machine: gfxstream `error null ctx` / `Failed to create EGL fence sync`, a
guest that stopped answering adb, and repeated guest restarts. So the default is `-gpu auto`. The guest restarts of that time are now explained by the startup snapshot restore (see Guest restarts), not by the GPU; the gfxstream errors are not. The host GPU was not re-tested in this round, so `-gpu auto` stays the default. The
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
- **Keys are injected, like `adb shell input`.** A phone AVD (`hw.keyboard=no`, so the soft keyboard behaves as on a real phone) has no keyboard device, so the emulator's gRPC `sendKey` has nothing to drive. Touches do go through the emulator's virtual touchscreen.
- **`type` sets the field's text by default.** The app sees a text change (TextWatchers, listeners, IME selection updates), not key presses. Apps that react to individual key events (`onKeyDown`, custom key handling, games) need `type --keys`. A non-empty password field exposes masked text, so `type` falls back to keys there. Text is inserted at the cursor read fresh from the app (the accessibility cache can lag a change behind; reading it stale inserted at old positions).
- **One UiAutomation client at a time.** Android allows only one UiAutomation connection per device, and the agemu agent holds it.
  - While the agent runs, `uiautomator dump` and UiAutomation-based instrumentation tests (UiDevice, Espresso with UiAutomator) cannot connect.
  - Stop the agent with `adb -s <serial> shell 'kill $(pidof agemu-agent)'`. agemu restarts it on its next call.
- **Stylus handwriting is off in the baseline.** The emulator's virtio touchscreen advertises a stylus (`BTN_STYLUS`, source `TOUCHSCREEN | STYLUS`). Gboard therefore treats taps on text fields as stylus input and shows its floating handwriting toolbar over the app instead of the keyboard (seen in this session's screenshots), or a one-time "Try out your stylus" sheet. agemu runs `settings put secure stylus_handwriting_enabled 0` once before it saves the baseline, a user setting: with it, Gboard shows the normal QWERTY keyboard, as on a phone used with a finger.
- **Tap, swipe and key return when the input is queued.** `agemu tap` returns once the emulator has queued the touch; `agemu key` once InputManager has queued the key. `adb shell input tap/keyevent` waits until the app has handled the event. Agents should follow them with `wait` or `ui` (milliseconds) or `screenshot` (which waits for the input's frame). `type` returns after the app's UI thread has applied the text; `launch` after the activity has drawn, like `am start -W`.
- **Back-to-back text changes are bound by the app.** `ACTION_SET_TEXT` and the cursor read run on the app's UI thread. Idle, a `type` takes 5–10 ms in the agent; issued while the app is still redrawing the previous change (software rendering on a loaded host), it waits 16–130 ms for the UI thread.
- **Standby keeps guest RAM allocated.** A device in standby uses no CPU, but the paused emulator keeps its guest RAM (about 2 GB) on the host. Run `shutdown` twice, or set `AGEMU_ANDROID_STANDBY=0`, to free it.
- **Disk.** One baseline snapshot is about 2 GB (`ram.bin` is the full guest RAM). The AVD's userdata qcow2 grows to about 3 GB. `ANDROID_SNAPSHOT_COMPRESS=1` (via `AGEMU_ANDROID_SNAPSHOT_COMPRESS=1`) cuts the snapshot to about 40 % at a similar load time.
- **Rotation is not handled.** Profiles are portrait; a landscape app or device rotation is not mapped.
- **Emulator features are part of a snapshot.** Starting the emulator with a `-feature` set that differs from the snapshot's makes it print "different emulator features" and **delete** the snapshot, then cold boot. Do not pass `-feature` flags through `AGEMU_ANDROID_EMULATOR_FLAGS`.
- **The measurements ran on a shared, often saturated host.** The M2 Pro with 16 GB shared the machine with another emulator, iOS simulators, a browser and other sessions; load averages ranged from 3 to 450 and swap was nearly full during this round. Absolute numbers are noisy (see the p95 columns and the run times listed with the table); both sides of each row ran back to back under the same conditions.
