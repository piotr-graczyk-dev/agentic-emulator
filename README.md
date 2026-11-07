# agentic-emulator (`agemu`)

An agent-first runtime for the Android emulator and the iOS Simulator on Apple Silicon
macOS. It has no window for humans. LLM agents drive devices through a CLI or an MCP server:

- read the UI tree,
- take screenshots,
- tap, swipe, type and press keys,
- launch apps,
- switch between real-device screen profiles,
- reset to a clean state.

All coordinates are screenshot pixels, so the same loop works on both platforms.

**Guest operating systems are stock**, because only the real OS behaves like a real device:
- **Android:** the SDK's arm64 system image running in the SDK emulator (QEMU + Apple
  Hypervisor.framework).
- **iOS:** the Simulator runtime from Xcode, through CoreSimulator.

agemu replaces the **control plane**, which is where standard tooling is slow for agents.
It does this with:
- a long-lived daemon,
- emulator gRPC with a shared-memory frame stream,
- an on-device Android agent holding one warm UiAutomation connection,
- an in-process iOS helper reading the framebuffer IOSurface, sending HID events and
  translating accessibility.

## Results

Speedup of agemu over the standard tools, on the same device. Values are medians on an Apple M2
Pro (16 GB, macOS 26.6), measured on 2026-11-06 with the final code; other apps were running on
the host, with load average 5-150. **CLI** is one `agemu` process per action, as a shell-using
agent runs it. **MCP** is a persistent connection to the daemon, as `agemu mcp` uses it. Raw
tables and JSON: [bench/final/](bench/final/).

- **Android baseline:** `adb`, `uiautomator`, and the emulator's cold boot.
- **iOS baseline:** `simctl`, plus Maestro for tap, type, swipe and UI tree, because `simctl`
  has none of them. A single Maestro call starts a JVM and an XCUITest runner. The "in one
  flow" row spreads that start-up over 10 operations.

| Operation | Android CLI | Android MCP | iOS CLI | iOS MCP |
|---|---|---|---|---|
| screenshot (PNG, full resolution) | **18×** (8.4 ms) | **35×** (4.4 ms) | **18×** (8.3 ms) | **33×** (4.5 ms) |
| screenshot in a non-native profile (Android: resampled) | **15×** | **30×** | n/a | n/a |
| UI tree | **337×** (5.9 ms) | **1051×** (1.9 ms) | **233×** (30 ms) | **309×** (23 ms) |
| tap | **11×** (4.7 ms) | **172×** (0.3 ms) | **1884×**; **380×** in one flow | **24165×**; **1118×** in one flow |
| type text | 1.4–2.4× | 1.7–3.2× | **1588×** | **3322×** |
| key (home) | 5× | **42×** | n/a | n/a |
| swipe: overhead on top of the gesture time | 8.9× | **34×** | **22×** (total) | **23×** (total) |
| boot to agent-ready, device kept ready (standby / booted) | **516×** (24 ms) | **1300×** | **26×** | **332×** |
| reset to a clean state (vs cold boot / `simctl erase`) | 6.4× (2.2 s) | 6.6× | **17×** (2.1 s) | n/a |
| boot from powered off / shut down (cold) | 2.3× (5.5 s) | 2.3× | 0.7× (7.5 s) | n/a |
| launch app | 0.6–0.9× | 1.0–1.1× | 0.9× | 1.0× |
| one agent step: find + tap + wait for next screen + screenshot | 3.6× (1.5 s) | 3.6× | n/a | n/a |

### Where 10× is not reached, and why

- **App launch, and the agent step.** The time goes to the app itself: process start, first
  frame, a screen transition with its animation.
  - agemu makes the same OS call (`startActivityAndWait`, CoreSimulator launch) and adds
    almost nothing.
  - In the agent step, the tool overhead dropped from about 3.7 s to about 0.1 s. Settings
    still needs about 1.4 s, with software rendering, to open the next screen.
- **Cold boot.** Neither platform can avoid the OS boot:
  - Android loads its baseline snapshot in place: 2.4–5.5 s, against a 12 s cold boot.
  - The iOS Simulator has no snapshots. agemu waits until the home screen is drawn, which
    `simctl bootstatus` does not.
  - The fast path is keeping devices ready: 24 ms for Android standby, 4 ms for a booted
    simulator.
- **Android reset.** The floor is the emulator restoring 2 GB of guest RAM (about 2.1 s).
  agemu is at parity with the emulator's own `avd snapshot load` (1.1×), and 6.4× faster
  than a cold boot.
- **Android typing.** agemu sets the focused field's text (`ACTION_SET_TEXT`, as Appium does)
  in 2–9 ms. It then waits until the app's listeners and the keyboard have handled the change,
  so that `type` followed by `ui` is consistent. `adb shell input text` returns before that.
- **Android CLI key: 5×.** Android 17's `input` is cheap (49 ms), and an `agemu` process
  costs about 3–9 ms to start. Over MCP the same key is 42×.
- **Profile switch.** On Android it is not comparable: agemu waits for the app to re-lay out,
  while `wm size` returns immediately. On iOS it is a cold boot of another device type
  (13 s, 0.7×).

### A real app, against agent-device and Argent

The same 23-step flow through a React Native app in development, on one iOS simulator: sign in
with e-mail and one-time code, search, open a result, switch tabs, scroll, sign out. Every step
waits for the screen it leads to. There were 5 interleaved rounds; the values are medians. Method,
per-step times and an earlier series: [docs/IOS.md](docs/IOS.md#a-real-app-against-agent-device-and-argent).

| | agemu (action and wait in one call) | Argent 0.32 | agent-device 0.21 |
|---|---|---|---|
| whole flow | **29.7 s** | 38.8 s (1.31× slower) | 65.5 s (2.21× slower) |
| after the app is up (14-17 s launch excluded) | **15.8 s** | 21.1 s (1.34×) | 47.6 s (3.01×) |
| tool calls (agent turns) | **26** | 51 | 36 |
| one UI tree / one screenshot | 24-122 ms / 18-26 ms | 188-513 ms / 159-212 ms | 304-455 ms / 484-700 ms |

After launch, most of agemu's time is the app's own transitions: a tap returns when the first
changed frame shows, and a wait ends on the first read that has the next screen. Compared with
agemu before this round of work, the flow after launch went from 18.9 s to 15.2 s.

### Lighter devices, against the standard emulators

Each device runs a full guest OS, so most of its memory and CPU is iOS's or Android's, not
agemu's (agemu's daemon and iOS helper take 21 MB together). agemu cuts what an app under test
does not use. All figures were measured on one Apple M2 Pro (16 GB).

**iOS.** These are the summed processes of one simulator; see
[docs/IOS.md](docs/IOS.md#lean-simulators).

| per simulator | all services (Simulator default) | agemu, lean (the previous 32 jobs) |
|---|---|---|
| memory, no app / React Native dev app open | 4.0 GB / 4.8 GB | **2.5 GB / 3.9 GB** |
| idle CPU, no app | 20% of a core | **1.5%** |
| simulator CPU during an app flow right after boot | 148 s | **101 s** |
| cold boot to agent-ready | 9.3 s | 8.4 s |

Three lean simulators at once took 7.7 GB and 19% of one core together.

**Android.** These are the emulator process of one AVD; see
[docs/ANDROID.md](docs/ANDROID.md#host-gpu-and-lean-devices-2026-11-06-evening).

| per emulator | as Android Studio runs it (window, 4 GB guest) | agemu before | agemu now (host GPU, lean) |
|---|---|---|---|
| memory | 7.5 GB | 5.9 GB | **5.4 GB** |
| idle CPU | 86% of a core | 54-79% | **24%** |
| agent step: find + tap + wait for next screen + screenshot | - | 1506 ms | **866 ms**; 358 ms with `AGEMU_ANDROID_ANIMATIONS=0` |
| cold app launch (Settings) | - | 602 ms | **151 ms** |

**Scale.** The floor is the guest OS: about 2.5 GB per idle iOS simulator and 4.3-5.4 GB per
Android emulator (2 GB of guest RAM, plus the emulator and GPU). A hundred devices would need
250-540 GB of memory. A 16 GB Mac that also runs other apps fits 3 lean iOS simulators
(measured together) or, estimated from the per-device figures, 1-2 Android emulators.

## Install

Requirements:
- An Apple Silicon Mac.
- Xcode, with an iOS Simulator runtime.
- Android SDK with the emulator, platform-tools and the arm64 system image
  `system-images;android-37.1;google_apis_playstore_ps16k;arm64-v8a` (override with
  `AGEMU_ANDROID_IMAGE`).
- Go 1.26+.

```sh
make build        # bin/agemu (CLI + MCP), bin/agemud (daemon), bin/agemu-ios-helper, bin/agemu-bench
```

`bin/agemu` is a thin client: it starts `bin/agemud` from the same directory on first use.
Keeping the backends out of the CLI makes it start in about 3 ms, which matters because an
agent runs one process per action. State lives in `$AGEMU_HOME` (default `~/.agemu`).

## Use

```sh
agemu create --platform android --name pixel          # -> android:agemu-pixel
agemu create --platform ios --profile iphone-17-pro    # -> ios:<udid> named agemu-iphone-17-pro
agemu boot android:agemu-pixel                         # warm snapshot: seconds; from standby: ~50 ms
agemu launch com.android.settings
agemu ui                                               # compact outline: [ref] role "text" id=... @(x,y wxh)
agemu tap --text "Network & internet"                  # or: tap 393 614, tap --id ..., --contains ...
agemu wait --text Internet                             # re-reads the UI tree as the screen changes
agemu tap --text "Network & internet" --wait-text Internet --ui   # act, wait and observe in one call
agemu type "hello"                                     # into the focused field
agemu key back                                         # home, back, enter, delete, tab, escape, lock, ...
agemu screenshot -o shot.png                           # pixels = the profile's resolution
agemu profile small-phone                              # or --width 900 --height 1800 --density 360 (Android)
agemu reset                                            # back to the clean baseline
agemu profiles                                         # all real-device profiles
```

With exactly one booted device, `-d <device>` can be left out.

Actions (`tap`, `swipe`, `type`, `key`, `launch`) take `--wait-text/--wait-id/--wait-contains/--wait-role`
(plus `--wait-gone`, `--wait-timeout-ms`) to wait for their effect in the same call, and `--ui` to
also return the screen they end on. One call per agent step instead of two or three.

### MCP

```sh
claude mcp add agemu -- /path/to/bin/agemu mcp
```

The tools are `devices`, `profiles`, `create`, `boot`, `shutdown`, `reset`, `set_profile`,
`screenshot` (returns the image), `ui_tree`, `find`, `tap`, `swipe`, `type_text`, `press_key`,
`install`, `launch`, `terminate` and `wait_for`. All MCP sessions share the one daemon.

### Profiles

| Platform | Profiles | How they switch |
|---|---|---|
| Android | `pixel-9-pro`, `pixel-9`, `galaxy-s24`, `small-phone`, `pixel-fold-inner`, `pixel-tablet`, custom `WxH@dpi` | At runtime with a display size and density override; no reboot. |
| iOS | `iphone-17-pro-max`, `iphone-17-pro`, `iphone-17`, `iphone-air`, `iphone-16e`, `iphone-se-3`, `ipad-mini`, `ipad-pro-11` | The screen is fixed per simulator device type, so agemu switches to an `agemu-<profile>` simulator. |

### Lighter devices

- iOS: agemu turns off 49 launchd jobs an app under test does not use (widgets, Siri, photo
  analysis, asset downloads, Continuity, ...) in its simulators, and stops the wallpaper
  gallery (all but the lock screen's wallpaper) after boot: 1.97 GB 2 minutes after
  agent-ready, against 2.42 GB with the previous lean setting of 32 jobs (medians of 4 boots
  each). `AGEMU_IOS_LEAN=0` keeps the full system.
- Android: `-gpu host` by default, and 14 Google apps are turned off in the baseline (Play
  services and the Play Store stay). `AGEMU_ANDROID_LEAN=0` keeps them;
  `AGEMU_ANDROID_GPU=auto` goes back to software rendering.
- Android: `AGEMU_ANDROID_ANIMATIONS=0` turns system animations off, so screen changes finish
  at once. It is off by default.
- Android: after 30 s without calls agemu stops its frame stream if the screen is still
  changing, and the next call restarts it (`AGEMU_ANDROID_STREAM_IDLE_S`; see
  [docs/ANDROID.md](docs/ANDROID.md#frame-stream-idle-stop-2026-11-06-night)).
- iOS: `AGEMU_IOS_ANIMATIONS=0` shortens UIKit and SwiftUI animations (push, back, tab switch,
  alert, `UIView` and SwiftUI animations; durations and delays x0.05). agemu applies it after
  each boot it makes and before the first launch; apps launched after that use it, SpringBoard
  from the next boot. From a tap to the end of the transition, summed over a test app's screens:
  4.8x less time (SwiftUI) and 7.4x (UIKit); a 15-call agent flow took 6.9 s instead of 13.3 s
  (medians of 5 runs each). Sheets keep their speed and timers do not change. It is off by
  default; see [docs/IOS.md](docs/IOS.md#fast-animations-opt-in).

These are daemon settings: set them where `agemu` first starts the daemon.

### Safety

agemu manages only devices named `agemu-*`. Other emulators and simulators on the machine are
never booted, shut down or driven, so agemu can run next to your own devices and other tools.

## Proof

| What | How to run it |
|---|---|
| End-to-end, Android | `scripts/e2e-android.sh` (20 checks; CLI only) |
| End-to-end, iOS | `scripts/e2e-ios.sh` (109 checks; CLI only; includes a third-party SwiftUI app and iPad) |
| Benchmarks | `bin/agemu-bench --platform android\|ios --device <id> --agemu bin/agemu` |
| Unit tests | `make test` |

The e2e scripts cover Settings navigation by selector, typing, screenshots in several
profiles with checked dimensions, tapping by selector in non-default profiles, and reset.

## Known limitations

- **iOS uses private CoreSimulator, SimulatorKit and AccessibilityPlatformTranslation APIs.**
  They can change with any Xcode release; this version was tested on Xcode 26.6 with iOS 26.5.
- **Android: one UiAutomation client per device.** While agemu's on-device agent runs,
  `uiautomator dump` and UiAutomator-based tests cannot connect. agemu restarts its agent when
  it is evicted.
  - A request sent to an agent that has just died can wait up to 60 s before agemu restarts
    the agent and retries. This was seen once in the final benchmark, which evicts the agent on
    purpose.
- **Android profiles other than the AVD's native one are resampled** from the framebuffer.
  Create the AVD with `--profile <name>` to make that profile pixel-exact.
- **iOS has no snapshots.** Reset is a soft reset: it terminates and uninstalls user apps and
  resets privacy, keychain and pasteboard. Built-in apps' data is not reset.
- **iPad Split View / Slide Over is untested.** `ui` reads the app at the screen centre.
- **Simulator is not a device.** The iOS Simulator runs apps natively on the Mac; GPU,
  sensors and performance differ from an iPhone.

## Documentation

- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md): components and conventions.
- [docs/ANDROID.md](docs/ANDROID.md): Android design, benchmark history, caveats.
- [docs/IOS.md](docs/IOS.md): iOS design, benchmark history, caveats (private API use,
  Simulator vs device).
