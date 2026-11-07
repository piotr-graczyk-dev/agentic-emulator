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
| `AGEMU_ANDROID_GPU` | `host` | Emulator `-gpu` mode. `auto` picks software rendering (SwiftShader) without a window; see "Host GPU and lean devices". |
| `AGEMU_ANDROID_LEAN` | `1` | `0`: leave on the Google apps a lean baseline turns off (applies when a baseline is taken). |
| `AGEMU_ANDROID_ANIMATIONS` | `1` | `0`: set the window, transition and animator scales to 0 after every boot and reset (screen changes finish at once). |
| `AGEMU_ANDROID_RAM_MB` | `2048` | Guest RAM for new AVDs. Below 4096 the emulator needs `-lowram`. |
| `AGEMU_ANDROID_STANDBY` | `1` | `0`: `shutdown` always powers off. |
| `AGEMU_ANDROID_CONSOLE_PORT` / `AGEMU_ANDROID_GRPC_PORT` | `5580` / `8580` | First ports to try. |
| `AGEMU_ANDROID_SHM` | `1` | `0`: frames travel in the gRPC reply instead of a shared mmap. |
| `AGEMU_ANDROID_BASELINE_SETTLE_S` | `20` | Settle time before the baseline snapshot. |
| `AGEMU_ANDROID_STREAM` | `1` | `0`: no frame stream; every screenshot reads the framebuffer (`getScreenshot`). |
| `AGEMU_ANDROID_STREAM_IDLE_S` | `30` | Close the frame stream after this many seconds without a call to the device, if the screen is still posting frames then (at least one per 3 s over the last 10 s); the next call reopens it. `0`: keep it open. See Screenshots. |
| `AGEMU_ANDROID_AGENT_TCP` | `1` | `0`: reach the agent through adb only (no console port redirection). |
| `AGEMU_ANDROID_SNAPSHOT_COMPRESS` | (unset) | `1`: the emulator saves the baseline's RAM LZ4-compressed (`ram.bin` about 41% of the raw size). `0`: always raw. Unset leaves the choice to the emulator, which can turn compression on by itself when the host has little free RAM. Applies when a baseline is taken; both formats load. See Caveats (Disk). |
| `AGEMU_ANDROID_BOOT_LOAD` | `inplace` | `startup`: restore the baseline with `-snapshot` at emulator start. This corrupts guest memory on emulator 37.1 (see Guest restarts). |
| `AGEMU_ANDROID_EMULATOR_FLAGS` | (none) | Extra emulator flags, e.g. `-show-kernel` to log the guest kernel console. Never pass `-feature`: the emulator deletes snapshots whose feature set differs. |
| `AGEMU_ANDROID_TRACE` | `0` | `1`: log whether each screenshot came from the frame stream. |

## How each operation works

| Operation | Standard tool | agemu |
|---|---|---|
| boot | cold boot | `shutdown` parks the device in **standby**: the baseline snapshot is restored in place and the VM is paused. `boot` resumes it. When the device is powered off, `boot` starts the emulator with `-no-snapshot-load` and restores the baseline **in place** (gRPC `LoadSnapshot`) as soon as gRPC answers, then waits for the restored agent over TCP. |
| reset | cold boot | gRPC `LoadSnapshot(agemu-baseline)` in the running emulator. The agent restored with the snapshot answers over TCP 30–110 ms later; agemu does not wait for adb. The active screen profile is re-applied. |
| screenshot | `adb exec-out screencap -p` | The latest frame from a `streamScreenshot` subscription (MMAP transport), already in host memory: crop, resample, encode. Falls back to `getScreenshot` while the screen keeps changing. The subscription is closed after 30 s without calls if the screen is still posting frames then, and reopened by the next call. See Screenshots. |
| tap / swipe | `adb shell input tap/swipe` | gRPC `sendTouch` on the emulator's virtio multitouch screen, the same device a finger would use. Returns once the touch is queued. |
| type | `adb shell input text` | The agent inserts the text at the cursor of the focused editable node with `AccessibilityNodeInfo.ACTION_SET_TEXT` (what UiAutomator and Appium `setText` do). Returns once the app's UI thread has applied it, so its TextWatchers and listeners have run. `\n` / `\t` become Enter / Tab keys. `type --keys` (MCP `keys: true`) sends one key event per character through the IME, like `input text`. With no focused editable node, `type` also falls back to keys. |
| key | `adb shell input keyevent` | The agent injects the key through `IInputManager.injectInputEvent` (the call `input` makes) in async mode: it returns once InputManager has queued the down and up events, like a tap. Follow with `wait` or `ui` to see the effect. |
| UI tree | `uiautomator dump` + reading the XML | The agent walks `AccessibilityNodeInfo` of every on-screen window, top-most first, over its warm UiAutomation connection, and returns compact JSON. The Go side maps it to `api.Element`. After a `launch`, the first tree read waits (up to 3 s) until the accessibility layer reports the launched app's window. |
| launch | `am start -W` | The agent calls `IActivityTaskManager.startActivityAndWait`, the call `am start -W` makes, and returns when it does (the activity has drawn). The wait for the app's accessibility window moved to the next tree read. A launch less than 300 ms after a `key` waits until then (see Caveats), and the wait is bounded at 20 s. |
| open, permission, clipboard, appearance, apps, uninstall, clear-data | `am start -d`, `pm grant/revoke`, `cmd uimode`, `pm list/uninstall/clear` | The agent makes the system service call behind the standard command, as the shell. See Test capabilities. |
| location | `adb emu geo fix` | Emulator gRPC `SetGps`, the same GNSS fix `geo fix` sets. |

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
- **Idle stop.** The stream costs nothing while the screen is static, but every frame the
  guest posts costs a readback in the emulator (gfxstream's post worker) and a copy and
  compare in agemud, whether or not anyone takes a screenshot. An app that keeps animating (a
  spinner, a running stopwatch) therefore keeps the host busy. agemu closes the subscription
  when there was no call to the device for `AGEMU_ANDROID_STREAM_IDLE_S` (30 s) and the guest
  is still posting frames: at least one per 3 s over the last 10 s (or the last half of the
  idle time, when that is shorter). The frames of a transition right after the last call do not
  count, so a screen that has been static since keeps its subscription and its copy; it is
  checked again every 10 s. Every call reopens a closed subscription before it sends input.
  - **A new subscription's first message** (emulator 37.1, measured with
    `TestDeviceStreamReopenGap`). The emulator listens for frames from the call on, and
    captures the first frame as soon as the guest posts one (that message has seq 1), or after
    waiting about 125 ms for one (seq 0). That is why a first message takes 130-150 ms on a
    static screen (median 139-142 ms; 0.2-0.9 s at load 70), and less when frames are posted.
    The 125 ms matches the frame wait (`kTimeToWaitForFrame`) in the open-source
    `EmulatorService.cpp`; 37.1's own source was not checked. A frame posted after the call is
    in the first message's pixels or notified after it: in 150 new subscriptions (a static
    screen, a blinking cursor, a running stopwatch), 68 with a frame posted before their first
    message, the first frame never missed one. 9 first frames differed from the screen read
    right after them, and the newer frame was notified each time.
  - A seq-0 first frame counts as generated when the subscription was opened, before the input
    that reopened it, so it never passes for that input's effect. A seq-1 first frame is a
    posted frame and counts like any other.
  - The first screenshot without an input after a stop reads the framebuffer, which is current;
    the reopened subscription serves the next ones.
  - A screenshot right after an input that reopened the stream waits for the first message (at
    most 1 s), and then for the input's frame as usual.
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
- **`boot`** runs `emulator -avd <name> -port 5580 -grpc 8580 -no-window -no-audio -no-boot-anim -gpu host -memory 2048 -lowram -no-snapshot-save -no-snapshot-load`.
  - A baseline taken under another `-gpu` mode does not load ("different renderer configured"). agemu then lets the guest finish its cold boot and retakes the baseline (about 45 s once).
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
  - Re-tested on emulator 37.1.11 (2026-11-06, an APFS clone of the AVD, baseline saved under the feature): a reset and a powered-off boot both hung until agemu's 2-minute limit.
  - Cause, from `sample` of the hung emulator: on macOS the feature write-protects guest RAM and fills pages from a Mach exception handler. The background loader (`RamLoader::backgroundPageLoad` -> `MemoryAccessWatch::fillPage`) faults in its own `memmove` into a page it is filling. The exception handler (`MacDoAccessCallback` -> `RamLoader::fillPageData`) then spins in `cthread_yield` waiting for that same page. Neither thread can proceed, and the vCPUs wait on them.
  - It cannot be fixed from outside the emulator. A working lazy load could matter: under host memory pressure, macOS had compressed or swapped out 1.7-2.0 GB of the 2 GB guest RAM of a running device, so most of it had not been touched recently.
- **File-backed quickboot RAM** (`QuickbootFileBacked`, which maps `snapshots/default_boot/ram.img`). It needs a `default_boot` snapshot saved on exit. Headless, the emulator refused to save one ("you have configured it not to save on exit" / "Snapshots have been disabled by the user, save request is ignored"), also with a per-AVD `quickbootChoice.ini` (`saveOnExit = true`). The remaining switch is the emulator's global settings, shared with the other session's `Pixel_9_Pro`, so it was not changed. Even working, it maps the RAM file shared, so every boot would need a fresh APFS clone of a pristine copy, and in-place resets would not benefit. Time-boxed and stopped. The feature is on by default in emulator 37.1.11, but it only applies to `default_boot`, which is restored at emulator startup, and that path corrupts guest RAM here (see Guest restarts).
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

### Host GPU and lean devices (2026-11-06, evening)

**Why the default changed.** An idle headless emulator with `-gpu auto` used about half a core
while the guest itself was idle (guest `top`: system_server 1%, surfaceflinger 3%). A 3 s
`sample` of the emulator process showed the cause. Without a window, `auto` selects SwiftShader
(`vulkan_mode_selected:swiftshader gles_mode_selected:swangle`), and two of SwiftShader's
worker threads spin while they wait for work (`marl::Scheduler::Worker::spinForWork` ->
`cthread_yield`), each about a third of a core. The vCPU threads were waiting (`pselect`).

Idle CPU of the emulator process, a read-only cold-booted instance, 60 s after boot:

| `-gpu` / flags | idle CPU (% of a core) | memory | SwiftShader loaded |
|---|---|---|---|
| `auto` (the old default) | 42% | 4.45 GB | yes, spinning |
| `auto -feature -Vulkan` | 51% | 4.42 GB | yes (GLES runs on it) |
| `swiftshader_indirect` | 55% | 4.42 GB | yes |
| **`host`** | **19%** | 4.26 GB | no |
| `auto -cores 2` | 45% | 4.45 GB | yes |
| `auto -vsync-rate 30` | 46% | 4.46 GB | yes |

**Host GPU re-tested.** The earlier instability (below) was seen at load average 40-230, next to
the startup snapshot restore bug that has since been found. On emulator 37.1.11 with `-gpu host`:
- `scripts/e2e-android.sh` passed 3 times (20/20 each). The first run also showed the new
  baseline retake from a renderer mismatch.
- The benchmark rows below all completed. Six resets in a row kept the emulator's memory flat
  (5403 -> 5422 MB).
- The emulator log had no `error null ctx`, `EGL fence` or kernel errors. After snapshot loads it
  had 4 `bad color buffer handle` and 2 `stale colorbuffers` lines, with no visible effect: the
  screenshot after a reset showed Settings correctly.

**Lean devices.** The Play image starts Google apps an app under test does not need. agemu turns
14 of them off before it takes the baseline (`internal/android/lean.go`; 12 exist in this
image):
- Messages, Photos, device restore, the Google app, Android System Intelligence;
- Digital Wellbeing, YouTube, YouTube Music, Gmail, Maps, Calendar, Drive, Meet, Device Health.

Google Play services, the Play Store (Play Billing, Play Integrity), WebView, the keyboard, the
launcher and the system UI stay on. On a cold boot, turning these apps off took the first
minute from 196% to 120% of a core and gave the guest 120 MB more free memory; idle CPU did not
change (that cost was the GPU's).

**In agemu's own flow.** Each configuration was booted from its own baseline, then reset twice
with Settings opened, sent home, and measured in two 30 s windows:

| | before (`auto`, apps on) | now (`host`, lean) |
|---|---|---|
| emulator memory | 5.94 GB | **5.44-5.51 GB** |
| idle CPU | 54-79% of a core | **24-25%** |

| operation (`agemu-bench`, daemon) | before (`auto`, results above) | now (`host`, lean) |
|---|---|---|
| agent step: find + tap + wait for next screen + screenshot | 1506 ms | **866 ms** |
| launch app (cold start) | 602 ms | **151 ms** |
| launch app (warm start) | 79 ms | **32 ms** |
| reset to clean state | 2141 ms | **2035 ms** |
| boot to agent-ready (powered off) | 5552 ms | **5116 ms** |
| screenshot | 4.4 ms | 3.9 ms |

The adb baselines got faster too (cold launch 601 -> 186 ms), because both tools drive the same
emulator.

**Animations off (opt-in).** With `AGEMU_ANDROID_ANIMATIONS=0`, agemu sets
`window_animation_scale`, `transition_animation_scale` and `animator_duration_scale` to 0 after
every boot, resume and reset, as Espresso setups do. A reset restores the baseline's settings,
hence the re-apply. adb reconnects 0.2-0.6 s after a snapshot load, and agemu retries until it
answers. The agent step, alternating 1x / off / 1x / off: 867, 379, 899 and 357 ms. A cold app
launch did not change (130-145 ms). Apps then behave as on a phone with animations turned off in
the developer options, so it is not the default.

**Against the standard emulator.** For comparison, the same AVD as Android Studio runs it: a
window, audio, the boot animation, and no `-lowram` (emulator 37 then gives an API 37 image
4 GB of RAM). This is a read-only cold-booted instance, measured the same way:

| | standard emulator | agemu |
|---|---|---|
| cold boot to `sys.boot_completed` | 19.3 s | 9.0-12.6 s; agent-ready from agemu's baseline in 5.1 s |
| first minute after boot | 202% of a core | 120% (lean, cold boot) |
| emulator memory | **7.5 GB** | 4.3 GB cold boot, 5.4 GB after baseline loads |
| idle CPU | **86% of a core** | 19% cold boot, 24% after baseline loads |

Memory in these tables is macOS's `phys_footprint`; in a later run under heavy host swap (12-14 GB
in use) the emulator's `phys_footprint` exceeded the sum of its memory regions (`footprint`,
`vmmap`) by 1.3-2.1 GB, for a reason not known.

The idle CPU rows above are single sessions on a busy host. The interleaved 30 s windows on the
launcher home in the next section ranged from 5.8% to 93.4% of a core, as the guest's own
background work comes and goes.

### Frame stream idle stop (2026-11-06, night)

agemu's frame stream (see Screenshots) makes the emulator read back every frame the guest
posts, and agemud copy it, whether or not anyone takes a screenshot. With the idle stop
(`AGEMU_ANDROID_STREAM_IDLE_S`, default 30 s) the subscription is closed after 30 s without a
call if the screen is still posting frames then: at least one per 3 s over the last 10 s.

The measurements below ran with a first version of that rule, which averaged the frames posted
since the last call over the whole idle time. A transition of a dozen frames right after a tap,
followed by a static screen, then also closed the stream (reproduced in a unit test). The rule
now looks only at the recent frame rate (`TestFrameStreamIdleStopSparesBurstThenStatic`,
`TestFrameStreamIdleStopWhileFramesKeepComing`). On the two screens measured, which post frames
all the time, both versions close the stream at the first check, 30 s after the last call.

**Method.** `agemu-pixel` (host GPU, lean baseline), emulator 37.1.11. For each 30 s window the
daemon was restarted with the arm's binary (HEAD f4286db, or HEAD plus this change), took one
screenshot and then got no calls for 35 s. CPU is the emulator (qemu and netsimd) plus agemud,
from `proc_pid_rusage`. Windows were counterbalanced (ABBA). The app's frame rate and jank are
from `dumpsys gfxinfo <pkg>`, reset before and read after each window. The host was shared
(1-minute load 6-388). Values are medians (range).

**Clock app, running stopwatch** (about 37 frames/s). 7 windows per arm (ABBAABBAAB, then
BAAB):

| | stream open (before) | idle stop |
|---|---|---|
| emulator + agemud (% of a core) | 57.5 (51.3-66.5) | **39.9 (37.5-42.7): -30.6%** |
| emulator | 53.4 (47.7-62.1) | 39.9 (37.5-42.7) |
| agemud | 4.0 (3.6-4.4) | 0.0 |
| the app's frame rate (gfxinfo) | 37.3 fps (12.4-37.5) | 36.9 fps (34.4-37.4) |
| the app's janky frames | 58.2% (57.0-73.2) | 58.1% (57.6-59.0) |
| emulator memory | 4951 MB | 4948 MB |

- The cut is 30.6%, with a 95% bootstrap interval of about 24-38% (n=7 per arm), so "at least
  30%" is a point estimate, not established.
- Every idle-stop window was below every stream-open window, also in the low-load windows alone.
- In two stream-open windows the app ran slower (12.4 and 29.4 fps, at load 65 and 87). The
  stream-open median without them is the same, 57.5.
- The first 10 windows alone: 60.7 against 40.6 (-33%).
- So the open stream cost about 0.48% of a core for each frame per second the guest posts
  (17.6 points at 37 frames/s).
- The app's frame rate and jank were measured 35-65 s after the last call, not at the moment an
  input reopens the stream.

**Launcher home** (its analog clock widget posts about 1 frame/s). 5 windows per arm (ABBA,
then ABBAAB: one window was lost when the host's disk filled up, and the order was resumed):

| | stream open (before) | idle stop |
|---|---|---|
| emulator + agemud (% of a core) | 15.0 (5.8-93.4) | 11.4 (9.4-65.7) |
| agemud | 0.2 (0.1-1.2) | 0.0 (0.0-0.1) |
| the launcher's frame rate | 1.0 fps (1.0-1.3) | 1.0 fps (0.0-1.1; no frame in 2 windows) |

The effect here is unresolved: the windows overlap completely, and the guest's own bursts (up
to 93% of a core in one window) are much larger than the stream's cost. In 2 of the idle-stop
windows the launcher posted no frame at all, so they do not measure the 1 frame/s condition. At
about 0.48% per frame per second, the stream would cost about half a point on this screen; the
2-3 points estimated in an earlier comparison were not confirmed. On a screen that is static at
the check (Settings) the stream stays open, so nothing changes there.

**Fidelity.** The guest is not touched: same AVD, flags and snapshot. The app's frame rate and
jank did not change (tables above).
- Screenshots after an input that reopened the stream: `TestDeviceScreenshotFresh` with
  `AGEMU_TEST_IDLE_STOP=1` (1 s idle; every tap or back key is sent after the stream stopped):
  20 of 20 streamed frames byte-identical to a direct framebuffer read, 0 stale. Stream wait
  53-434 ms, against 261-385 ms with the stream kept open.
- `TestDeviceStreamReopenFresh` takes the screenshot right after the input: either the input
  reopens the stream, or a screenshot reopens it and the input follows 0-250 ms later, while
  the subscription waits for its first message. It ran as two pairs of 32 rounds, each pair
  with the idle stop and with the stream kept open:
  - Second pair (with generation-time logging): with the idle stop, 25 showed the new screen or
    a frame of the transition, 7 were pixel-identical to the screen before the input, and 0
    were older than the input; the 7 were frames the guest posted 31-190 ms after the input
    that did not show its effect yet. With the stream kept open: 22 and 10, also 0 older.
  - First pair (no generation-time logging): 5 pixel-identical frames with the idle stop, 10
    with the stream kept open. These cannot be classified as stale or as an early frame of the
    transition.
  - Pooled: 12 of 64 pre-input-looking frames with the idle stop, 20 of 64 with the stream kept
    open. Pixel-identical early frames are how agemu's screenshot treats an input's first
    frame, with or without the idle stop.
  - In these runs the control kept the stream open by touching it before each input; with a
    1 s idle time a long round could still have closed it, which would shrink the difference
    measured below, not create it. The test's control now sets `AGEMU_ANDROID_STREAM_IDLE_S=0`.
- Emulator assumption (`TestDeviceStreamReopenGap`, see Screenshots): 150 new subscriptions on
  a static screen, a blinking cursor and the running stopwatch; 68 had a frame posted before
  their first message; no frame was missed. (On the 37 fps stopwatch a later frame always comes,
  so most of the evidence is the 100 cursor subscriptions.)
- 1000 stops and reopens (`TestDeviceStreamSoak`, 200 ms idle, on static Settings): emulator
  threads 78 -> 77, footprint 3036 -> 3047 MB (`footprint`; 3036-3054 MB at the checkpoints
  every 100 cycles), first message median 136 ms, max 495 ms.

**The first call after a stop.** CLI wall time with `AGEMU_ANDROID_STREAM_IDLE_S=2` and 4 s
without calls before each sample, interleaved ABBAABBA (the daemon trace shows which path served
each screenshot):
- Launcher home (the stream stops): the first screenshot took 44.5 ms (24.7-232.7) against
  35.6 ms (19.3-87.9) with the stream open, n=12 each, load 69-117. That is +9 ms (median). It
  reads the framebuffer. So does a second screenshot right after it, because the reopened
  subscription's first message has not arrived yet.
- Static Settings (the stream stays open): 17.0 ms (14.1-59.7) against 20.6 ms (15.4-84.8),
  n=6 each, served from the stream in both.
- **An input that reopens the stream, then a screenshot at once is slower** (in-process, both
  pairs of `TestDeviceStreamReopenFresh` above): median 498 ms against 261 ms with the stream
  kept open, mean 417 against 318 ms, n=64 each (37-718 and 61-717 ms). The pairs differed: 615
  against 248 ms in the first, 311 against 307 ms in the second. Over 500 ms: 32 of 64 against
  13 of 64. After a reopen the screenshot more often waited for the transition to settle, and
  it returned fewer pre-input-looking frames (12 against 20 of 64): latency traded for
  freshness, not a loss of correctness. With the current rule this path runs only after the
  screen was still posting frames 30 s after the last call.
- On a screen that keeps animating, both read the framebuffer: the copy is never current while
  frames keep coming (by design; not measured here).

Two choices keep the first call cheap. A seq-1 first message counts as a posted frame, so an
input's first frame can arrive in it. In an earlier version every first message counted as
generated before the input, so a tap's screenshot waited for a second frame (tap then
screenshot: 295-894 ms, against 110-550 ms with the stream open, n=4). And a screen that is
static at the check keeps its subscription, so its first screenshot after idle is served from
the copy as before.

`scripts/e2e-android.sh` passed 20/20 with `AGEMU_ANDROID_STREAM_IDLE_S=1` (first version of
the rule). With the current rule (2026-11-07, `agemu-lab`, compressed baseline) it passed 20/20
with the default and with `AGEMU_ANDROID_STREAM_IDLE_S=1`. One more pair of
`TestDeviceStreamReopenFresh` runs, 16 rounds each, back to back at load about 13 (a smoke test,
not interleaved): median 319 ms (72-841) with the idle stop against 416 ms (79-765) with
`AGEMU_ANDROID_STREAM_IDLE_S=0`, 4 pre-input-looking frames in each, 0 stale.

### Earlier supplementary run: `AGEMU_ANDROID_GPU=host` (previous version, 2026-11-05)

These are the cheap operations the previous version measured (22:11–22:16, load 40–230, from a fresh reset) with the host GPU (gfxstream). They are faster, mostly because
screenshots read back from the host GPU in about 9 ms. With the host GPU this emulator was
not stable on this machine: gfxstream `error null ctx` / `Failed to create EGL fence sync`, a
guest that stopped answering adb, and repeated guest restarts. So the default is `-gpu auto`. The guest restarts of that time are now explained by the startup snapshot restore (see Guest restarts), not by the GPU; the gfxstream errors are not. The host GPU was not re-tested in that round; it was later, see "Host GPU and lean devices", and `-gpu host` is now the default. The
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

### Test capabilities (2026-11-07)

The capabilities of [CAPABILITIES.md](CAPABILITIES.md) (`internal/android/capabilities.go`,
`android-agent/src/agemu/Caps.java`). Each one makes the system call that the user action or
the standard command makes, from a process that is already running: the on-device agent (the
shell uid, so the same permissions as `adb shell`) or the emulator's gRPC. The standard tools
pay for a host `adb` process, a guest shell and a `cmd` process per call.

| Capability | Standard command | agemu |
|---|---|---|
| open | `am start -W -a VIEW -c BROWSABLE -d <url>` | Agent: `Intent(ACTION_VIEW, uri)` + `CATEGORY_BROWSABLE` + `FLAG_ACTIVITY_NEW_TASK`, no package or component, `startActivityAndWait` with the shell as caller. Returns the package of the activity that took it (`WaitResult.who`); the next tree read waits for its window, as after `launch`. No handler: "no app handles <url> as a link". |
| permission | `pm grant/revoke`, `pm set-permission-flags`, `appops set` | The agent reads the app's requested permissions with their protection, grant state and flags (`IPermissionManager.getAllPermissionStates`, one call; platform permission protections cached); the host plans the change (`planPermissions`, unit-tested) and the agent applies it: `UiAutomation.grantRuntimePermission` / `revokeRuntimePermission`, then `updatePermissionFlags`, or `IAppOpsService.setMode` for app-op permissions. The host checks the state the agent returns. |
| location | `adb emu geo fix` | gRPC `SetGps` on the emulator, which feeds the guest's GNSS HAL (the same fix `geo fix` sets). `clear` sets the emulator's default fix: agemu reads it with `GetGps` when it starts the emulator, before anything can set one, and keeps it next to the device's log for that emulator process. Emulator 37 reports 39.237256, -123.150032 (constants in its binary), which is also the fallback for an emulator agemu did not start. |
| clipboard | none (`cmd clipboard` has no shell command on this image) | Agent: `IClipboard.setPrimaryClip` / `getPrimaryClip` / `clearPrimaryClip` as `com.android.shell` (the shell may read the clipboard in the background). |
| appearance | `cmd uimode night yes/no` | Agent: `IUiModeManager.setNightMode`, waits until the global configuration has the night bit, then until the focused app has redrawn: an activity that declares `uiMode` in `configChanges` redraws in place; any other is recreated, and agemu waits for its new window with content (at most 2 s). Read: the global configuration's night bit. |
| apps | `pm list packages -3` | Agent: `getInstalledPackages`, non-system ones, label read from the APK's resources in the device's current configuration (its locale, as the launcher shows it), `versionName`. Each APK's label is read once per update and locale and then cached in the agent, because loading an APK's resources costs time per app. |
| uninstall | `adb uninstall` | Agent: `IPackageManager.deletePackageAsUser(DELETE_ALL_USERS)`, waiting for its observer. A system app is refused. |
| clear-data | `pm clear` | Agent: `IActivityManager.clearApplicationUserData`, waiting for its observer: what `pm clear` and Settings' "Clear storage" call. |

The probe app `testapps/android/AgemuProbe` (built by `testapps/android/build.sh`, no Gradle)
shows on screen what an app sees: the URL that opened it and whether the intent was
BROWSABLE, each permission's grant and rationale state, a "Ask camera" button, its GPS
listener's fix, the clipboard, the night mode, a launch counter in its own data, its pid and
whether it may draw overlays. `scripts/e2e-android.sh` checks every capability against it (13
checks, after the existing 20).

Measured on agemu-lab (API 37.1, host GPU), interleaved (baseline, CLI, daemon, then the
reverse), n=12 per arm, medians with ranges. The host also ran another session's Gradle,
Metro and simulators: load average 22-110 during these runs. The two rows that set the
appearance come from a repeat run of the same build at load 22-30; the first run of that
build, at load 35-110, gave daemon speedups of 2.5x and 1.5x. An earlier build (v1) measured
the app-visible appearance row at 0.6x (baseline 153.7 against daemon 246.1 ms) and clear-data
at 1.5x with the same method. The daemon arm is a persistent socket client (no process start),
as `agemu mcp` uses it; the standard tool's figure includes its `adb` process.

| Operation | Standard tool | Standard tool ms | agemu CLI ms | agemu daemon ms | daemon speedup |
|---|---|---|---|---|---|
| open a deep link, app in front | `am start -W -d` | 34.7 (28.1-146.1) | 9.3 (7.6-46.7) | 3.7 (2.4-22.9) | 9.4x |
| open a deep link, app stopped | `am start -W -d` | 221.8 (156.8-623.6) | 216.4 (123.9-605.2) | 260.9 (118.2-675.5) | 0.9x |
| grant camera | `pm grant` | 35.5 (28.6-108.0) | 10.6 (8.5-51.8) | 4.2 (2.5-9.6) | 8.5x |
| revoke camera | `pm revoke` | 30.4 (25.0-65.1) | 13.8 (8.3-30.3) | 4.1 (2.5-31.3) | 7.5x |
| reset camera | `pm revoke; pm clear-permission-flags` | 31.8 (29.2-47.6) | 8.6 (7.6-16.2) | 2.6 (2.4-5.7) | 12.3x |
| set location (the call; the app sees the fix no sooner, see below) | `adb emu geo fix` | 16.3 (13.9-27.6) | 8.3 (5.5-28.8) | 0.6 (0.2-2.1) | 29x |
| set clipboard | none; `adb shell true` | 27.2 (18.8-35.8) | 8.6 (6.5-27.2) | 2.4 (0.8-8.3) | (11x the adb floor) |
| read clipboard | none; `adb shell true` | 24.4 (20.6-55.1) | 9.9 (7.0-25.4) | 1.8 (0.9-14.8) | (14x the adb floor) |
| dark/light, home screen | `cmd uimode night` | 67.4 (36.7-489.5) | 49.2 (26.2-202.3) | 27.2 (16.7-77.6) | 2.5x |
| dark/light until the app in front shows it (not established, see below) | `cmd uimode night` + agemu's wait | 280.6 (130.0-1263.1) | 200.9 (134.5-693.4) | 168.4 (101.8-360.4) | 1.7x |
| read the appearance | `cmd uimode night` | 22.8 (21.2-26.6) | 6.0 (5.2-9.0) | 0.8 (0.7-7.5) | 28x |
| list user apps (one user app installed; before the label cache) | `pm list packages -3` (ids only) | 28.0 (24.8-54.4) | 14.0 (10.8-30.4) | 8.5 (5.6-17.4) | 3.3x |
| clear data (app in front) | `pm clear` | 79.3 (48.8-273.5) | 44.0 (30.2-118.3) | 35.6 (24.2-102.7) | 2.2x |
| uninstall | `adb uninstall` | 119.1 (62.1-147.9) | 53.0 (37.0-115.5) | 61.7 (27.1-95.2) | 1.9x |

- **Where 3x is not reached.** Timed inside the guest (including starting `cmd`, and about 3 ms
  of timer overhead), `cmd uimode night` takes 22.9 ms (16.5-64.3), `cmd package clear` 13.5 ms
  (12.1-43.6, app idle for 0.5 s), `cmd package uninstall` 77.9 ms (31.9-110.5), n=10 each; a
  cold start of the probe is the 118-675 ms of the cold-open row. agemu removes the host and
  shell processes (20-30 ms). For uninstall and a cold open the guest's own work is shown to
  dominate. For clear-data it is not: the floor was measured with the app idle, while the table
  cleared an app that had just been launched (35.6 ms), so the floor for that case is unknown.
  For the appearance on the home screen, the agent's trace shows the night-mode change applied
  in 11-16 ms plus 1-3 ms of settling, below the 27.2 ms median; the rest may be agemu's own
  accessibility and configuration polling.
- **The appearance call does more than `cmd uimode`.** It returns once the app in front has
  redrawn. Right after it returns, the probe's UI tree showed the new mode 10 of 10 times; right
  after `cmd uimode night` returned, 1 of 10 times. That is the solid result. How much sooner
  the app shows the change is not established: the baseline arm's "shown" time is measured by
  agemu's own wait loop (50 ms polls, or a frame wait of up to 250 ms), its samples are bimodal
  (130-210 and 260-1263 ms), and the v1 build measured 0.6x with the same method.
- **Location reaches the app at the GNSS's pace.** The probe's GPS listener saw a new fix
  111.7 ms (71.0-4128) after `geo fix` started, 109.5 ms (71.0-129.1) without one first `geo fix`
  that took 4.1 s, and 113.9 ms (58.4-125.9) after agemu's call started (n=10 each,
  interleaved). As the app sees it, location is 1.0x: only the call returns sooner (agemu
  0.5 ms against 14.8 ms for `geo fix` in that run), and it does not wait for the app to get the
  fix.
- **open with the app running in the background** (a warm start) was not measured; the
  table has the app in front (`singleTop`, delivered to the top activity at once) and stopped.
- **Permission planning moved two agent round trips to one read and one write.** A first version
  read every permission's info and flags one by one and returned the whole state: 10-12 ms
  (2.1-2.9x). Reading all states in one call, caching platform protections and returning only
  the changed permissions brought it to 2.6-4.2 ms.
- **Clipboard: the agent, not gRPC.** The emulator's gRPC `SetClipboard` works headless (the
  guest had the text at the first read, 10 of 10), but an empty text leaves a clip holding ""
  (the probe showed `clipboard: ""`), not an empty clipboard as on a fresh device;
  `clearPrimaryClip` does. Reading through the agent returns what the guest holds, not the
  emulator's copy of it.

Fidelity, as the probe app showed it:
- A link opens only activities whose intent filter has `BROWSABLE`: `agemu open agemunb://x`
  fails while `am start -d agemunb://x` opens the probe's non-browsable activity. https URLs go
  to Chrome, which shows its first-run screen on a fresh device. The caller is the shell.
- grant: the app sees the permission granted, flag `USER_SET`. revoke: the app's process is
  killed (pid changed or gone), flags `USER_SET|USER_FIXED`, and its `requestPermissions` returns
  denied without a dialog. reset: no user flags, and the next request shows the system dialog
  ("While using the app / Only this time / Don't allow"). `location` after `location-always`
  takes background location away. App-op permissions: `SYSTEM_ALERT_WINDOW` grant makes
  `Settings.canDrawOverlays` true, revoke false (`appops` shows allow / deny / default).
- The clipboard toast "Agemu Probe pasted from your clipboard" shows when the app reads a clip
  agemu set, as when another app copied it.
- Dark mode recreates the probe's activity (its launch counter goes up), as on a device. An
  activity that declares `uiMode` in `configChanges` (common in React Native, Flutter and
  Compose apps) redraws in place: agemu then returns without waiting for that redraw (a
  `screenshot` right after still waits for the next frame). Only the probe, which is
  recreated, was checked.
- clear-data: the process stops, the launch counter starts again at 1, permissions are reset.
- Survives reset: nothing found. The GPS fix did survive a snapshot load (it is emulator state);
  reset and standby now send the default fix before the load (so the restored guest never gets
  the old one) and check it after.
- `grant location-always,location` leaves background location not granted (the later name
  wins), and background location is always granted after the foreground pair.
- Not checked: fused location (Play services; only `LocationManager` GPS was), approximate
  location, a chooser with two handlers, App Links, hard-restricted permissions. The clip's
  label (`agemu`) and source (`com.android.shell`) are visible to apps.
- The Mac clipboard did not reach the guest (checked by writing to the Mac's clipboard and
  reading the guest's). The other direction (guest to Mac) is reported checked, but no log of it
  was kept; the emulator does keep a copy of the guest's clip.

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
- **A launch or open right after a key waits for the key.** `key` returns once the key is queued. A `launch` or `open` that starts an activity before the system has handled that key (home 1 ms before, from one MCP connection) lost the race in 3 of 6 tries: the key sent the new activity behind, it never reported being displayed, and `startActivityAndWait` (also `am start -W`) did not return; agemu answered after its 60 s agent timeout. With 300 ms between them it never happened (0 of 8). The agent now starts an activity no sooner than 300 ms after its last key (12 of 12 such starts returned), and bounds the wait at 20 s with an error that names the app in front. This adds up to 300 ms to a `key` followed at once by `launch` or `open`. A start that passed the 20 s bound keeps a thread in the agent and a binder thread in system_server until it returns; after 3 such starts the agent refuses new ones with an error until one returns or `reset` restores the baseline.
- **The agent's socket has no authentication.** The on-device agent listens on an abstract unix socket as the shell user (only agemu's TCP path through the emulator console carries a token). A process that can connect to it could use the capability commands: grant permissions and app-ops, read the clipboard, uninstall or clear apps. `tap` and `key` already gave equivalent control. Whether an app under test can connect to a shell-domain socket depends on SELinux policy, which was not checked.
- **Tap, swipe and key return when the input is queued.** `agemu tap` returns once the emulator has queued the touch; `agemu key` once InputManager has queued the key. `adb shell input tap/keyevent` waits until the app has handled the event. Agents should follow them with `wait` or `ui` (milliseconds) or `screenshot` (which waits for the input's frame). `type` returns after the app's UI thread has applied the text; `launch` after the activity has drawn, like `am start -W`.
- **Back-to-back text changes are bound by the app.** `ACTION_SET_TEXT` and the cursor read run on the app's UI thread. Idle, a `type` takes 5–10 ms in the agent; issued while the app is still redrawing the previous change (software rendering on a loaded host), it waits 16–130 ms for the UI thread.
- **Standby keeps guest RAM allocated.** A device in standby uses no CPU, but the paused emulator keeps its guest RAM (about 2 GB) on the host. Run `shutdown` twice, or set `AGEMU_ANDROID_STANDBY=0`, to free it.
- **Disk.** One baseline snapshot is about 2 GB (`ram.bin` is the full guest RAM). The AVD's userdata qcow2 grows to about 3 GB. `AGEMU_ANDROID_SNAPSHOT_COMPRESS=1` (the emulator's `ANDROID_SNAPSHOT_COMPRESS=1`) saves `ram.bin` LZ4-compressed, and `0` forces raw. Unset, the emulator decides: emulator 37.1 can turn compression on by itself ("Enabling RAM compression: only %u MB of free RAM", "snapshot is on HDD"). Measured on 2026-11-06, while other sessions were writing to the same disk (load average 4-11, 6.5-7.9 GB of swap in use):
  - **Disk:** `ram.bin` 0.83-0.86 GB instead of 2.04 GB (58-59% less).
  - **Reset: depends on the host's disk.** Both snapshots were loaded alternately in one emulator, 14 loads each: 1.77 s compressed against 2.25 s raw (medians; every compressed load was faster than every raw load). But in the one raw round with a fast disk (its baseline save took 3.1 s, against 7.8 s in the other raw round), raw resets took 1.16-1.78 s (median 1.36 s), faster than any compressed load measured (1.61 s at best). On a quiet disk the gain can vanish or reverse.
  - **Powered-off boot:** 5.64-6.04 s compressed against 5.00-5.61 s raw (4 boots each), but the compressed boots ran at about twice the host load of the raw ones, so the size of the penalty is not established.
  - **Cost:** about 3 CPU-seconds more per load. The emulator used 5.6 instead of 2.7 CPU-seconds per load (medians), because LZ4 decompresses on a thread pool (about 3 cores for 1.7 s).
  - **Memory:** unchanged (emulator footprint after resets, 10 per format). Both formats restore every guest page.
  - **Guest:** coarse checks found no difference: uptime and boot count continued, the crash buffer stayed empty, the kernel console was clean on 4 compressed loads, and one Settings, typing and screenshot pass behaved the same. Guest RAM was not compared byte for byte.
  - It stays opt-in, for a host that is short of disk.
- **Rotation is not handled.** Profiles are portrait; a landscape app or device rotation is not mapped.
- **Emulator features are part of a snapshot.** Starting the emulator with a `-feature` set that differs from the snapshot's makes it print "different emulator features" and **delete** the snapshot, then cold boot. Do not pass `-feature` flags through `AGEMU_ANDROID_EMULATOR_FLAGS`.
- **The measurements ran on a shared, often saturated host.** The M2 Pro with 16 GB shared the machine with another emulator, iOS simulators, a browser and other sessions; load averages ranged from 3 to 450 and swap was nearly full during this round. Absolute numbers are noisy (see the p95 columns and the run times listed with the table); both sides of each row ran back to back under the same conditions.
