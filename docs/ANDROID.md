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
| `AGEMU_ANDROID_SNAPSHOT_COMPRESS` | `1` | The emulator saves the baseline's RAM LZ4-compressed (`ram.bin` 0.83-0.89 GB instead of 2.04-2.05 GB). `0`: raw. agemu always passes the emulator's `ANDROID_SNAPSHOT_COMPRESS`, so the emulator never picks a format by itself. Applies when a baseline is taken (first boot, an agent upgrade, a GPU mode change); an existing baseline keeps its format until then, and both formats load. See Caveats (Disk). |
| `AGEMU_ANDROID_BOOT_LOAD` | `inplace` | `startup`: restore the baseline with `-snapshot` at emulator start. This corrupts guest memory on emulator 37.1 (see Guest restarts). |
| `AGEMU_ANDROID_EMULATOR_FLAGS` | (none) | Extra emulator flags, e.g. `-show-kernel` to log the guest kernel console. Never pass `-feature`: the emulator deletes snapshots whose feature set differs. |
| `AGEMU_ANDROID_LOGS` | `1` | `0`: no log capture: `logs` and `crashes` answer that capture is off, and calls carry no crash notice. See Logs and crashes. |
| `AGEMU_ANDROID_TAP_WAIT` | `1` | `0`: a tap by selector touches the element where the first tree read found it, without waiting for transitions or for it to stop moving (agemu before 2026-11-07). See Selector taps. |
| `AGEMU_ANDROID_TRACE` | `0` | `1`: log whether each screenshot came from the frame stream, how long each selector tap waited, each crash record the log capture makes, and each answer of the app watcher. |

## How each operation works

| Operation | Standard tool | agemu |
|---|---|---|
| boot | cold boot | `shutdown` puts the device in **standby**: the baseline snapshot is restored in place and the VM is paused. `boot` resumes it. When the device is powered off, `boot` starts the emulator with `-no-snapshot-load` and restores the baseline **in place** (gRPC `LoadSnapshot`) as soon as gRPC answers, then waits for the restored agent over TCP. |
| reset | cold boot | gRPC `LoadSnapshot(agemu-baseline)` in the running emulator. The agent restored with the snapshot answers over TCP 30–110 ms later; agemu does not wait for adb. The active screen profile is re-applied. |
| screenshot | `adb exec-out screencap -p` | The latest frame from a `streamScreenshot` subscription (MMAP transport), already in host memory: crop, resample, encode. Falls back to `getScreenshot` while the screen keeps changing. The subscription is closed after 30 s without calls if the screen is still posting frames then, and reopened by the next call. See Screenshots. |
| tap / swipe | `adb shell input tap/swipe` | gRPC `sendTouch` on the emulator's virtio multitouch screen, the same device a finger would use. Returns once the touch is queued. A tap by selector first waits (bounded) until the element can take it: no window transition runs and it has stopped moving. See Selector taps. |
| type | `adb shell input text` | The agent inserts the text at the cursor of the focused editable node with `AccessibilityNodeInfo.ACTION_SET_TEXT` (what UiAutomator and Appium `setText` do). Returns once the app's UI thread has applied it, so its TextWatchers and listeners have run. `\n` / `\t` become Enter / Tab keys. `type --keys` (MCP `keys: true`) sends one key event per character through the IME, like `input text`. With no focused editable node, `type` also falls back to keys. |
| key | `adb shell input keyevent` | The agent injects the key through `IInputManager.injectInputEvent` (the call `input` makes) in async mode: it returns once InputManager has queued the down and up events, like a tap. Follow with `wait` or `ui` to see the effect. |
| UI tree | `uiautomator dump` + reading the XML | The agent walks `AccessibilityNodeInfo` of every on-screen window, top-most first, over its warm UiAutomation connection, and returns compact JSON. The Go side maps it to `api.Element`. After a `launch`, the first tree read waits (up to 3 s) until the accessibility layer reports the launched app's window on screen, or until the launched app has no live process. A window whose app process is gone (dead, crashing, or stopped by `crash_dump`) is listed without content instead of being waited for (see "The first tree read after a crash"). |
| launch | `am start -W` | The agent calls `IActivityTaskManager.startActivityAndWait`, the call `am start -W` makes, and returns when it does (the activity has drawn). The wait for the app's accessibility window moved to the next tree read. A launch less than 300 ms after a `key` waits until then (see Caveats), and the wait is bounded at 20 s. |
| open, permission, clipboard, appearance, apps, uninstall, clear-data | `am start -d`, `pm grant/revoke`, `cmd uimode`, `pm list/uninstall/clear` | The agent makes the system service call behind the standard command, as the shell. See Test capabilities. |
| location | `adb emu geo fix` | Emulator gRPC `SetGps`, the same GNSS fix `geo fix` sets. |
| logs, crashes, the crash notice | `adb logcat -d`, `dumpsys dropbox`, `dumpsys activity exit-info` | One `logcat -B` stream per device (adb `exec:` service) into ring buffers in the daemon; crash records from the crash and events buffers as they are logged. See Logs and crashes. |

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
- **The input's effect, not the old screen.** The guest can post frames right after an input
  that still show the screen from before it (the app has not drawn its effect yet). For tap,
  swipe, key and type, agemu keeps the copy that was current when the input was sent, and a
  screenshot after it does not take a frame with the same pixels for the input's effect until
  500 ms after the input; then it returns the unchanged screen (the input may have changed
  nothing) and logs that. It does not cover every case: there is no reference when the copy was
  not current as the input was sent (a frame posted but not copied yet, or the stream reopening
  after idle), and a screenshot that falls back to a framebuffer read is not checked. In the
  measurement (Agent-loop reliability) 5 of 120 screenshots still showed the old screen; 4 of
  them returned in 41-135 ms with no log line, so the skip did not engage for them. Those are the
  only two ways `get` can return the old screen that fast without the log line: no reference, or
  a stream that is not delivering (then the framebuffer is read). A traced run (round 5, a build
  with extra log lines, load 5-6) did not reproduce them: 40 Settings tap/back screenshots, 0 old
  screens; all 30 served from the stream within 600 ms of their input had a reference, and the
  stream never stopped. A missing reference did occur, on 21 inputs of the tap driver sent while
  the screen still changed: each time a frame had been notified 0-18 ms before the input and its
  copy (made after 20 ms without a new frame) did not exist yet. The round-4 screenshots ran at
  load 30-94, where copies lag further; which of the two ways served them is not established.
  The skip also makes screenshots right after an input slower (Settings back 91 -> 381 ms,
  dialog open 334 -> 809 ms, medians).
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

### Selector taps

A tap by selector (`tap --text/--id/...`) reads the UI tree and touches the element's centre.
On an element that has just appeared or is still moving, that touch often does not do what the
agent asked (measured in Agent-loop reliability): a closing dialog takes every touch until its
exit animation ends, a touch on a scrolling list stops the scroll instead of clicking the row,
and the runtime permission dialog's buttons are first reported where the dialog's window comes
from. So before the touch, like Playwright's actionability checks:

1. **Still screen.** When the guest has posted no frame for 500 ms (the frame stream sees every
   posted frame), the usual tree read is used as it is, and the touch goes out at once.
2. **Window transitions.** Otherwise the agent waits until WindowManager runs no window
   animation or transition and has synced its input windows (`IWindowManager.syncInputTransactions(true)`,
   which UiAutomation also waits for before an injected event since API 33), at most 2 s.
3. **At rest.** Then the element is read again, from the app rather than from the accessibility
   cache (the cache keeps a node's bounds until an event about it arrives; a scrolling list
   sends one at most every 100 ms), until the screen has posted no frame for 50 ms around a read,
   or the element's frame stayed the same across posted frames (something else animates, such
   as the previous tap's ripple). When step 2 waited for a transition (80 ms or more), the frame
   must stay the same for 600 ms instead: after a window transition the UI tree kept a
   permission dialog's buttons at their slide-in position for up to 435 ms while the screen
   still changed. Then a second check for a transition (a dismissed dialog leaves the UI tree
   before its exit animation starts). At most 1.5 s for the element, 2 s per transition wait.

When a wait gives up, agemu logs it and taps the latest read, as before. An element that
disappears while it moves is an error (`... (it disappeared while it was moving)`, no tap),
where agemu before this change would have tapped. The touch itself is the same `sendTouch` down
and up, but it reaches the app later: right after the screen moved, a tap now waits a median
72-821 ms more (see Agent-loop reliability). The waits add up to about 5 s at worst (a first
transition check under 80 ms, 1.5 s for the element, a second transition wait of up to 2 s,
then 1.5 s more). Taps by coordinates do not wait.

Limits and costs:
- The failures it prevents were measured with the selector tap sent the moment the previous call
  returned (no agent think time). With 1 s or 3 s between the calls, taps without the wait did
  not fail in these rows (0/120 at each), and the wait added -1 to +1 ms per tap at 3 s but
  84-91 ms at 1 s in three of the four rows (the screen had posted a frame less than 500 ms
  before the tap); see "With agent think time" in Agent-loop reliability.
- While the screen moves, each read after the first clears the accessibility cache, so the app's
  UI thread serializes its whole node tree for every read (13-56 ms each in the traces), several
  times per tap, during its animation. The effect on the app's frame timing was not measured; a
  large Compose tree could make each read much slower.
- A slow read (a hung UI thread) is not cut at the 1.5 s bound: the deadline is checked between
  reads.
- There is no per-call way to tap during a transition (Playwright's `force`); only the
  daemon-wide `AGEMU_ANDROID_TAP_WAIT=0` turns the waits off.
- The waits use agent commands (`settle`, `clearcache`) of the agent built with this change; on
  an AVD whose baseline holds an older agent, every reset restarts the agent until the baseline
  is retaken (agemu does that on the first boot with the new build).

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
- **`shutdown`** puts the device in standby. A second `shutdown` (or `AGEMU_ANDROID_STANDBY=0`) powers the emulator off.
- **`park`** pauses the VM where it is, with the app as it is (standby restores the baseline first); `resume` or the next call continues it. See Parking.
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
agemu's reset is now at parity with it (2.08 vs 2.26 s), without waiting for adb. Since
2026-11-07 `ram.bin` is LZ4-compressed (0.83-0.89 GB on disk), which made resets faster on a host
whose disk other sessions shared (see Caveats, Disk).

**agemu's own share** (2026-11-07, `agemu-lab`, from the CLI call's wall time and the daemon
log, loads at least 8 s apart). It is the wall time minus the daemon's load span: the CLI
process, the daemon, the display read, the GPS fix checked after the load, the wait for the
restored agent and, for a boot, the agent check and the display read. The daemon's load span
(the "emulator" column) also holds a little of agemu's own work: for a reset the GPS fix sent
before the load and the capture's bookkeeping, for a boot the port allocation, the process
launch, the 20 ms gRPC polling and the shared-memory setup. So the shares below are somewhat
low (by a few to tens of ms).

| | wall (median) | emulator (daemon's span) | agemu's own (median, range) | share |
|---|---|---|---|---|
| reset, compressed baseline (n=12) | 1.75 s | load 1.71 s (mostly `LoadSnapshot`) | 45 ms (24-105) | 2.7% |
| reset, raw baseline (n=12) | 2.28 s | load 2.23 s | 52 ms (37-137) | 2.3% |
| powered-off boot, compressed (n=8) | 2.31 s | process start to gRPC 0.86 s, load 1.34 s | 74 ms (30-117) | 3.0% |
| powered-off boot, raw (n=8) | 2.48 s | process start to gRPC 0.92 s, load 1.49 s | 70 ms (50-89) | 2.8% |

The restored agent answered 22 ms (compressed) and 33 ms (raw) after the load. With `ram.bin`
dropped from the page cache before every load the shares were 3.3-3.8% (n=10-12); one raw reset
there had 10.2% (411 ms of agemu's own time). Two cases cost far more:

- **A load within 3 s of the previous one** waits for the emulator (see Emulator deadlock): a
  reset right after a reset took 5.05-5.08 s, 2.89-2.91 s of it waiting (n=2); a reset right
  after a powered-off boot took 5.49 s, 2.87 s of it waiting (n=1). The wait alone is 52-57% of
  those calls; with agemu's other own time 55-59%.
- **A non-native profile.** Reset re-applies it after the load: with `small-phone` active,
  resets took 3.77-5.99 s against 1.78-2.91 s native (n=4 each, alternated), 1.85-2.93 s of it
  after the load (48-53% of the reset). That time is Android laying the screen out again and the
  agent's own settle around it (the window-fit loop, a quiet window of at least 300 ms, the input
  sync); how much is the settle's was not measured. A switch to `small-phone` from a settled home
  screen took 1.54-2.24 s (n=2).

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

How long the background restore runs (2026-11-07, host GPU, compressed baseline): `sample`
over a reset shows `GLBackgroundLoader::main()` in `usleep` for 2,005 of its 2,227 samples and
restoring textures (`restoreTexture`, mostly waiting for the `TextureLoader` lock) for the
rest. In one traced reset (the emulator's threads listed every 50 ms) its thread started before
`LoadSnapshot` returned and ended 2.4 s after the reset call returned, so the 3 s spacing left
about 0.6 s to spare; a second thread born 0.25 s after the call returned lived until 6.7 s
after it with no CPU (not identified). The spacing cannot be cut to the loader's real end: the
emulator has no log line or API for it, the thread has no name, and the snapshot's `ram.bin` and
`textures.bin` stay open after it ends, so nothing outside the emulator process shows when it
is done.

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

### Agent-loop reliability (2026-11-07)

How often does a tap by selector, or a screenshot, right after the screen changed do the wrong
thing, and what does the fix (Selector taps; "The input's effect" in Screenshots) cost? Measured
on agemu-lab (pixel-9 profile, host GPU, animations on) through the daemon socket, as `agemu mcp`
uses it. Each try is checked through the UI tree: a probe app (TapRace: a dialog, a bottom sheet
sliding up over 250 ms, a 100-row list with "Jump down" smooth-scrolling to row 40, a camera
permission request, and filler buttons so that a misplaced tap is logged) shows the last click as
"last: <target> #<count>". The step that makes the target appear is an action with a wait for the
target (`tap X --wait-text Y`) from a still screen; the selector tap on the target follows at
once (0 ms between the calls in every try: no agent think time). Accepted build against this
one, alternating (A B, B A, ...), 5 rounds, n = 50 per arm (25 for the scenarios marked *),
load average 13-85 at the starts of the blocks (per-scenario medians 17-35). Load was not
balanced: the arm with the wait ran at higher load in 4 of 5 blocks. Shared host, another
session's emulator and simulators running.

| scenario: tap at once after | accepted build: failed | with the wait: failed | tap call, median: accepted / with the wait |
|---|---|---|---|
| "Open dialog", the moment the closing dialog left the UI tree | 48/50 (44 lost, 4 not found) | 0/50 | 7.8 / 311 ms |
| row 42, as it first shows during a smooth scroll | 37/50 lost | 5/50 lost | 4.3 / 224 ms |
| the permission dialog's "Don't allow" | 20/50 lost | 0/50 | 3.9 / 76 ms |
| "Sheet two" in the sheet sliding up | 2/49 (1 lost, 1 wrong) | 0/50 | 5.0 / 247 ms |
| a dialog's "Confirm" * | 0/25 | 0/25 | 3.9 / 825 ms |
| row 3, right after the list screen opened * | 0/25 | 0/25 | 3.4 / 451 ms |
| Settings "Internet", right after Network & internet opened * | 0/25 | 1/25 (the next screen did not show in 3 s, load 72) | 7.4 / 421 ms |
| a still button 1 s after the previous tap (its ripple still running) | 0/50 | 0/50 | 4.0 / 98 ms |
| a still button on a screen unchanged for 1.5 s | 0/50 | 0/50 | 4.5 / 4.6 ms |

- An earlier series with the accepted build alone (n = 50 each, load medians 18-47): 50/50,
  48/50, 16/50 and 0/50 failed in the first four rows, 0/50 in the dialog, list-open, Settings
  and "1 s after the previous tap" rows. Screenshots: 13 of 52 showed the old screen (Settings
  back 11/13).
- Over the four rows that failed with the accepted build: 107/199 -> 5/200. The sheet's 2/49 is
  not significant on its own (0/50 in the earlier series); the pooled figure rests mostly on the
  dialog-reopen, list and permission rows. Over all nine rows: 107/374 -> 6/375.
- The 5 remaining list failures were all the first try of a block, the first scroll of a list
  screen just opened: the tap went to the row's resting place (y = 615) and the list did not
  click it. 0 of the 45 other tries failed.
- **Cost.** A tap on a screen that has not changed for 0.5 s costs nothing more (+0.1 ms). Right
  after a change, the tap waits for the change to end: 72-304 ms more in the rows that failed,
  and 414-821 ms more in the three rows (25 per arm) where the accepted build never failed (a
  new window's transition makes the check wait 600 ms of unchanged frame while the previous
  tap's ripple keeps the screen changing); +94 ms while a ripple still runs. No tap wait gave up
  during the measured blocks. `AGEMU_ANDROID_ANIMATIONS=0` shortens screen changes;
  `AGEMU_ANDROID_TAP_WAIT=0` turns the wait off.
- **Why the UI tree is not enough.** After "Ask camera" (tree read back to back for 1.5 s),
  the tree reported "Don't allow" at its slide-in position (y = 1562-1959) for up to 435 ms in
  7 of 10 tries with the accessibility cache cleared before every read (1 of 6 with cached
  reads); its final place is y = 1475, where the taps that failed did not go. (Screenshots
  614-996 ms after the tap, in 3 other tries, showed the dialog only in its final place; the
  frames at the time of the stale reads were not captured.) The screen kept posting frames
  until 896-1227 ms after the tap, and in every try the tree was right before the last frame
  (by at least 119 ms with fresh reads, 103 ms with cached ones). So the frame stream, which
  sees every posted frame, decides when the screen is still. In this small sample fresh reads
  were stale more often than cached ones (7/10 against 1/6, at different loads).
- **Screenshots right after an input** (tap a row or "Open dialog", or press back; 30 tries per
  case and arm): the screenshot was pixel-identical to the screen before the input, while the
  settled screen differed, in 35 of 120 with the accepted build (Settings back 25/30, Settings
  tap 2/30, dialog back 5/30, dialog tap 3/30) and 5 of 120 with the change (Settings back
  4/30, Settings tap 1/30). Median screenshot call: Settings back 91 -> 381 ms, Settings tap
  306 -> 261 ms, dialog back 301 -> 308 ms, dialog tap 334 -> 809 ms (it now waits for the
  dialog's animation to settle instead of returning the unchanged first frame). For the dialog
  tap the gain is small (3/30 -> 0/30) and 19 of its 30 screenshots still caught the dialog
  mid-animation. Of the 5 stale screenshots left, 4 (Settings back 3, Settings tap 1) returned
  in 41-135 ms with no "no frame differed" log line: the 500 ms skip did not engage for them
  (no reference, or a framebuffer read; see Screenshots for the round-5 trace, which did not
  reproduce them at low load). Only the fifth (544 ms) was
  logged.
- The touch is the same `sendTouch` down and up; only its timing changes. `scripts/e2e-android.sh`
  passed 33/33 with the change. During it, one tap (the probe app's "Refresh") gave up after
  1.5 s, was logged and sent, and its check passed.
- **Merged build smoke** (with the log capture running; load 12): the same driver, n=5 per
  scenario, 0 ms between calls: dialog reopen, list scroll, permission, sheet, dialog confirm and
  still screen 30/30 landed (tap medians 307, 235, 706, 244, 821, 3.6 ms; the permission tap
  waited 633-820 ms in 4 of 5 against a 76 ms median above, not investigated at this n); 12
  screenshots right after an input (Settings and dialog, tap and back), 0 showed the old screen.
  `scripts/e2e-android.sh`: 41/41, no tap wait gave up.

**With agent think time** (round 5). The same driver and the four rows that failed above, with
1 s or 3 s between the call that made the target appear and the selector tap (and 0 ms as a
control), the wait off (`AGEMU_ANDROID_TAP_WAIT=0`) against on, on one build (the merged one,
agent ad4b3b73), the daemon restarted per arm, alternating A B | B A over 6 rounds: n = 30 per
arm, row and think time (18 at 0 ms). Load average 2.9-82.3 at the starts of the blocks (medians
of the tries 6.2 with the wait off, 5.8 on). The first two rounds were unbalanced (in one, the
wait arm's tries ran at a median load of 93, peak 125, against 16); the four low-load rounds
alone give the same result: +82 to +90 ms at 1 s for the list, permission and sheet rows, -3.2
to -0.4 ms at 3 s, no failure in either arm.

| row | 0 ms: failed off / on | tap median off / on | 1 s: failed | tap median off / on | 3 s: failed | tap median off / on |
|---|---|---|---|---|---|---|
| reopen after a closing dialog | 18/18 / 0/18 | 3.2 / 285.2 ms | 0/30 / 0/30 | 5.6 / 6.0 ms | 0/30 / 0/30 | 6.0 / 6.2 ms |
| row 42 after a smooth scroll | 18/18 / 0/18 | 2.8 / 229.3 ms | 0/30 / 0/30 | 14.9 / 106.1 ms | 0/30 / 0/30 | 18.2 / 17.0 ms |
| permission dialog "Don't allow" | 8/18 / 0/18 | 3.4 / 752.7 ms | 0/30 / 0/30 | 8.8 / 96.4 ms | 0/30 / 0/30 | 9.2 / 10.2 ms |
| "Sheet two" in the sliding sheet | 0/18 / 0/18 | 3.8 / 235.4 ms | 0/30 / 0/30 | 15.0 / 99.0 ms | 0/30 / 0/30 | 15.9 / 15.8 ms |

- With 3 s of think time the wait adds -1.3 to +1.0 ms (difference of medians) and no tap failed
  in either arm (0/120 each). With 1 s it adds nothing to the reopen row (+0.4 ms) but 84-91 ms to
  the other three, and again no tap failed in either arm (0/120 each): at 1 s and more the taps
  without the wait did not fail, so the wait prevented no failure there. Its benefit is confined
  to taps sent within a fraction of a second of the change (0 ms: 44/72 failed off, 0/72 on).
- Why 1 s still costs: the wait trusts a cached read only after 500 ms without a posted frame.
  In 15 traced taps at 1 s (a build with extra log lines, n=5 per row) the guest had posted a
  frame 0-390 ms before the list, permission and sheet taps (some 600-1000 ms after the call
  that opened them), so they took the rest path: two reads, the second from the app, 91-122 ms.
  Before the reopen taps the last frame was 747-833 ms old: one cached read, 3-8 ms. What posts
  those late frames was not identified.

### Logs and crashes (2026-11-07)

`logs`, `crashes` and the crash notice of [CAPABILITIES.md](CAPABILITIES.md#logs-and-crashes)
(`internal/android/logs.go`, `crashes.go`, `logcat.go`). One `logcat -B` per device, opened
through the adb server's `exec:` service (no `adb` process on the Mac), streams logd's `main`,
`system`, `crash` and `events` buffers into the daemon as binary records: the logger_entry
header gives each record's pid, tid, uid, buffer and guest time, and a message logged in one
call (a stack trace) stays one record.

- **Lines** go into two rings of 2 MiB each (by the size `logs` counts): lines of user apps
  (app uids that are not system packages, read once with `cmd package list packages -U -s`)
  and all others, so system chatter cannot push an app's lines out. `logs <app>` matches the
  package's uid, so all its processes (`:remote` too); a package that shares a system uid
  (Settings) is matched by process name. Process names come from the process list read at
  capture start, ActivityManager's `am_proc_start`, and a process-list read at most every 2 s
  for a process the stream has not named. Levels map V D I W E F to verbose ... fatal
  (`Log.wtf` is logged at E on this image).
- **Crash records** come from what the system already logs:

  | kind | first evidence (the record's time) | completed by |
  |---|---|---|
  | Java/Kotlin crash | AndroidRuntime's `FATAL EXCEPTION` in the crash buffer, logged by the app's own handler | `am_crash` (2 ms later); exception and reason are the root cause's, as ActivityManager reports them; frames are the root cause's stack, completed with the frames `... N more` shares with the exception that wraps it |
  | native crash | libc's `Fatal signal 11 (SIGSEGV), code 1 (SEGV_MAPERR), fault addr 0x0 in tid ...`, logged by the crashing process before `crash_dump` runs | crash_dump's tombstone (tag `DEBUG`: `Cause:` or `Abort message:`, the crashing thread's backtrace), `tombstoned`'s path, then `am_crash` "Native crash" from system_server (0.8 s after the signal in the first look) |
  | ANR | `am_anr` (package, reason) | the main thread's stack from the dropbox entry `data_app_anr`, read by `crashes` |
  | killed | lmkd's `Kill '<process>' (pid), uid N, oom_score_adj ...`, or `am_kill` for excessive resource use or by the freezer | - |
  | exited | `am_proc_died` of a user app's process with no crash, ANR or kill before it | a crash that is recorded later for the same pid (kept at the exit's time) |

  A force-stop (`terminate`, Settings, `am force-stop`), cache trimming and a removed task log
  `am_kill` with their reason and no `am_proc_died`: they are not reports. agemu also marks
  its own exits (terminate, uninstall, clear-data, a permission revoke or reset): the app's
  processes alive at the call are marked, and their deaths within 30 s are not reports. A
  process the app starts later is reported as usual (an earlier build dropped every death of the
  app for 3 s after the call, which hid a real `System.exit` of the app relaunched a second after
  a terminate: 3 of 3 missed). A crash record (AndroidRuntime's or libc's line) is made even for
  a marked process.

  **Whose records count.** Any app can write any tag and text to the main and crash buffers and
  any event tag (`EventLog.writeEvent`), but logd stamps each record with its writer's pid and
  uid. So: ActivityManager's events count only from system_server (uid 1000); lmkd's and
  tombstoned's lines only from a system uid (below 10000); AndroidRuntime's `FATAL EXCEPTION` and
  libc's `Fatal signal` only from the crash buffer and only for the process that wrote them (a
  report naming another pid is dropped); crash_dump's tombstone only for a process of the uid
  that wrote it. A record's app is the writer's package: a process name that claims the package of
  another uid is not believed. (An earlier build took the pid and process from the message text,
  so another app could make agemu report that the app under test crashed, was killed or exited;
  a unit test now writes each of those records from another uid and checks the app's state.)
- **The watcher** (`AppState`) answers from memory: the app's live processes (the process list
  at capture start, then every start and death in logd's order, backfilled ones included) minus
  the ones with a crash record, and the newest record since the launch. While a capture is still
  starting it answers "running, no crash" at once, so a crash during the first launch through
  agemu is told by the next call, not by the launch's own result.
- **Stream or dumps.** The capture follows logd with a live stream, and switches to a dump
  (`logcat -d -T` from its newest record) every 250 ms once the device has logged more than 100
  records in each of 3 consecutive seconds; it goes back to the stream when a dump brings fewer
  than 10 new records. Measured below: a live stream costs the emulator per record. A cold
  launch alone logs over 100 records in its first second; the builds measured below switched
  after one such second, which delayed the notice of a crash right after a launch to 11-255 ms.
  The merged build waits for a sustained rate (see "Crash to notice").
- **Lifecycle.** The capture starts when a `launch` or `open` has returned (not waited for) or
  at the first `logs` or `crashes` (waited for, at most 10 s), and backfills from logd's buffer with
  `logcat -d -T`: from the start of the last snapshot load into this emulator process (kept in
  `$AGEMU_HOME/android/<avd>.load.json`, so a restarted daemon finds it), at most 10 minutes
  back. Older lines count as not captured (`partial`). A snapshot load (reset, standby) and
  shutdown stop it: the restored logd holds the baseline's lines. A broken stream is reopened
  from its newest record, and the records of that millisecond it sends again are dropped by key.
- **The guest's clock.** Times are kept as the guest stamped them and converted when a result
  is made, with an estimate of the Mac's clock minus the guest's that is an upper bound: a
  guest timestamp seen at Mac time t was taken at or before t. Three `date` round trips give
  the first bound; every record bounds it again as it arrives, so it settles at the stream's
  delivery latency above the true offset (the smallest bound of the last one to two minutes,
  which follows a drift). A converted time is never before the event. An earlier build used the
  midpoint of the fastest `date` round trip (error up to half of its 20-30 ms): it put Java
  crashes, which come about 5 ms after the request that causes them, up to 3 ms before that
  request, and the daemon, which reports only crashes after the launch or link that started the
  watch, missed 10 of 11 Java crashes, 8 of 11 native crashes and 1 of 11 exits (0 of 11
  aborts, which come 50 ms later). Measured offsets (the guest behind the Mac): 72 ms
  after a cold boot's in-place baseline load in the first look, 0.40-1.09 s after a reset,
  standby or power-off boot in later runs.

Measured on agemu-lab with the probe app's lab screen (`testapps/android`, `LabActivity`:
buttons and `agemulab://<action>` links for each level, a Java exception, a null write and an
abort in `libprobe.so`, an ANR, `System.exit`, a `:remote` process and log bursts), host load
average 9-15 unless noted:

**Crash to notice.** Per trial: terminate the app, cold-launch the lab screen, wait for its
"lab: ready" line and 0.6 s more, trigger the crash with `open`, then poll a call that takes no
device lock (`logs --limit 1`) every 2 ms until a response carries the notice. "After the
request" is the Mac's own clock from sending the link to the notice: an upper bound of
crash-to-notice, since the app crashes after it gets the link. Three builds ran this procedure:

| kind | always streaming (n=10 each, 50 of 50 told) | switch after one busy second (n=3 each, 12 of 12) | merged build, 5 s quiet before each trial (n=5 each, 20 of 20) | merged build, trials back to back (n=5 each, 20 of 20) |
|---|---|---|---|---|
| Java exception | 10.1 (8.0-13.7) | 55.5 | 10.3 (9.1-21.8) | 13.0 (10.4-279.4) |
| native SIGSEGV | 8.8 (7.5-20.0) | 53.4 | 10.3 (7.2-11.5) | 73.7 (11.5-168.5) |
| native abort | 60.3 (55.9-63.3) | 227.1 | 62.1 (54.6-67.5) | 238.8 (59.0-345.0) |
| `System.exit(0)` | 15.1 (11.7-20.0) | 238.6 | 19.2 (16.0-22.6) | 147.8 (17.7-201.2) |
| ANR | 3.1 (1.1-9.8) after `am_anr` (15,535 ms after the request) | not run | 2.9-5.9 after `am_anr` (n=3; 15,529-15,551 ms after the request) | not run |

Request to notice in ms, medians (ranges). The second build's trials ranged 11-255 ms over all
kinds: the cold launch had switched the capture to dumps, so a crash waited for the next one.
The merged build (same capture code as measured, load 9-23) keeps the stream through a cold
launch: with 5 s between trials every notice came from the stream (the daemon's trace showed no
switch during them). Back to back, a crash, its handling (a native crash's tombstone, dropbox,
ActivityManager), the terminate and the next cold launch kept the device over 100 records a
second for 3 s, so the next one or two trials ran in dumps (the trace showed the switches);
an agent that crashes the app again within seconds after a crash meets that.

Other columns of the always-streaming run: the crash's record to the notice took 3.2-4.2 ms
(medians), and frames came in `crashes` at once for a Java crash (7.2 ms after it; 3 in the
notice, 12 in the report), 121 ms (110-248) after a SIGSEGV and 120 ms (111-128) after an abort
(the tombstone; ART's aborter dumps every thread for 49 ms before the signal). For an ANR the
dropbox entry with the main thread's stack came for 6 of 10 (Android keeps at most 6 per process
in 10 minutes), 1.07 s after `am_anr`: that delay was agemu's own, since `crashes` looked in
dropbox at most once a second and its first look came before the entry was written. It now
looks again after 150 ms (for up to 30 s after the ANR): on the merged build the stack came
621-786 ms after `am_anr` (3 of 3, 16 frames each, `crashes` polled every 50 ms). In the merged
build's quiet trials the frames came 7.5 ms (4.8-18.0) after a Java crash, 126 ms (118-164)
after a SIGSEGV and 116 ms (113-124) after an abort.

The ANR's 15.5 s is the system's own timeout: input dispatch waits 5 s × `ro.hw_timeout_multiplier`
(3 on this emulator image) before `am_anr`. "Crash's record → notice" uses the converted guest
time, which may be late by the bound's slack, so it is a lower bound of the latency, and
"request → notice" an upper one. The app's own line logged just before the crash, converted the
same way, came 4.4-11.3 ms after the request in every trial (none before it), so a converted
time was at most that late. Every report named
the app's function in its frames (`LabActivity.crashJava`, `probe_null_write` in
`libprobe.so`, `abortNative`, `LabActivity.act` for the ANR); every failing tap after a
crash or exit answered `app_crashed`.

**What capture costs.** Capture on against off (`AGEMU_ANDROID_LOGS=0`), the daemon restarted
for each 45 s window, ABBA, 4 windows per arm; the lab screen in front, idle, or the app logging
500 lines/s (`agemulab://spam`, one `Log.i` per line on a background thread). CPU is % of one
core (`.lab/meter.py`: the emulator process, which holds all guest work; the daemon; the adb
server, which another session's emulator also uses), medians with ranges:

| | emulator, off | emulator, on | daemon, on (off) | adb server, on | daemon footprint, on (off) |
|---|---|---|---|---|---|
| idle (stream) | 5.30 (5.00-5.50) | 5.25 (5.00-5.50) | 0.02 (0.02) | 0.02 | 78.7 (66.7) MB |
| 500 lines/s, record-by-record stream (previous build) | 13.8 (12.5-15.2) | 58.7 (54.9-59.3) | 1.76 (0.02) | 2.2 | 84.4 (68.9) MB |
| 500 lines/s, dumps every 250 ms (this build) | 12.6 (12.1-13.2) | 20.1 (20.0-20.2) | 0.27 (0.02) | 0.13 | 82.8 (66.7) MB |

Where the stream's cost went (500 lines/s, 25 s windows, n=2 each, interleaved): no reader
12.2-14.5%; a reader inside the guest writing to /dev/null 19.4-21.7%; the adb `exec:` stream
56.2-59.4%; `logcat | nc 10.0.2.2` over the emulator's user-mode TCP 45.1-48.8%; a dump every
250 ms 19.5-20.6%. Inside the guest, the stream cost adbd 5.2%, logd 1.2% and logcat 0.5% of a
core: most of it is the emulator moving one small packet per record out of the VM. So the
capture dumps in bursts while the device keeps logging over 100 records a second; in dumps,
logd spends about 5.8% of a core finding each dump's start time in its compressed buffer.
Not measured: the cost between about 40 and 100 records a second, where the stream stays on.
From the 500 lines/s rows (+45 points for the stream), a stream costs roughly 0.09 points of a
core per record a second, so up to about 9 points just below the switch (an estimate). How many
records an idle agemu-lab logs a second was not measured either; the idle row shows no cost.

**While the device logs heavily** the capture reads logd in dumps, so a crash waits for the next
one: with the probe logging 500 lines/s, a Java crash's notice came 218.6 ms (49.7-290.0) after
the request, n=10, 0 missed (a build that switched after one busy second), and the failing tap
after it answered `app_crashed` 10 of 10 times. On the merged build, with the crash 4.5 s into
the 500 lines/s (after the switch): 121.4 ms (25.2-143.2), n=5, 5 of 5 told, every failing tap
`app_crashed`. The merged build streams the first 2-3 s of such a rate before it switches, which
the cost table's 45 s windows did not include (they ran on the one-second build).

**A failing action after a crash** (tap "Crash Java" by its coordinates, then
`wait --text "Log levels" --timeout-ms 10000`, then `tap --text "Log levels"`; capture on and
off alternating, the daemon restarted for each, n=5 each): with capture the wait stopped with
`app_crashed` after 5.0 s (5004-5006 ms) and the tap answered `app_crashed` (`no element matches
{...} (dev.agemu.probe crashed at ...: java.lang.IllegalStateException: probe crash #1; it is
not running; crashes has the report)`); without, the wait timed out at 10.0 s and the tap said
only `no element matches`. The 5 s were the first UI tree read after the crash: the agent's
`getRoot()` on the dying app's window waits out Android's 5 s accessibility timeout (`ui` right
after a crash: 5008 ms, then 2-18 ms), and the daemon's wait looked at the crash only after each
read. The merged build asks the watcher (in memory) before each read and every 25 ms while one
runs; once the crash shows, that read gets 300 ms more (an element still on screen, such as a
system dialog, is found; a read held by the dying app is not waited for), and a cancelled read
now ends the agent call instead of waiting out its deadline. Same procedure, merged build, n=5:
the wait answered `app_crashed` after 326.2 ms (325.9-352.9). The agent's own read still ran
on the device until the timeout, so the next call that reads the tree (here `tap --text`) took
4,693 ms (4,667-4,711) and answered `app_crashed`: the 5 s moved from the wait to the next
tree read. (Two intermediate builds, n=5 each: a check before each read only, 5,079-5,087 ms in
4 trials, the fifth wait found the screen before the crash; watching during the read without
ending the agent call, 5,004-5,015 ms.) Calls that do not read the tree (`logs`, `crashes`,
`screenshot`, a tap by coordinates) carry the notice within milliseconds. Round 5 removed the
read's wait on the device (next paragraphs).

**The first tree read after a crash** (round 5). A window's content comes from its app:
`getRoot()` sends the request to the app's process and waits up to 5 s for the answer. A process
that is crashing when the request reaches it never answers (a read after it is dead fails at
once), and every tree read queued behind it in the agent waited too. Traced on agemu-lab (one
crash per kind, the process lists read every 10-20 ms): after a Java crash ActivityManager listed
the process as crashing within about 40 ms of the trigger and left it out of its running
processes; after a native crash or `abort()` the process looked normal until `crash_dump`
stopped it (thread state `t`, `TracerPid` = crash_dump64, roughly 0.1 s after the signal), and
ActivityManager logged the crash only after the tombstone was written (309 ms after the signal
in one logcat trace); `System.exit` left a zombie within about 25 ms. So the agent
(`android-agent/src/agemu/Windows.java`, rules in `Owners.java`) reads each window's content on a
reader thread and, while the read has not answered, checks every 25 ms whether the window's
process is gone:
- the process is the top activity's process of the window's task (an activity's window, its
  dialogs and pop-ups), unless the window's content came from another package the last time it
  was read (a window below the top activity, such as an app under the permission dialog Android
  starts in its task); then, and for a window without a task, any process of that package counts;
- it is gone when ActivityManager does not list it as running (it leaves out crashing processes)
  and does not list it as not responding, or when `/proc` shows it as a zombie or stopped by
  `crash_dump`. When the lists cannot be read, or the window's owner cannot be told, the read is
  waited for as before. An ANR keeps the wait only once ActivityManager has filed its report:
  Android marks the app not responding (which drops it from the running list) before it dumps
  the ANR's stack traces, and files the report after the dump, so for the dump (about 0.6-0.8 s
  here) a live, not-responding app counts as gone and a pending read of its window is dropped
  (no on-device ANR trial was run; read from AOSP's `ProcessErrorStateRecord`). Round 4 also
  showed no content for such a window unless the app recovered within 5 s;
- a gone window is listed without content (as a window whose root cannot be read), and the agent
  logs it. The read left behind blocks only its thread (each thread has its own accessibility
  client) until Android's 5 s pass;
- a tree read right after a launch no longer waits up to 3 s for the window of an app that has no
  live process (it crashed right after it started), and does not take the home screen, which
  shows for a moment while a crashed app's task closes, or a window that is still off screen
  (sliding in) for the launched app's window; `launch` reads the crashed app's package (learned
  from its last read) as the app that was in front.

The rules are plain Java, tested on the host (`android-agent/test/agemu/OwnersTest.java`, run by
`go test ./internal/android/` when a JDK is installed; skipped otherwise).

Measured against the merged build (agent ad4b3b73), ABBA over 4 rounds (r4 build against this
one, the daemon restarted from each build per block, which replaced the agent on the device),
probe app crashed by the kind's button or link, 336 trials at load 3.6-7.6:

| after the crash (n per kind and build) | merged build | this build |
|---|---|---|
| first `ui` once the crash notice came (polling `logs`) (12) | native 5,006 ms (5,003-5,015), abort 5,005 (5,003-5,009), Java: 6 of 12 reads 5,005-5,013 ms, the other 6 2.1-9.6 ms, exit 4.1 (2.4-9.3) | native 113 ms (108-119), abort 110 (87-141), Java 8.2 (4.0-39.4), exit 3.4 (2.1-7.9) |
| first `ui` after a crash right after the start (`open_url` of the crash link, app not running) (12) | 3,029.8-3,031.3 ms (medians per kind; 3,020-3,040) | 33.5-36.0 ms (31.0-54.3) |
| round 4's sequence: `wait` for "Log levels" after the crash (8) | `app_crashed` after 325.6-377.3 ms (medians; 30/32, two Java waits found the label before the crash) | `app_crashed` after 29.9-168.8 ms (32/32) |
| ... then the next `ui` (8) | 4,651.9-4,711.2 ms (medians per kind) | 2.8-5.3 ms |
| `launch` Settings once the notice came (8) | native 5,054 ms, abort 5,050, Java 66 (11-5,014), exit 51 | 50.6-159.6 ms (medians per kind) |
| ... then `ui`: Settings' window with its content (8) | 21/32 (exit 0/8, Java 5/8: the screen read had no app), 141-165 ms (medians per kind) | 32/32, 169-369 ms (medians per kind; up to 583 ms while Settings slid in) |

- **What the fast reads show.** In the notice row both builds read the same thing: the crashed
  app's window without content. In the start-up crash and the wait rows they do not: round 4's
  slow read (3.0 s, 4.7 s later) showed the screen the device had settled on (Settings with its
  content 48 of 48 in the start-up flow; Settings in 28 of 32 after the wait), while the fast
  read shows the screen in transition (no application window in 48 of 48, also a second read at
  about 35 ms; after the wait, the dying window without content or no app window). How long
  until a read shows the screen the device landed on was not measured: read again (or `wait`
  for the expected element). The Settings launch's first read is slower than before (169-369
  against 141-165 ms) because it now waits for Settings to slide in instead of reading the
  screen before it.
- The status bar (system UI) was in every first read, both builds (334 trials; 2 more, of this
  build, failed before the crash: a `launch` behind a "keeps stopping" dialog timed out). Its
  read answers within the 25 ms check, so the rule is never consulted for it: this shows the
  read path still works, not that the rule spares every live window. The cases where it could
  misjudge a live window were not run: the ANR dump (above), a never-read window in a task whose
  top process died, several processes in one task, a crash followed at once by a relaunch.
- Settings in the next read 21/32 -> 32/32 comes from the new launch rules (the home screen and
  an off-screen window are not taken for the launched app's), not from the skip. `app_crashed`
  30/32 -> 32/32 in the wait row is the test's race (the label waited for is on screen before
  the crash), not an effect.
- The crash dialog ("keeps stopping", a system_server window) could not be provoked reliably:
  Android showed it only for some crashes, and a launch behind it timed out. The tree had it in
  three trials: twice in the merged build's 5.0 s read, once 154 ms after the crash with an
  earlier build of this change (same rules). Its owner, the system process, is always running,
  so the check never applies to it.
- The native and abort reads end once `crash_dump` has stopped the process (above): that is the
  110-113 ms. The reads after a Java crash or an exit end at the first check or need none.
- A read that answers at once pays for the hand-over to the reader thread: `ui` on the probe's
  lab screen 5.7 -> 6.3 ms and on Settings 5.8 -> 6.8 ms (medians, n=40 each, alternating).
- The alternated runs used an earlier jar of this change (1f2a7b7d); the lane's final jar was
  checked with 2-3 trials per kind and flow (111.1/109.6/7.3/5.1 ms in the notice row; next app
  8 of 8). Round 5's merge rebuilt the jar again with comment, log-line and reflection-cache
  changes only (e8c984c1). While a read is held, each 25 ms check makes 3 binder calls to
  system_server (tasks, running processes, processes in error); that load was not measured.
- Not covered: a window without a task whose content was never read before its app crashed (it
  is waited for as before); typing (`type` reads the focused field from the app) right after a
  crash; a crash that is immediately followed by a relaunch of the same app (the new process
  keeps the old window's read waiting).

**Reading lines.** `logs` against `adb logcat -d` with the same filter, both returning 100
lines, interleaved, n=15 each, load 10.2-10.6. `logcat -t 100` takes the newest 100 lines of the
whole buffer before `--uid` and `-e` filter them, so the baseline reads the filtered dump and
keeps its newest 100 (on the Mac, included in the time):

| lines | `adb logcat -d` | agemu CLI | agemu daemon |
|---|---|---|---|
| the app's newest 100 (`--uid`) | 252.1 ms (229.9-346.3) | 8.2 (7.8-12.3) | 0.56 (0.47-3.36) |
| the app's warnings and worse (`--uid '*:W'`) | 223.2 (202.9-262.7) | 8.6 (6.9-22.7) | 0.48 (0.40-1.08) |
| the app's lines containing a text (`--uid -e`) | 197.5 (185.0-237.4) | 12.1 (10.3-15.5) | 3.83 (3.39-5.44) |
| the device's newest 100 (`-t 100`) | 42.7 (40.9-50.1) | 9.1 (8.1-15.1) | 1.38 (0.76-3.65) |

The rings held about 7,500 lines. Two earlier runs of this comparison are in the runs but not
used: the first's adb arms used `-t 100` with the filters (0 lines) and its text filter matched
nothing; the second's agemu arms kept the default 16 KiB bound (about 40 device lines).

**Completeness.** agemu holds what logd holds: for a 5,000- or 10,000-line burst that the app
logged in 5-17 ms, agemu had exactly the lines logd kept, 5 of 5 (2,562 to 9,227), and 4 of 4
in the capture-on runs below. logd itself does not keep such a burst: its intake socket drops
lines before any reader sees them (logd kept 51-92% of a burst). At a steady 500 lines/s for 8 s (4,000 lines), logd held all
4,000 in 4 of 4 runs, capture on or off, and agemu all 4,000 in both capture-on runs.

**Fidelity**, capture on against off (`AGEMU_ANDROID_LOGS=0`), the daemon restarted per arm,
interleaved, 4 arms each:
- What the app logs is unchanged: for one `log-levels` action logd held the same six lines and
  levels (V D I W E E; `Log.wtf` is logged at E on this image) in all 8 arms. Capture sets no
  property, buffer size or log level.
- An overflowing burst loses more lines in logd with a reader: of 5,000 lines logged in 6-16 ms,
  logd kept 2,073-4,129 with capture on (median 3,153) against 3,661-4,997 off (median 4,618).
  A live reader makes logd wake for every record while the app writes. With n=4 per arm this is
  borderline (Mann-Whitney U=1, two-sided p≈0.06). The app's own time for the burst: pooling all
  five runs (20 bursts per arm), a median 7.54 ms with capture on against 6.43 ms off (U=293 of
  400, p≈0.01), so the app's `Log.i` calls are measurably slower during a burst while the stream
  reads.
- Calls: the daemon-side time of a UI tree read (medians per arm 1.41-1.59 ms on, 1.41-1.69
  off) and of a tap (0.26-0.32 against 0.28-0.31 ms) did not change with the watcher asking
  after every call.
- Launch: the first launch after a daemon start, which starts the capture, 95-175 ms on against
  111-128 ms off; a cold launch with the capture running 118-217 against 116-214 ms. Two earlier
  builds were slower and were fixed: one started the capture before the launch (its setup and
  backfill ran beside the app's start: 369-763 ms), the next made the daemon's crash check after
  the launch wait for the starting capture (up to its 250 ms bound).

**Across reset, standby, a daemon restart and a power-off boot** (`lifecycle.py`, the probe
reinstalled after each, one round each):16 of 16 checks: after a reset, standby and a
power-off boot, `logs` and `crashes` return nothing from before and the new lines and crash
after; an old cursor answers `partial`; a restarted daemon backfills the lines and the crash
since the reset (from the recorded load) and its cursors stay above the old ones; `logs` on a
device in standby answers that it is shut down; `since launch` holds the app's startup line.
The capture measured the guest clock 0.40-1.09 s behind the Mac's after a snapshot load, and
72 ms after a cold boot's first load.

### Parking (2026-11-07)

`agemu park` pauses the VM in place (gRPC `setVmState PAUSED`) without restoring the baseline
first, so the guest keeps its apps, their processes and the screen; `agemu resume`, or any
other call on the device (`logs` and `crashes` excepted), continues it and waits for the agent,
as a boot from standby does. To the emulator a parked VM and one in standby are both paused; a
marker file (`$AGEMU_HOME/android/<avd>.parked`) tells them apart, also for a restarted
daemon. `devices` lists the device as `parked`. `shutdown` of a parked device resumes it first
and then goes to standby as from a running device.

Measured on agemu-lab with the probe's Park screen (`ParkActivity`: a counter, a 0.5 s ticker
with its largest gap by `currentTimeMillis`, `elapsedRealtime` and `uptimeMillis`, a TCP
connection to a host server at 10.0.2.2 that writes a line a second, and an exact
`ELAPSED_REALTIME_WAKEUP` alarm due 20 s after it was set), 60 s waits, arms interleaved (park,
idle, standby, n=5 each), host load average 3-7 (`.lab/r5/r5-s2-parking`):

| | park (paused in place) | running idle | standby (`shutdown`, then `boot`) | hibernate (snapshot save, exit; start, load) |
|---|---|---|---|---|
| emulator CPU over 52 s | **0.24-0.25 CPU-s** (0.5% of a core) | 8.5-10.8 CPU-s (16-21%) | 0.25-0.27 CPU-s | none (no process) |
| to stop | `agemu park` 8.5 ms (8.0-22.0) | - | baseline load, then pause | save 1.6-3.2 s writing 0.8 GB, exit 0.8-1.8 s (n=4) |
| back to agent-ready, the app on screen | **133 ms** (116-279); `agemu resume` returns after 46 ms (36-81) | first `ui` 5.4 ms (5.0-23.9) | boot 64 ms (55-167), but the app is gone: 1.33 s (1.32-1.51) with its reinstall and start | 2.5-3.3 s (n=4) |
| the app's state | kept, 5 of 5 | kept | lost (baseline) | kept, 4 of 4 |

What the app saw after a park (5 of 60 s):
- **Clocks**: the guest's clocks run on through the pause: the ticker's gap was the park's
  length by the wall clock, `elapsedRealtime` and `uptimeMillis` alike (60.3-60.5 s), and the
  guest clock's offset from the Mac's was the same before and after (within 2 ms).
- **Network**: the TCP connection stayed open (0 reconnects in 5), and the 62-63 lines the
  server wrote during the park arrived on resume.
- **The exact alarm** due 20 s into the park fired on resume, 40.7-40.9 s late (idle: 1-3 ms).
- **No crash of the app**, the tap after the resume landed in 5 of 5. Android's Bluetooth stack
  (`com.google.android.bluetooth`) crashed once ("hci_layer.cc:589 on_hardware_error: Hardware
  Error Event"), at or right after 1 of the 5 resumes. The crash queries covered 90 s, longer
  than the 65 s between trials, so the same crash also falls in the next (idle) trial's window;
  counted once, there was none in the idle or standby windows. The same crash ("on_hardware_error
  ... code 0x42") came once more during round 5's merge e2e, 98 s after a park of 12 ms had
  ended; whether parks cause it is not established. Watchdog lines in the windows were the same kinds as idle (keystore2's idle
  watchdog thread, system_server pausing a HandlerChecker for an install).
- **Memory**: the paused VM's memory stays allocated, but nothing touches it, so the host
  compresses or swaps it out: the emulator's uncompressed resident memory fell from 0.21-0.44
  GB to 0.011-0.038 GB by the end of each 52 s park (running idle: 0.24-0.49 to 0.14-0.35 GB).
  On this host, already swapping, 2.8-3.1 GB of the emulator's 3.1-3.4 GB footprint was
  compressed or swapped out before each wait in both arms (4.1-4.2 of 4.3-4.5 GB in the trials
  below); parking frees most of the rest.
- **When memory ran short** (three iOS simulators booted while the emulator waited, the host's
  free memory down to 0.06-0.08 GB, 3 trials per arm, interleaved): the parked emulator kept
  0.011 GB uncompressed in each (it had 0.16-0.35 GB), the running idle one 0.138-0.144 GB (it
  had 0.17-0.20 GB). After the simulators shut down, the parked device was agent-ready 91-104
  ms after the resume with the app on screen; the idle one's first `ui` took 8.5-16.8 ms.

**Hibernate** (the emulator's own snapshot save of the current state, then exit; resume is a
power-off boot that loads that snapshot in place, as `boot` loads the baseline; done with the
emulator console as a baseline arm, not implemented): with agemu's console port redirection
(its agent socket) inside the snapshot the load failed 3 of 3 times ("load vmstate_slirp_socket
error -22", the device stayed offline); with the redirection deleted before the save, as
agemu's baseline save does, 4 of 4 loaded. The guest's monotonic clocks did not count the time
the emulator was gone (`elapsedRealtime` gap 2.6-3.7 s for 29-31 s of wall time), so an
`elapsedRealtime` alarm fired on time by the guest's clock and 30 s late by the wall clock (the
one seen firing). It
writes about 0.8 GB per hibernate and resumes 20x slower than a park, so agemu parks instead.

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
- **Standby and park keep guest RAM allocated.** A device in standby or parked uses no CPU, and the paused emulator keeps its guest RAM (about 2 GB) on the host, which macOS can compress or swap out when it needs memory (see Parking). Run `shutdown` twice, or set `AGEMU_ANDROID_STANDBY=0`, to free it.
- **Disk.** A baseline snapshot holds the guest RAM (`ram.bin`) and about 23 MB of GPU textures; the AVD's userdata qcow2 grows to about 3 GB. agemu has the emulator save `ram.bin` LZ4-compressed (`ANDROID_SNAPSHOT_COMPRESS=1`); `AGEMU_ANDROID_SNAPSHOT_COMPRESS=0` saves it raw. agemu always sets the emulator's variable: without it, emulator 37.1 picks a format by itself ("Enabling RAM compression: only %u MB of free RAM", "snapshot is on HDD"). A baseline keeps its format until it is retaken. Measured on 2026-11-07 on `agemu-lab` (2 GB guest, host GPU) in blocks that alternated the format (ABBA), each block with its own baseline, loads at least 8 s apart, while other sessions ran Gradle, Metro, a simulator and another emulator (load average 6-52, 6.3-14.6 GB of swap in use):
  - **Disk:** `ram.bin` 0.83-0.89 GB compressed against 2.04-2.05 GB raw (9 and 8 baselines): 1.15-1.23 GB less per AVD by file size, about 1.14-1.22 GB by allocated blocks (the raw file is fully allocated, the compressed one 1.3% above its size). Saving the baseline took 1.8-6.4 s against 5.5-10.3 s (round 1's quiet disk saved raw in 3.1 s), so the disk was shared throughout.
  - **Reset:** 1.75 s against 2.28 s (medians of 12 per format, 8 blocks, the Mac's page cache as it was); every compressed block's median was below every raw block's. The raw load read 2.17 GB from disk every time, at 733-1020 MB/s; the compressed one 0.90 GB (median), less when the page cache held the file. With `ram.bin` dropped from the page cache before every load, so that every load read the SSD: 2.35 s against 2.66 s (medians of 10, 8 more blocks). That one is less clean: the two sets alone gave -8% and -30%, one compressed block's median (2.57 s) was above one raw block's (2.50 s), and the 12% rests partly on two slow raw blocks (4.03 and 3.57 s).
  - **Powered-off boot** (`shutdown` twice, then `boot` to agent-ready): 2.31 s against 2.48 s (8 per format); with `ram.bin` dropped from the page cache, 2.78 s against 3.06 s (12 per format). In the first 4 of those 8 blocks the compressed boots were slower (3.26 against 2.89 s, n=4) at a higher host load (load average 32 against 19), mostly before the snapshot load (the emulator process took 1.25 against 0.94 s to answer gRPC); in the other 4 blocks they were faster (2.56 against 3.13 s, n=8).
  - **Cost:** more emulator CPU per load, as LZ4 decompresses on a thread pool (12 threads in a traced reset): 3.99 against 2.06 CPU-seconds per reset with the page cache as it was, 6.01 against 2.38 with `ram.bin` dropped from it (medians, the emulator's idle CPU over the same time subtracted). A whole powered-off boot to agent-ready used 4.04 against 2.62 and 4.25 against 3.08 CPU-seconds.
  - **Memory:** not measured on 2026-11-07. On 2026-11-06 the emulator's footprint after resets did not change (10 per format); both formats restore every guest page.
  - **Guest:** after each of the 90 resets and boots the guest had finished booting, the restored agent was running, the boot count was unchanged within each baseline and the guest's uptime stayed within 2.7 s of the others from the same baseline (no reboot), the crash buffer of every baseline taken in this run was empty, and `framework.jar`, `services.jar` and the agent's jar had the same MD5 everywhere. A Settings flow (launch, tap by text, back, search, type, wait, screenshot) passed 9 of 9 times (after the first 8 blocks and on the final baseline). `scripts/e2e-android.sh` passed 41/41 on a compressed baseline; an earlier run stopped at check 37, where the crash notice came with the `open` call and the tap after it exited 1 instead of 4 (a race in the script, fixed in round 5's merge; agemu-lab's e2e has only run on compressed baselines). Guest RAM was not compared byte for byte, and these are coarse checks; LZ4 is lossless.
  - **Quiet disk: not measured again.** On 2026-11-06, in the one raw round with a fast disk (its baseline save took 3.1 s), raw resets took 1.16-1.78 s (median 1.36 s), faster than any compressed load measured then (1.61 s at best) and close to the best compressed load of 2026-11-07 (1.41 s, file fully cached). On a quiet disk the gain can vanish or reverse. Both comparisons ran on a shared disk otherwise (2026-11-06: 1.77 s against 2.25 s, 14 loads each in one emulator).
  - **Not measured:** the decompression burst's effect (4-6 CPU-seconds on 12 threads per reset) on other devices running at the same time, and several AVDs resetting at once.
- **Rotation is not handled.** Profiles are portrait; a landscape app or device rotation is not mapped.
- **After an app's crash** (see Logs and crashes): the crashed app's window is listed without
  content while it closes; the first UI tree read after a native crash or `abort()` still takes
  about 0.11 s (until `crash_dump` has stopped the process), and a window without a task whose
  content was never read before the crash is still waited for (Android's 5 s). A fast read right
  after a crash (also after a crash at start-up or a `wait` that answered `app_crashed`) shows
  the screen in transition, not yet the one the device returns to: read again or `wait`. While
  Android dumps an ANR's traces the not-responding app's window is also listed without content
  (see "The first tree read after a crash"); the
  second crash within a minute leaves "keeps stopping" on screen with the crashed process frozen,
  and a `launch` then can fail after 20 s (dismiss it or `terminate` first); an ANR's frames come
  from dropbox, which Android writes for at most 6 ANRs of a process in 10 minutes.
- **Emulator features are part of a snapshot.** Starting the emulator with a `-feature` set that differs from the snapshot's makes it print "different emulator features" and **delete** the snapshot, then cold boot. Do not pass `-feature` flags through `AGEMU_ANDROID_EMULATOR_FLAGS`.
- **The measurements ran on a shared, often saturated host.** The M2 Pro with 16 GB shared the machine with another emulator, iOS simulators, a browser and other sessions; load averages ranged from 3 to 450 and swap was nearly full during this round. Absolute numbers are noisy (see the p95 columns and the run times listed with the table); both sides of each row ran back to back under the same conditions.
