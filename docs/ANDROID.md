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
- **`boot`** runs `emulator -avd <name> -port 5580 -grpc 8580 -no-window -no-audio -no-boot-anim -gpu host -memory 2048 -lowram -no-snapshot-save` plus either `-snapshot agemu-baseline` or `-no-snapshot-load` for a cold boot.
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
re-laid out at the new size. No reboot is needed. Typical switch time is 0.3–2 s, most of
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

BENCH_TABLE_PLACEHOLDER

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
- **The measurements ran on a saturated host.** The M2 Pro with 16 GB shared the machine with another emulator, iOS simulators, a browser and other sessions, at load averages of 50–340. Absolute numbers are noisy (see the p95 columns); both sides of each row ran back to back under the same conditions.
