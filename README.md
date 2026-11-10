# agentic-emulator (`agemu`)

[![npm](https://img.shields.io/npm/v/agentic-emulator)](https://www.npmjs.com/package/agentic-emulator)
[![CI](https://github.com/piotr-graczyk-dev/agentic-emulator/actions/workflows/ci.yml/badge.svg)](https://github.com/piotr-graczyk-dev/agentic-emulator/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue)](LICENSE)

**A fast Android emulator and iOS Simulator for coding agents that build mobile apps.**

When an agent builds an app, writing the code is quick. Checking the result on a device is slow.
Every screenshot, screen read, tap and reset goes through `adb`, `simctl` or Maestro, and each call
takes from hundreds of milliseconds to several seconds. A test flow has dozens of these steps, so
the agent spends most of its time waiting for the emulator instead of building.

agemu replaces that control layer and leaves the devices stock: the Android SDK's arm64 image and
Xcode's Simulator runtime. Your app behaves exactly as it does on the standard emulator and
Simulator. Agents use agemu through an MCP server or a CLI.

```sh
claude mcp add agemu -- npx -y agentic-emulator@latest mcp
```

[![The same agent task with adb, agent-device, Argent and agemu, on Android and iOS](examples/agent-race/out/teaser.gif)](examples/agent-race/out/montage.mp4)

The same 18-step shopping flow (sign in, search, cart, checkout) driven by four tool stacks.
[Montage](examples/agent-race/out/montage.mp4) ·
[Android race](examples/agent-race/out/android/race.mp4) ·
[iOS race](examples/agent-race/out/ios/race.mp4) ·
[method](examples/agent-race)

## Where it helps

The build, run and check loop: the agent changes code, installs the app, looks at the screen, taps
through a flow, reads the logs, resets, and goes again.

**A whole test flow** (the race above, 3 runs per tool, medians):

| | standard emulator + `adb` | agent-device | Argent | agemu |
|---|---|---|---|---|
| Android | 74 s | 24 s | 30 s | **10.5 s** |
| iOS | - | 36 s | 18 s | **17 s** |

On iOS most of the flow's time belongs to the app itself, so the gap is smaller there.

**Each step of the loop:**

| Step | Standard tools | agemu |
|---|---|---|
| Take a screenshot | ~150 ms (`adb`, `simctl`) | **4-5 ms** |
| Read the screen (UI tree) | 2.0 s (`uiautomator`), 7.1 s (Maestro on iOS) | **2-23 ms** |
| Tap on iOS | 8.4 s per call (Maestro) | **160 ms** |
| Return to a clean state | 14 s (Android cold boot), 36 s (iOS `erase` and boot) | **2.1 s** |
| Notice that the app crashed | the agent waits until its timeout | **under 0.2 s**, with the crash's top frames |
| Read the app's latest logs | 250 ms (`logcat`), 960 ms (`log show`) | **~1 ms** |

**Each device uses less of your Mac**, so it can run beside your IDE, builds and browser:

| Per device | Standard | agemu |
|---|---|---|
| Android emulator, memory | 7.5 GB | **4.3-5.4 GB** |
| Android emulator, CPU at idle | 86% of a core | **19-24%** |
| Android emulator, CPU in the first minute after boot | 202% of a core | **120%** |
| iOS simulator, memory with no app | 4.0 GB | **2.5 GB** |
| iOS simulator, CPU at idle | 20% of a core | **1.5%** |
| iOS simulator, memory with the app running (the race) | 4.3-4.6 GB | **2.1-2.3 GB** |

The standard Android emulator is the AVD as Android Studio runs it: 4 GB of guest RAM, a window and
audio. agemu runs it headless with 2 GB, the host GPU and preinstalled Google apps turned off. The
iOS simulator saves come from system services an app under test doesn't use, which agemu turns
off. Idle figures are single sessions on a busy Mac, so treat them as rough.

Measured on an M2 Pro Mac with 16 GB, over agemu's daemon connection as the MCP server uses it. The
method, sample sizes and every other measurement are in [docs/ANDROID.md](docs/ANDROID.md),
[docs/IOS.md](docs/IOS.md) and [bench/final/](bench/final/).

## What the agent gets

- **One call per step.** `tap --text "Sign in" --wait-text Welcome --ui` taps, waits for the
  result and returns the new screen. Fewer calls means fewer agent turns.
- **Taps that land.** A tap by selector waits until the element can take the touch: after a screen
  transition, or when an alert or sheet has just appeared. A flow doesn't fail because a tap came
  too early.
- **Crash notice.** When the app dies, the next call says so instead of waiting for a timeout.
- **Logs and crash reports from memory**, filtered to your app and to what happened since launch.
- **Test capabilities.** Deep links, permissions, location, clipboard, dark mode, push (iOS),
  clear data and uninstall. `reset` undoes them all.
- **Lighter devices.** agemu turns off system services an app under test doesn't use, such as
  widgets, Siri and preinstalled Google apps.
- **Leaves your own devices alone.** agemu touches only devices named `agemu-*`, so it runs next to
  your emulators, simulators and other tools.

## Where it doesn't help

- **Boots, app launches and your app's own animations take as long as before.** That time belongs
  to the OS and the app. `AGEMU_ANDROID_ANIMATIONS=0` and `AGEMU_IOS_ANIMATIONS=0` shorten
  animations if you opt in.
- **A new Android device takes 7-8 minutes to prepare, once.** agemu saves a clean baseline after
  the system has settled, so every later boot and reset is fast and quiet.
- **More devices don't add up.** On a 16 GB Mac, total throughput levels off at about 2.5 devices'
  worth.
- **iOS needs an Apple Silicon Mac and relies on private Xcode APIs.** Android also runs on Linux
  and Windows, where it is newer and less measured. See [Known limitations](#known-limitations).

## Install

```sh
claude mcp add agemu -- npx -y agentic-emulator@latest mcp   # Claude Code
codex mcp add agemu -- npx -y agentic-emulator@latest mcp    # Codex CLI
npx -y agentic-emulator doctor                               # checks this Mac, prints fixes
```

`agemu doctor` checks each platform separately. One is enough: an iOS-only or Android-only Mac is
fine.

| | Needed for |
|---|---|
| Apple Silicon Mac | everything on macOS |
| or Linux (x86-64, arm64) with KVM, or Windows (x86-64) with WHPX | Android ([Linux and Windows](docs/INSTALL.md#linux-and-windows); the image is the `x86_64` one on x86-64) |
| Xcode with an iOS Simulator runtime | iOS |
| Android SDK (emulator, platform-tools, command-line tools), the arm64 image `system-images;android-37.1;google_apis_playstore_ps16k;arm64-v8a` and Java 17+ | Android |

For agents that use a shell, put `agemu` on `PATH`:

```sh
npm install -g agentic-emulator
# or without Node, into ~/.local/bin (macOS, Linux):
curl -fsSL https://raw.githubusercontent.com/piotr-graczyk-dev/agentic-emulator/main/install.sh | sh
# or on Windows, in PowerShell:
irm https://raw.githubusercontent.com/piotr-graczyk-dev/agentic-emulator/main/install.ps1 | iex
```

Other MCP clients, updates, uninstalling, building from source and troubleshooting:
[docs/INSTALL.md](docs/INSTALL.md).

## Use

```sh
agemu create --platform ios --name iphone              # -> an agemu-iphone simulator
agemu create --platform android --name pixel           # -> android:agemu-pixel
agemu boot agemu-iphone
agemu install MyApp.app && agemu launch com.example.app
agemu ui                                               # compact outline: [ref] role "text" id=... @(x,y wxh)
agemu tap --text "Sign in" --wait-text Welcome --ui    # act, wait and observe in one call
agemu type "hello"                                     # into the focused field
agemu key back                                         # home, back, enter, delete, tab, escape, ...
agemu screenshot -o shot.png
agemu logs com.example.app --since launch --level warn
agemu crashes com.example.app                          # crash, ANR and system-kill reports with top frames
agemu open myapp://orders/7 --wait-id order-7          # deep link
agemu permission grant com.example.app camera location
agemu appearance dark
agemu profile small-phone                              # another screen size
agemu reset                                            # back to the clean baseline
agemu shutdown
```

Every command prints JSON, and `agemu help` lists them all. Coordinates are screenshot pixels on
both platforms. With exactly one booted device, `-d <device>` can be left out. The test
capabilities and what `reset` restores: [docs/CAPABILITIES.md](docs/CAPABILITIES.md).

The MCP server exposes the same functions as 31 tools: `devices`, `profiles`, `create`, `boot`,
`shutdown`, `reset`, `park`, `resume`, `set_profile`, `screenshot`, `ui_tree`, `find`, `tap`,
`swipe`, `type_text`, `press_key`, `install`, `launch`, `terminate`, `wait_for`, `open_url`,
`permission`, `location`, `clipboard`, `appearance`, `apps`, `uninstall`, `clear_app_data`,
`push`, `logs` and `crashes`. While a new Android device prepares, `boot` returns state
`preparing` at once and the agent polls `devices`, so no tool call outlasts a client timeout.

### Profiles

| Platform | Profiles |
|---|---|
| Android | `pixel-9-pro`, `pixel-9`, `galaxy-s24`, `small-phone`, `pixel-fold-inner`, `pixel-tablet`, or `profile --width W --height H --density D`. Switches at runtime, without a reboot. |
| iOS | `iphone-17-pro-max`, `iphone-17-pro`, `iphone-17`, `iphone-air`, `iphone-16e`, `iphone-se-3`, `ipad-mini`, `ipad-pro-11`. agemu switches to an `agemu-<profile>` simulator. |

### Settings

<details>
<summary>Environment variables for the daemon</summary>

The daemon reads them from the process that started it, which may be an MCP client without your
shell's environment. Set them in the MCP config's `env`, or run `agemu daemon restart` from your
shell. Devices keep running across a restart.

| Variable | Default | Effect |
|---|---|---|
| `AGEMU_HOME` | `~/.agemu` | State directory and the daemon's socket. |
| `AGEMU_IOS_LEAN` | on | `0`: the full Simulator (jobs back from the next boot, keynote mode removed). |
| `AGEMU_IOS_IDLE_EXIT` | on | Lean simulators end the on-demand daemons that stay idle after boot, except those found behind public app APIs. `all`: those too (an app's first call to one then waits for its restart, up to 2-3 s); `0`: none. |
| `AGEMU_IOS_IDLE_KEEP` | unset | Comma-separated daemon names (executable file names) that are never ended as idle. |
| `AGEMU_ANDROID_LEAN` | `1` | `0`: keep the Google apps (applies when a baseline is taken). |
| `AGEMU_ANDROID_GPU` | `host` (`auto` on Linux without a display) | `auto`: software rendering. |
| `AGEMU_ANDROID_SNAPSHOT_COMPRESS` | `1` | `0`: raw baseline (applies when a baseline is taken). |
| `AGEMU_ANDROID_BASELINE_UPTIME_S` | `420` | Guest uptime (and a quiet minute) the baseline waits for; a first boot then takes 7-8 min (at most 20), during which the device is `preparing`; `0`: save at once (applies when a baseline is taken). |
| `AGEMU_ANDROID_FIRSTBOOT_REPAIR` | `1` | `0`: do not boot an AVD whose first boot was interrupted before it ever completed with `-wipe-data` (two interruptions about 10 s in leave it unbootable otherwise). |
| `AGEMU_ANDROID_STREAM_IDLE_S` | `30` | Seconds without calls before the frame stream of a changing screen stops; `0`: never. |
| `AGEMU_ANDROID_ANIMATIONS` | normal | `0`: system animations off. |
| `AGEMU_IOS_ANIMATIONS` | normal | `0`: UIKit and SwiftUI animations ×0.05 (or a factor from 0.05 to 1). |
| `AGEMU_ANDROID_TAP_WAIT` | `1` | `0`: taps by selector do not wait for transitions or motion. |
| `AGEMU_IOS_TAP_SETTLE` | on | `0`: taps and typing do not wait for a still screen, nor taps for a just-presented element. |
| `AGEMU_ANDROID_LOGS`, `AGEMU_IOS_LOG_CAPTURE` | on | `0`: no log capture; `logs` says so (iOS still watches launched apps' exits and `.ips` reports). |
| `AGEMU_IOS_MAX_BOOTED` | 2, or 1 while memory is short | agemu simulators booted at once; a boot or iOS profile switch beyond it shuts down the least recently used one. |
| `AGEMU_ANDROID_STANDBY` | `1` | `0`: `shutdown` always powers off. |
| `AGEMU_ANDROID_AVDS` | `agemu-` | AVD name prefixes agemu manages. |
| `AGEMU_IOS_ALL_DEVICES`, `AGEMU_IOS_ALLOW_FOREIGN` | off | `1`: list every simulator (read-only); also act on them. |
| `AGEMU_AUTO_PARK` | off | A duration (`10m`, `90s`; at least 10 s): park a device the daemon serves once it has had no call for that long; the next call resumes it. See [docs/IOS.md](docs/IOS.md#parking). |

</details>

## Known limitations

- **iOS uses private CoreSimulator, SimulatorKit and AccessibilityPlatformTranslation APIs.** Any
  Xcode release can break them. This version was tested on Xcode 26.6 with iOS 26.5.
- **The Simulator is not an iPhone.** It runs apps natively on the Mac, so GPU, sensors, memory
  limits and performance differ from a real device.
- **iOS reset is a soft reset.** It removes user apps, privacy grants, the keychain, the pasteboard,
  appearance and location. Built-in apps' data stays. For a full wipe, use `agemu shutdown` and
  `xcrun simctl erase`.
- **Android: one UiAutomation client per device.** While agemu runs, `uiautomator dump` and
  UiAutomator-based tests can't connect.
- **Android profiles other than the AVD's native one are resampled.** Create the AVD with
  `--profile <name>` to make that profile pixel-exact.
- **Rotation isn't handled**, and iPad Split View and Slide Over are untested.
- **Android on Windows is a preview.** On GitHub's Windows runner the emulator boots under agemu,
  but the guest's services fail under nested virtualization, so the UI tree and taps are not yet
  verified on Windows. CI covers the unit tests, the daemon and the installers there, and runs the
  Android end-to-end test on Linux with KVM. Windows screenshots use the gRPC reply for now (see
  [docs/INSTALL.md](docs/INSTALL.md#linux-and-windows)).

## Documentation

- [docs/INSTALL.md](docs/INSTALL.md): every install method, MCP clients, updates, troubleshooting.
- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md): components and conventions.
- [docs/ANDROID.md](docs/ANDROID.md) and [docs/IOS.md](docs/IOS.md): platform design, all
  measurements, caveats.
- [docs/CAPABILITIES.md](docs/CAPABILITIES.md): test capabilities, the reset contract, logs and
  crashes.
- [examples/agent-race](examples/agent-race): the recorded race and how to run it.

## Contributing and license

Bug reports, measurements on other Macs and pull requests are welcome: see
[CONTRIBUTING.md](CONTRIBUTING.md), and [SECURITY.md](SECURITY.md) for vulnerabilities. Tests:
`make test` (no device needed), `make android-e2e` and `make ios-e2e`. agemu is released under the
[MIT License](LICENSE); the code it includes from others is listed in
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
