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

Test capabilities ([docs/CAPABILITIES.md](docs/CAPABILITIES.md)):

```sh
agemu open myapp://orders/7 --wait-id order-7          # a deep link, as a tapped link opens it
agemu permission grant com.example.app camera location # also revoke, and reset [<name>...]
agemu location set 52.2297 21.0122                     # simulated GPS fix; location clear
agemu clipboard set "code 1234"                        # clipboard prints it back
agemu appearance dark                                  # appearance alone reads light|dark
agemu apps                                             # user-installed apps: id, name, version
agemu clear-data com.example.app                       # like a fresh install, app kept
agemu uninstall com.example.app
agemu push com.example.app '{"aps":{"alert":"Hi"}}'    # iOS only; payload inline, a file or -
```

Logs and crashes ([docs/CAPABILITIES.md](docs/CAPABILITIES.md#logs-and-crashes)):

```sh
agemu logs com.example.app --since launch --level warn # the app's lines since its last launch through agemu
agemu logs --contains "timeout" --since 5m             # the whole device; --regex, --limit, --max-bytes
agemu logs --since c1794046584879630                   # only lines after the cursor the last call printed
agemu crashes com.example.app                          # crash, ANR and system-kill reports with top frames
```

When the app launched last through agemu has crashed, the next command says so (`note: ...` on
stderr, `appCrash` over MCP), and a command that fails while the app is down says why and exits 4
(`app_crashed`): a `wait` for an element of the crashed app stops instead of timing out.

A capability a platform cannot do faithfully answers "not implemented" (exit status 3) instead
of faking it: push on Android, and on iOS the photos grant, `contacts-limited`, notifications
and raw Android permission names.

With exactly one booted device, `-d <device>` can be left out.

Actions (`tap`, `swipe`, `type`, `key`, `launch`, `open`, `push`) take `--wait-text/--wait-id/--wait-contains/--wait-role`
(plus `--wait-gone`, `--wait-timeout-ms`) to wait for their effect in the same call, and `--ui` to
also return the screen they end on. One call per agent step instead of two or three.

On Android, a tap by selector right after the screen moved first waits (bounded, about 5 s at
worst) until no window transition runs and the element has stopped moving; on a screen that has
not changed for 0.5 s it taps at once. A screenshot right after an input skips frames that still
show the screen from before it (at most 0.5 s; not every case is covered). See
[docs/ANDROID.md](docs/ANDROID.md#selector-taps) and "Agent-loop reliability" below.

### MCP

```sh
claude mcp add agemu -- /path/to/bin/agemu mcp
```

The tools are `devices`, `profiles`, `create`, `boot`, `shutdown`, `reset`, `set_profile`,
`screenshot` (returns the image), `ui_tree`, `find`, `tap`, `swipe`, `type_text`, `press_key`,
`install`, `launch`, `terminate` and `wait_for`, for the test capabilities `open_url`,
`permission`, `location`, `clipboard`, `appearance`, `apps`, `uninstall`, `clear_app_data` and
`push`, and `logs` and `crashes` (29 in all). All MCP sessions share the one daemon.

### Test capabilities

Each is the system call the standard tool makes, sent from agemu's long-lived on-device agent
(Android) or helper (iOS) instead of a new process per call. Daemon (MCP) medians against the
standard tool, n=12 per arm, interleaved, on a loaded host; details and caveats in
[docs/CAPABILITIES.md](docs/CAPABILITIES.md).

| Operation | Android: agemu / `adb` | iOS: agemu / `xcrun simctl` |
|---|---|---|
| open a deep link | 3.7 / 34.7 ms (9.4x), app in front; 261 / 222 ms (0.9x), app stopped | 6.7 / 120 ms (18x) |
| grant, revoke, reset a permission | 2.6-4.2 / 30.4-35.5 ms (7.5-12.3x) | 1.5-4.7 / 116-130 ms (28-78x) |
| set a location (the call) | 0.6 / 16.3 ms; the app sees the fix no sooner (1.0x) | 0.5-0.8 / 107-109 ms |
| clipboard set, get | set 2.4 ms, get 1.8 ms (no standard tool) | set 0.8 / 133 ms (159x), get 11.7 / 161 ms (14x) |
| read the appearance | 0.8 / 22.8 ms (28x) | 1.0 / 175 ms (174x) |
| set the appearance | 27.2 / 67.4 ms (2.5x) on the home screen | 694 / 670 ms until a screenshot shows it (1.0x); the call waits for the ~0.5 s crossfade |
| list user apps | 8.5 / 28.0 ms (3.3x; one user app installed) | 0.7 / 173 ms (another mechanism: Info.plists on the host) |
| uninstall | 61.7 / 119.1 ms (1.9x) | 83 / 194 ms (2.3x) |
| clear app data | 35.6 / 79.3 ms (2.2x) | 258 / 538 ms (2.1x; delete and reinstall from a clone) |
| push | not implemented (no faithful path) | 15.8 / 133 ms (8x) |

`reset` restores what these change: light appearance, the baseline location, an empty
clipboard, no permission grants, no user apps.

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
  each). In the first 5 minutes after boot it also stops what iOS starts in the background
  and then keeps idle (the Calendar app, App Intents extensions, device management services):
  0.24 GB less 4 minutes after boot when iOS had opened Calendar and it stayed open (6 pairs of
  boots, all of them so); otherwise about 0.15 GB (estimated from the processes' sizes). A
  Calendar that is in front at one of the passes is never stopped. `AGEMU_IOS_LEAN=0` keeps the
  full system.
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

### Taps on just-presented UI and the home button (iOS)

- An alert's, action sheet's, sheet's or system permission alert's buttons are in the UI tree
  before the presentation has drawn, and UIKit drops every touch until it has finished. A tap
  by selector on an element that appeared since the last input now waits for that. Tapping the
  button right after waiting for the alert's text, as an agent does, lost 26-49 of 50 taps per
  kind of presentation before and 0 of 50 now (5 kinds, probe app, normal animation speed;
  with `AGEMU_IOS_ANIMATIONS=0`, 0-35 of 50 before and 0 of 50 now). The tap took 1-37 ms
  longer than a landed tap did before (medians; action sheets: 507 ms against the one landed
  tap's 202 ms, since their wait ends on a fixed 300 ms tail); against all tap calls before,
  most of which returned in 15-95 ms and were lost, it adds about 420-510 ms. An element the
  agent could see before its last input is tapped as before (where nothing was presented: 160
  against 159 ms, medians of 30); the wait for a new element on a still screen was not measured.
  See [docs/IOS.md](docs/IOS.md#taps-on-just-presented-ui).
- `agemu key home` returns once SpringBoard has acted on the press (it does so about 0.3 s
  later), and `reset` no longer presses home when SpringBoard is already in front. An app
  launched right after was sent to the background 59 of 60 times before and 0 of 60 now (30
  `key home`, 30 `reset` with the home screen in front; at fast animation speed 60 of 60 and 0
  of 60). `key home` takes 0.66-1.3 s now instead of 5-23 ms; reset is not slower, but it no
  longer resets SpringBoard's own state (home-screen page, an open folder, Spotlight).

### Agent-loop reliability (Android)

Measured with the selector tap sent the moment the previous call returned (no think time), on a
probe app, n=50 per arm in the four scenarios below, alternating builds:
- Taps that failed: a tap right after a dialog closed 48/50 -> 0/50, a list row during a smooth
  scroll 37/50 -> 5/50, the permission dialog's button 20/50 -> 0/50, a sliding sheet 2/49 ->
  0/50.
- Cost per tap: +72 to +821 ms (medians) right after the screen moved, including +414 to +821 ms
  in three scenarios (25 per arm) that never failed before; +0.1 ms on a screen still for 1.5 s.
- Screenshots right after an input that still showed the old screen: 35/120 -> 5/120; their
  median time rose in two of four cases (Settings back 91 -> 381 ms, dialog open 334 -> 809 ms).
- `AGEMU_ANDROID_TAP_WAIT=0` turns the tap waits off. See
  [docs/ANDROID.md](docs/ANDROID.md#agent-loop-reliability-2026-11-07).

### Logs and crashes

Each device's log is captured into the daemon while it runs (Android: one `logcat -B` reader;
iOS: one `log stream` per simulator plus a process watch on launched apps), so `logs` and the
crash notice answer from memory. It observes only: no log level, buffer or environment
changes. Measured on agemu-lab and an iPhone 17 Pro simulator:

| | Android | iOS |
|---|---|---|
| an app's newest lines | 0.56 ms in the daemon, 8.2 ms CLI; `adb logcat -d --uid`: 252 ms (n=15) | 1.11 ms over the socket, 8.84 ms CLI; `simctl spawn log show`: 961 ms (n=12) |
| a crash told after the request that caused it (medians, n=5 per kind) | Java 10.3 ms, SIGSEGV 10.3, abort 62.1, `System.exit` 19.2 (merged build, quiet device); up to about 0.3 s while the device keeps logging over 100 records/s | a `wait` ended with `app_crashed` a median 199 ms after the crash (58-791 ms, 60 of 60, an earlier build) |
| a `wait` for the crashed app's element | `app_crashed` after 326 ms instead of a 10 s timeout (n=5; the next tree read then waits up to about 4.7 s for the device) | `app_crashed` instead of a 15 s timeout |
| crash frames | Java at once, native from the tombstone about 0.12 s later, ANR 0.6-0.8 s after the OS raised it (n=3) | an NSException's first throw stack from its log line; signal and Swift crashes only from the Mac's `.ips`, 1.7-17.5 s later for 13 of 30 (ReportCrash stopped after 25 reports a day) |
| cost | idle: not measurable (emulator 5.25 vs 5.30% of a core), daemon +12 MB; app logging 500 lines/s: +7.9% of a core in dumps (a build that switched at once; the merged build streams the first 2-3 s); 40-100 records/s not measured | idle: not measurable, +15 MB; app logging 200 lines/s: about +4% of a core, +22 MB |
| fidelity | an overflowing 5,000-line burst lost more lines in logd with capture on (median kept 3,153 vs 4,618, n=4 each) | the app's own logging about 20% slower while the stream runs (29.2 vs 24.3 ms per 1000 lines, 4 windows per arm) |

Records are believed only from their writers (Android: the crashing process itself,
system_server, system daemons), so another app cannot report that the app under test crashed;
an exit during agemu's own terminate, install or permission call is not a crash unless it
carries a crash signal or report. See [docs/ANDROID.md](docs/ANDROID.md#logs-and-crashes-2026-11-07)
and [docs/IOS.md](docs/IOS.md#logs-and-crashes).

### Safety

agemu manages only devices named `agemu-*`. Other emulators and simulators on the machine are
never booted, shut down or driven, so agemu can run next to your own devices and other tools.

## Proof

| What | How to run it |
|---|---|
| End-to-end, Android | `scripts/e2e-android.sh` (41 checks, 13 of them the test capabilities and 8 logs and crashes on a probe app; CLI only) |
| End-to-end, iOS | `scripts/e2e-ios.sh` (127 checks, including logs and crashes; CLI only; includes a third-party SwiftUI app and iPad) |
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
- **iOS has no snapshots.** Reset is a soft reset: it terminates and uninstalls user apps,
  resets privacy grants and the keychain, empties the pasteboard, sets light mode and clears the
  simulated location. Built-in apps' data is not reset.
- **iPad Split View / Slide Over is untested.** `ui` reads the app at the screen centre.
- **Simulator is not a device.** The iOS Simulator runs apps natively on the Mac; GPU,
  sensors and performance differ from an iPhone.

## Documentation

- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md): components and conventions.
- [docs/ANDROID.md](docs/ANDROID.md): Android design, benchmark history, caveats.
- [docs/IOS.md](docs/IOS.md): iOS design, benchmark history, caveats (private API use,
  Simulator vs device).
