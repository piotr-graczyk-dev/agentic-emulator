# Agent race: agemu against adb, agent-device and Argent

One 18-step task in the [Agemu Shop](../shop) app, driven the way an agent's tool calls drive it,
by every tool stack on the same Mac: on Android the standard emulator with `adb`, agent-device and
Argent, against agemu; on iOS a standard Simulator with agent-device and Argent, against agemu.
Every run is recorded; the median run of each stack is rendered side by side, per platform and in
one montage.

| Android | iOS |
|---|---|
| [![Android race](out/teaser.gif)](out/android/race.mp4) | [![iOS race](out/ios/race.gif)](out/ios/race.mp4) |

**Videos:** [montage, both platforms (67 s)](out/montage.mp4) ·
[Android race, 4 stacks, real time](out/android/race.mp4) ·
[iOS race, 3 stacks, real time](out/ios/race.mp4)

The task: open the app, sign in, scroll the catalog to Bath Towel, open it, add it to the cart, go
back, search for "lamp", open Desk Lamp, set the quantity to 2, add it, open the cart, check that
the total is $91.98, check out with a shipping address, place the order, read the order number.
Every step waits for the screen it leads to; four steps also take a screenshot.

## Results

2026-11-08, one Apple M2 Pro Mac (16 GB) that other sessions used at the same time (load average
3-15). Three runs per stack, the stacks' order rotated every round. All 21 runs completed the task.
Every run, with its host load, is in `out/<platform>/results.md`; each run's tool calls, frame
index and CPU samples are in `out/<platform>/runs/` (the frames are not committed).

### Android

| | standard emulator + `adb` | agent-device 0.21.36 | Argent 0.32.0 | agemu |
|---|---|---|---|---|
| task time | 73.3-74.9 s | 24.0-24.8 s | 29.4-30.4 s | **10.4-10.7 s** |
| agemu is faster by (medians) | 7.0× | 2.4× | 2.8× | |
| tool calls | 64 | 33 | 66 | 34 |
| median tool call | 1.91-1.92 s | 779-801 ms | 326-333 ms | 200-236 ms |
| device CPU for the task | 32-36 CPU-s | 14-15 CPU-s | 21-23 CPU-s | 12 CPU-s |
| the tool's host processes, CPU | 0.1 CPU-s | 2.9-3.0 CPU-s | 6.1-6.9 CPU-s | 0.3-0.4 CPU-s |

### iOS

| | agent-device 0.21.36 | Argent 0.32.0 | agemu |
|---|---|---|---|
| task time | 35.0-37.3 s | 17.8-18.5 s | **16.5-17.0 s** |
| agemu is faster by (medians) | 2.2× | 1.1× | |
| tool calls | 26 | 66 | 35 |
| median tool call | 1.33-1.37 s | 171-181 ms | 414-449 ms |
| device CPU for the task | 23-25 CPU-s | 10-11 CPU-s | 15 CPU-s |
| simulator memory (summed `phys_footprint`) | 4.33-4.60 GB | 4.28-4.55 GB | **2.06-2.33 GB** |
| the tool's host processes, CPU | 7.0-7.6 CPU-s | 4.2-4.5 CPU-s | 1.9-2.0 CPU-s |

What the numbers show:

- **Android: reading the screen is the difference.** The `adb` arm spends 69-70 of its 73-75 s in
  33 `uiautomator dump`s (median 2.0 s each). Argent's `describe` on Android also runs a uiautomator
  dump, through its own on-device service (medians 0.29-0.44 s); agent-device reads through its own helper
  app, and its `--settle` waits for a quiet window after every action. agemu's taps wait for their
  effect in the same call, from a warm on-device agent.
- **iOS: Argent is close; most of the time is the app's.** Argent taps by coordinates with no
  check that the screen has stopped moving, and scrolls without momentum. agemu loses its lead in
  three places: the tap right after scrolling waits 2.1-2.2 s for its momentum scroll to come to
  rest (agemu has no momentum-free swipe), the first paste into a field took 1.1-1.5 s, and every
  selector tap waits for a still screen. It has half the calls of Argent (35 against 66); with
  model think time each call is one more agent turn.
- **iOS memory.** agemu's lean simulator held about half the memory of the standard one, as in
  agemu's [own measurements](../../README.md#ios). On Android the emulator's `phys_footprint`
  moved between series of the same device (agemu-lab measured 3.1, 3.4 and 4.2 GB in three
  series on the same day), so the Android memory is not compared here.
- **Host processes.** On iOS, agent-device's XCTest runner and Argent's accessibility service run
  inside the simulator, so their CPU counts as the device's.

## How it was run

The plan is the same code for every stack ([plan.go](plan.go)); only the tools differ. Each driver
uses its tool as that tool's documentation tells an agent to ([drivers.go](drivers.go),
[drivers_tools.go](drivers_tools.go)):

| agent step | `adb` (Android) | agent-device | Argent | agemu |
|---|---|---|---|---|
| launch | `am start -W` | `open --relaunch` | `launch-app` | `launch` |
| find + tap | `uiautomator dump`, `input tap` at the centre | `press 'label="…"' --settle` | `describe`, `gesture-tap` at the frame's centre | `tap --text "…"` |
| wait for the next screen | dump again until the text is there | the settled diff, else `wait text` | `await-ui-element` | in the same call: `--wait-text` |
| enter text | tap, `input text` | `fill 'label="…"' --settle` | tap, `fill` | tap, `type` |
| scroll to an item | `input swipe` + dump, until it is on screen | `scroll down --until 'label="…"'` | `gesture-swipe` without momentum + `describe` | `swipe --ui`, until it is on screen |
| read a value | dump | `snapshot --force-full` | `describe` | `ui` |
| screenshot | `exec-out screencap` | `screenshot` | `screenshot` | `screenshot` |

- No model and no think time: the next call starts when the previous one returns, so the times are
  the tools' own. Each call is one process, as an agent with a shell runs it; agent-device and
  Argent forward it to their daemons.
- When a tap or fill finds no element, every driver reads the screen once more and retries once
  (agent-device with the ref its `snapshot -i` shows); the failed call's time counts.
- **Devices.** Android: the standard `Pixel_9_Pro` AVD started as a developer starts it
  (`emulator -avd Pixel_9_Pro`, window, 1280×2856, 2 GB guest; `-grpc` for the recording), and
  agemu's `agemu-lab` (1080×2424, 2 GB guest, no window, lean). iOS: a standard iPhone 17 Pro
  Simulator, and agemu's lean `agemu-iphone-17-pro`. Same Android 37.1 and iOS 26.5 images.
- While one device ran, the other was paused where that is possible (emulator console
  `avd stop`, `agemu park`); a standard Simulator cannot be paused, so it stayed booted and idle
  during the agemu runs. Between stacks, agent-device's and Argent's daemons and on-device helpers
  were stopped (they hold Android's single UiAutomation connection).
- **Untimed setup per run:** each tool's daemon warmed up, the app launched once and stopped,
  the home screen in front.
- **Recording**, the same for every stack and on the host: Android, the emulator's gRPC screenshot
  stream; iOS, `simctl io recordVideo`. CPU from `ps` every 0.5 s, memory with `footprint` every
  2 s (iOS: summed over the simulator's process tree).

### What had to change to make the race fair

- **The apps use visible text for every control** (`+` and `Quantity: 2`, not content
  descriptions; SwiftUI text fields get an accessibility label). agent-device on Android shows a
  button's text rather than its content description, and its `label=` selector on iOS does not
  match a SwiftUI field's placeholder.
- **The catalog is long enough that no stack scrolls to its end.** agemu's taps right after a
  swipe that reaches the end of the list were lost on both platforms (Android: 4 of 4 race runs;
  iOS: the one run tried): the list's overscroll or bounce takes the touch, and agemu's still-screen
  check does not wait for it. That is an open agemu bug.
- **Gboard's stylus handwriting is off on the standard emulator.** After some runs, Gboard
  switched to its stylus mode and showed a tutorial sheet over the app; the cause was not
  pinned down (likely a tool's injected touches), and every later arm would have been blocked.

## Run it

```sh
make build
npm install --prefix ~/.cache/agemu-race-tools @swmansion/argent@0.32.0 agent-device@0.21.36
emulator -avd Pixel_9_Pro -port 5600 -grpc 8600 &     # the standard Android emulator
agemu boot android:agemu-lab
examples/agent-race/run.sh android                    # 3 rounds, then out/android/race.mp4

xcrun simctl create race-iphone-17-pro com.apple.CoreSimulator.SimDeviceType.iPhone-17-Pro \
  com.apple.CoreSimulator.SimRuntime.iOS-26-5 && xcrun simctl boot race-iphone-17-pro
examples/agent-race/run.sh ios                        # then out/ios/race.mp4, out/montage.mp4
```

`run.sh` builds and installs the apps, rotates the stacks (`ROUNDS`, default 3), writes
`results.md`, `summary.md` ([stats.py](stats.py)) and `picked.json` (the median run of each stack),
and renders with [compose.py](compose.py) (2-4 stacks side by side) and [montage.py](montage.py)
(the montage and the GIF teaser; Pillow through `uv`, and ffmpeg). One run on its own:

```sh
go run ./examples/agent-race run --platform ios --arm argent --device <udid> --out /tmp/race/argent
```
