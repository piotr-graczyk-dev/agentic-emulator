# iOS backend

agemu drives the **real iOS Simulator runtime** through CoreSimulator, the same runtime
Simulator.app and `xcrun simctl` use. Apps run unmodified. No Simulator.app window is opened:
devices boot headless, and agents see the screen only through screenshots and the UI tree.
All the speed comes from the control plane.

```
agemu CLI / MCP ──unix socket──► agemu daemon (Go, internal/ios)
                                   │ JSON lines over stdio (one long-lived child process)
                                   ▼
                          agemu-ios-helper (ios-helper/, Swift + Objective-C)
                                   │ dlopen + Objective-C runtime, in-process
          ┌────────────────────────┼─────────────────────────────┐
  CoreSimulator.framework   SimulatorKit.framework    AccessibilityPlatformTranslation
  devices, boot, apps,      Indigo HID messages via   AXPTranslator: the frontmost app's
  framebuffer IOSurface     SimDeviceLegacyHIDClient  accessibility tree
```

## How each operation works

| Operation | Standard tool | agemu |
|---|---|---|
| screenshot | `simctl io screenshot`: new process, connects to the device, renders, encodes | The helper keeps the main display's framebuffer **IOSurface** (and its change callbacks) for the life of the process. A screenshot copies the surface (~2 ms) and encodes it: PNG with agemu's parallel encoder (`AGPNG.c`: the image is cut into 12 bands that are filtered and deflated on separate cores at zlib level 1, then joined into one valid zlib stream, as pigz does), JPEG with ImageIO. |
| tap / swipe | no simctl command; Maestro starts a JVM and an XCUITest runner | Indigo HID touch messages sent through SimulatorKit's `SimDeviceLegacyHIDClient`, the same channel Simulator.app uses for mouse clicks. Pixels are converted to screen ratios. A swipe is a touch-down, one move per ~16 ms, and a touch-up. |
| type | Maestro `inputText` | HID keyboard events (US layout, shift handled). Text the US keyboard cannot produce (accents, emoji, other scripts) is put on the simulator pasteboard (`simctl pbcopy`) and pasted with Cmd-V. |
| key | — | `home` and `lock` are the hardware buttons; `enter`, `delete`, `tab`, `escape`, arrows are keyboard keys; `volumeup/volumedown` are consumer-page HID usages. iOS has no back key: `back` taps the navigation bar's back button (UIKit's `BackButton` identifier) when the screen has one, and otherwise does a **left-edge swipe**, flagged as an edge gesture, which triggers the navigation controller's interactive pop like a thumb does. On the loaded test host one edge swipe was not recognized, hence the tap first. |
| UI tree | `maestro hierarchy` (JVM + XCUITest snapshot) | `AXPTranslator` from AccessibilityPlatformTranslation (the framework Simulator.app's accessibility support uses), with agemu as its token delegate: every attribute request is forwarded to the device with `-[SimDevice sendAccessibilityRequestAsync:...]`. The helper reads only the attributes agemu maps (role, subrole, label, identifier, value, placeholder, title, frame, enabled, selected, focused, children). |
| launch / terminate / install | `simctl launch` etc. | The same CoreSimulator calls, made in-process by the helper. |
| boot | `simctl boot` + `simctl bootstatus -b` | `-[SimDevice bootWithOptions:]` (persisted, headless), then agemu waits for **agent-ready**: booted, SpringBoard answers accessibility, the framebuffer is readable, the HID client is connected. A device that is already booted is ready in milliseconds, and agemu keeps devices booted (see below). |
| reset | `simctl shutdown` + `simctl erase` + boot | A soft reset of the running device; see below. |
| profile | — | iOS screens are fixed per device type, so `agemu profile <name>` switches to the `agemu-<name>` simulator of that device type (created and booted when missing) and returns its id. |

### Coordinates

Every coordinate is in **screenshot pixels**: tap a point read off a full-size screenshot and it
lands there. The accessibility frames iOS reports are in points; agemu multiplies them by the
device's scale (3 for current iPhones, 2 for iPhone SE and iPads), rounding each edge separately
so adjacent elements stay adjacent (`internal/ios/uitree.go`, tested in `uitree_test.go`).

### UI tree mapping

| api.Element | from |
|---|---|
| role | normalized AX role: `button`, `text` (static text and headings), `textfield` (incl. search and secure fields), `image`, `switch` (AXCheckBox with AXSwitch subrole), `checkbox`, `cell`, `link`, `slider`, `group`, `application`, ... |
| type | raw AX role and subrole, e.g. `AXTextField:AXSearchField` |
| text | static text: its label; text field: its value (the typed text); otherwise the AX title |
| label | `accessibilityLabel`; for an unlabeled text field, its placeholder |
| id | `accessibilityIdentifier` |
| value | `accessibilityValue` (an empty field's placeholder, which iOS reports as the value, is dropped) |
| frame | AX frame (points) × scale, in pixels |
| enabled / selected / focused | the AX flags |
| checked | switches, checkboxes and radio buttons: value `1` / `0` |
| clickable | interactive roles (button, cell, link, switch, textfield, ...) |

Elements that lie entirely off screen (e.g. the previous page during a navigation transition)
and zero-sized leaves are dropped. Two quirks of the iOS 26 accessibility tree are handled:

- Some containers report no children although they have some (the toolbar hosting the
  Settings search field reports empty `AXChildren` and `AXChildrenInNavigationOrder`). For an
  `AXGroup` with no children the helper hit-tests three points along its centre line and adds
  the distinct elements it finds inside the group.
- Apps load their accessibility runtime only while `ApplicationAccessibilityEnabled` is on in
  the simulator. XCUITest-based tools (Maestro, `xcodebuild test`) switch it on for their
  session and **off** when they finish, after which newly launched apps expose an empty tree.
  Whenever a live app exposes an empty root, agemu switches it on and posts `com.apple.accessibility.cache.app.ax` / `com.apple.accessibility.cache.ax` so the
  running app loads its accessibility runtime without a relaunch. Any UI automation tool,
  XCUITest included, does the same.

### Self-repair of the simulator's accessibility bridge

Inside the simulator, `CoreSimulatorBridge` answers the accessibility requests. On this
heavily loaded host SpringBoard was once SIGKILLed during a boot (presumably by the boot
watchdog) and relaunched; the bridge kept a handle to the dead SpringBoard and from then on
answered every request with "no frontmost application", for any client, until it was restarted
(observed for 10 minutes). After a cold boot this happened on every agemu boot measured. When
accessibility still returns no frontmost app although SpringBoard has been running for 3 s, the
helper restarts the bridge (`launchctl kickstart -k system/com.apple.CoreSimulator.bridge`
inside that simulator), once per SpringBoard instance, and logs it to the daemon log. During a
first boot (data migration) accessibility can stay unavailable for minutes regardless; agemu
then simply waits (up to 10 minutes).

## Fast boot and reset

There are no snapshots for iOS Simulators, so a cold boot costs what it costs (see the table).
agemu makes boot and reset fast with two honest techniques:

- **Keep devices booted.** `agemu boot` on a running device only verifies it is agent-ready.
  The daemon never shuts a device down on its own, except to stay within
  `AGEMU_IOS_MAX_BOOTED` (default 2) booted agemu simulators when an agent switches profiles;
  then the least recently used one is shut down.
- **Soft reset instead of erase.** `simctl erase` needs a shutdown, an erase and a full first
  boot (data migration). `agemu reset` cleans the running device instead:
  1. terminates every app (system and user). Terminating an app that is not running fails in
     about a millisecond, so this is cheaper than asking launchctl what runs. The built-in app
     list is fixed per runtime and cached (the CoreSimulator query takes 1-2 s); user apps are
     found by reading `Containers/Bundle/Application/*/*.app/Info.plist` in the device's data
     directory on the host,
  2. uninstalls every user-installed app, which deletes its data and app-group containers,
  3. resets privacy permissions (`simctl privacy reset all`), the keychain
     (`simctl keychain reset`) and the pasteboard,
  4. presses home and waits until SpringBoard is frontmost.

  It does **not** reset what `simctl erase` would: data stored by built-in apps (Settings search
  history, Safari, Contacts, Photos...), system settings an agent changed (language, appearance,
  location...), and media added with `simctl addmedia`. Use `agemu shutdown` +
  `xcrun simctl erase <udid>` for a full wipe.

## Safety: which simulators agemu touches

agemu lists and acts only on simulators whose name starts with **`agemu-`**. Devices created by
Xcode, other tools or people are invisible to `agemu devices` and any command that targets them
fails with an explanation. `AGEMU_IOS_ALL_DEVICES=1` lists every simulator (read-only),
`AGEMU_IOS_ALLOW_FOREIGN=1` also allows acting on them. agemu never runs
`simctl shutdown all` / `erase all` and never restarts CoreSimulatorService.

## Profiles

| profile | device type | pixels | scale | verified |
|---|---|---|---|---|
| iphone-17-pro-max | iPhone 17 Pro Max | 1320×2868 | 3 | device type profile.plist |
| iphone-17-pro (default) | iPhone 17 Pro | 1206×2622 | 3 | real screenshot |
| iphone-17 | iPhone 17 | 1206×2622 | 3 | device type profile.plist |
| iphone-air | iPhone Air | 1260×2736 | 3 | device type profile.plist |
| iphone-16e | iPhone 16e | 1170×2532 | 3 | device type profile.plist |
| iphone-se-3 | iPhone SE (3rd generation) | 750×1334 | 2 | real screenshot |
| ipad-mini | iPad mini (A17 Pro) | 1488×2266 | 2 | real screenshot |
| ipad-pro-11 | iPad Pro 11-inch (M5) | 1668×2420 | 2 | device type profile.plist |

The values in `internal/profiles` were already correct. "device type profile.plist" means the
size was checked against `mainScreenWidth/Height/Scale` in the device type bundle
(`/Library/Developer/CoreSimulator/Profiles/DeviceTypes/*.simdevicetype`), which is what the
simulator's framebuffer is created from; creating and booting all eight simulators did not fit
the disk budget. Density is scale × 160.

## Benchmarks

All numbers below are real output of `bin/ios/agemu-bench --platform ios --device
ios:<udid> --agemu bin/ios/agemu --n 20` on one `agemu-iphone-17-pro` simulator (iOS 26.5),
Xcode 26.6, Apple M2 Pro. Baseline and agemu run back to back on the same device.

**Host conditions.** The Mac was heavily loaded the whole time by unrelated work: a parallel
Android emulator, several Chrome renderers at 100% CPU each, and more. The load average was
about 25-50 during run A and reached ~380 afterwards. Every number is noisy (see the p95s), and
the Maestro baselines grew from ~16 s to ~30-48 s per call between runs. Each row compares the
two tools under the same conditions.

### Run A: full suite

### ios benchmark (2026-11-05)

Host: Apple M2 Pro, macOS 26.6, arm64. Device: `ios:ED6C55BD-83FB-49B9-93A2-54BA0AD521DD`. Median of n runs (p95 and n in parentheses).

| Operation | Baseline tool | Baseline | agemu CLI | agemu daemon/MCP | Speedup (CLI) | Speedup (daemon) |
|---|---|---|---|---|---|---|
| screenshot | `xcrun simctl io <udid> screenshot (png)` | 233.0 ms (p95 1296.0, n=20) | 21.7 ms (p95 59.2, n=20) | 12.6 ms (p95 27.4, n=20) | **10.7×** | **18.5×** |
| screenshot-jpeg | `xcrun simctl io <udid> screenshot --type=jpeg` | 253.4 ms (p95 1133.3, n=20) | 17.2 ms (p95 47.5, n=20) | 11.1 ms (p95 127.9, n=20) | **14.8×** | **22.8×** |
| ui-tree | `maestro hierarchy (JVM + XCUITest runner per call)` | 31582.9 ms (p95 40757.2, n=3) | 209.0 ms (p95 1128.8, n=20) | 191.6 ms (p95 559.6, n=20) | **151.1×** | **164.8×** |
| find-element x10 | `one maestro flow: 10x assertVisible` | 27333.2 ms (p95 31947.0, n=3) | 698.1 ms (p95 791.9, n=3) | 592.7 ms (p95 952.7, n=3) | **39.2×** | **46.1×** |
| tap | `maestro test (1x tapOn point)` | 40366.6 ms (p95 47828.3, n=3) | 14.7 ms (p95 53.6, n=20) | 0.9 ms (p95 6.4, n=20) | **2752.4×** | **47157.2×** |
| tap x10 | `one maestro flow: 10x tapOn point` | 47860.0 ms (p95 55421.1, n=3) | 157.7 ms (p95 160.2, n=3) | 39.3 ms (p95 43.9, n=3) | **303.5×** | **1216.3×** |
| swipe (300 ms gesture) | `maestro test (1x swipe, 300 ms)` | 29692.4 ms (p95 31799.9, n=3) | 443.8 ms (p95 454.7, n=10) | 426.0 ms (p95 451.7, n=10) | **66.9×** | **69.7×** |
| type text | `maestro test (inputText "hello")` | 30427.5 ms (p95 34578.2, n=3) | 11.3 ms (p95 39.4, n=20) | 4.2 ms (p95 15.2, n=20) | **2680.8×** | **7203.5×** |
| launch app | `xcrun simctl launch <udid> com.apple.Preferences` | 416.3 ms (p95 1276.5, n=10) | 382.6 ms (p95 466.2, n=10) | 332.0 ms (p95 536.4, n=10) | **1.1×** | **1.3×** |
| launch app + first screen | `` | - | 2382.1 ms (p95 4810.4, n=5) | - | - | - |
| boot (device kept booted by agemu) | `xcrun simctl bootstatus <udid> -b` | 227.5 ms (p95 700.1, n=10) | 17.7 ms (p95 23.8, n=10) | 3.6 ms (p95 13.6, n=10) | **12.9×** | **63.5×** |
| boot (cold, from shutdown) | `xcrun simctl boot + simctl bootstatus -b` | 17757.5 ms (p95 17757.5, n=2) | ERR: bin/ios/agemu boot ios:ED6C55BD-83FB-49B9-93A2-54BA0AD521DD -d ios:ED6C55BD-83FB-49B9-93A2-54BA0AD521DD: exit status 1: error: device ED6C55BD-83FB-49B9-93A2-54BA0AD521DD not agent-ready after 600000 ms: no frontmost application (is SpringBoard running?) | - | - | - |
| reset to clean state | `simctl shutdown + erase + boot + bootstatus -b` | ERR: before: bin/ios/agemu boot ios:ED6C55BD-83FB-49B9-93A2-54BA0AD521DD -d ios:ED6C55BD-83FB-49B9-93A2-54BA0AD521DD: exit status 1: error: device ED6C55BD-83FB-49B9-93A2-54BA0AD521DD not agent-ready after 600000 ms: no frontmost application (is SpringBoard running?) | 1762.9 ms (p95 1762.9, n=2) | - | - | - |

The cold-boot and reset rows of run A failed. They exposed two agemu bugs, both fixed and
re-measured in runs B and C:

- after a cold boot, `CoreSimulatorBridge` inside the simulator kept answering "no frontmost
  application" (agemu waited 10 minutes); the helper now restarts the bridge (see above);
- after `simctl` rebooted the device behind agemu's back, the helper's HID connection was dead;
  it now reconnects when `-[SimDevice lastBootedAt]` changes.

### Run B: boot of a device agemu keeps booted (after the fixes)

### ios benchmark (2026-11-05)

Host: Apple M2 Pro, macOS 26.6, arm64. Device: `ios:ED6C55BD-83FB-49B9-93A2-54BA0AD521DD`. Median of n runs (p95 and n in parentheses).

| Operation | Baseline tool | Baseline | agemu CLI | agemu daemon/MCP | Speedup (CLI) | Speedup (daemon) |
|---|---|---|---|---|---|---|
| boot (device kept booted by agemu) | `xcrun simctl bootstatus <udid> -b` | 362.4 ms (p95 446.2, n=10) | 14.6 ms (p95 39.6, n=10) | 5.1 ms (p95 8.9, n=10) | **24.8×** | **70.6×** |
| reset to clean state | `simctl shutdown + erase + boot + bootstatus -b` | ERR: before: bin/ios/agemu tap --id com.apple.settings.general -d ios:ED6C55BD-83FB-49B9-93A2-54BA0AD521DD: exit status 1: error: HID delivery: Mach port invalid, device disconnected | ERR: before: bin/ios/agemu tap --id com.apple.settings.general -d ios:ED6C55BD-83FB-49B9-93A2-54BA0AD521DD: exit status 1: error: HID delivery: Mach port invalid, device disconnected | - | - | - |

The reset row in run B failed on the HID bug above; it was fixed before run C.

### Run C: cold boot and reset (after both fixes)

### ios benchmark (2026-11-05)

Host: Apple M2 Pro, macOS 26.6, arm64. Device: `ios:ED6C55BD-83FB-49B9-93A2-54BA0AD521DD`. Median of n runs (p95 and n in parentheses).

| Operation | Baseline tool | Baseline | agemu CLI | agemu daemon/MCP | Speedup (CLI) | Speedup (daemon) |
|---|---|---|---|---|---|---|
| boot (cold from shutdown) | `xcrun simctl boot + simctl bootstatus -b` | 12437.6 ms (p95 12437.6, n=2) | 40096.8 ms (p95 40096.8, n=2) | - | **0.3×** | - |
| reset to clean state | `simctl shutdown + erase + boot + bootstatus -b` | 194979.4 ms (p95 194979.4, n=2) | 5204.8 ms (p95 5204.8, n=2) | - | **37.5×** | - |

With the final helper (the bridge repair fires once SpringBoard has run for 3 s), three more
cold boots through `agemu boot` took 35.5 s, 33.4 s and 34.7 s; each needed one bridge restart.

### Summary against the 10x target

| Operation | Standard tool | Result | Meets 10x? |
|---|---|---|---|
| screenshot (PNG) | `simctl io screenshot` | 10.7x (CLI), 18.5x (daemon/MCP) | yes, by a small margin for the CLI on this loaded host |
| screenshot (JPEG) | `simctl io screenshot --type=jpeg` | 14.8x / 22.8x | yes |
| UI tree | `maestro hierarchy` | 151x / 165x | yes |
| find element x10 in one flow | Maestro flow, 10 `assertVisible` | 39x / 46x | yes |
| tap | Maestro, one `tapOn` per call | ~2750x / ~47000x | yes (the baseline is dominated by JVM and XCUITest runner start-up) |
| tap x10 in one flow | Maestro flow, 10 `tapOn` | 304x / 1216x | yes; per tap inside a flow Maestro costs ~0.8 s ((47.9 s - 40.4 s) / 9), agemu 16 ms (CLI) |
| swipe (300 ms gesture) | Maestro `swipe` | 67x / 70x | yes; agemu's time includes the 300 ms gesture itself |
| type text | Maestro `inputText` | 2681x / 7204x | yes |
| boot, device kept booted | `simctl bootstatus -b` | 12.9-24.8x / 63-71x | yes: this is agemu's boot path (devices stay booted, `boot` only verifies agent-readiness) |
| boot, cold from shutdown | `simctl boot` + `bootstatus -b` | **0.3x** (40 s vs 12 s) | **no**. Both use the same CoreSimulator boot. agemu waits for *agent-ready* (SpringBoard answering accessibility, framebuffer readable, HID connected); `bootstatus` returns earlier and checks none of that, and after a cold boot the simulator's accessibility bridge needs a restart before it answers. There are no iOS Simulator snapshots to restore from. |
| reset to a clean state | `simctl shutdown` + `erase` + `boot` + `bootstatus -b` | 37.5x (5.2 s vs 195 s) | yes, as a soft reset (see "Fast boot and reset" for exactly what is and is not reset). Warm, the reset took 0.9-1.1 s. |
| launch app | `simctl launch` | **1.1-1.3x** | **no**. agemu makes the same `-[SimDevice launchApplicationWithID:]` call that simctl makes. The call takes ~300 ms because FrontBoard starts the app process inside it; the ~100 ms that `simctl`'s own start-up adds is all agemu can remove. After that it is the app's own start-up: Settings needs ~2 s to show its list (`launch app + first screen` row). |

agemu's `ui` took 46-69 ms through the daemon when the host was quieter
(`internal/ios` measured 149 accessibility round trips of ~0.5 ms each for the Settings root
screen); under run A's load the median was ~190-210 ms.

## Caveats

- **Simulator, not a device.** The Simulator runs the iOS userland natively on the Mac's CPU.
  There is no cellular, no real camera, no Secure Enclave-backed Face ID (it can be simulated),
  push notifications must be simulated, GPU and memory limits differ, and performance is the
  Mac's, not an iPhone's. Test on devices before shipping.
- **Private APIs.** CoreSimulator, SimulatorKit and AccessibilityPlatformTranslation are private
  frameworks; their classes and functions are looked up at run time and every call is wrapped
  in `@try/@catch`. Verified with Xcode 26.6 (CoreSimulator 1051.55, SimulatorKit 955.7), iOS
  26.5 runtime, macOS 26.6. Xcode updates can break them: idb documents that on Xcode 27 /
  CoreSimulator 1155.4 the legacy Indigo HID path no longer delivers buttons and keys and
  touches become unreliable (`dtuhidd` replaces it). `agemu-ios-helper` reports the framework
  versions it loaded (`hello`).
- **HID keyboard.** Typing uses a US hardware keyboard layout. While a hardware keyboard is in
  use iOS hides the software keyboard, as it does when you type on a Mac keyboard in
  Simulator.app, and iOS's hardware-keyboard auto-capitalization (on by default) applies, so
  `type hello` into an empty field with sentence capitalization can produce "Hello", exactly as
  on a device with a keyboard.
- **Disk.** Every simulator that has booted once costs 1-2 GB: on its first boot iOS stages
  about 1 GB of Siri, text-to-speech and linguistic assets into
  `data/private/var/MobileAsset` (`mobileassetd`; `simctl boot --disabledJob` does not stop it).
  `xcrun simctl erase <udid>` (on a shut-down device) returns it to ~17 MB.
- **First boot** of a new or erased simulator migrates data and took 1.5-3 minutes on this
  (heavily loaded) host; agemu waits up to 10 minutes. Later boots take seconds.
- **Launch time is the app's.** `agemu launch` makes the same CoreSimulator call as
  `simctl launch`; the call itself (FrontBoard starting the process) dominates, so it is not
  faster. What an app does after launch (Settings takes ~2 s to show its list on this host) is
  the app's own time.
- **Accessibility shows what iOS exposes.** Lists expose their visible cells only; scroll to see
  more. Elements without accessibility (custom drawing) are invisible to `ui`; use screenshots.
- **Rotation** is not handled: frames and taps assume portrait.

## Running

```sh
make build                      # agemu, agemu-bench, agemu-ios-helper into bin/
bin/agemu create --platform ios --profile iphone-17-pro
bin/agemu boot agemu-iphone-17-pro
bin/agemu launch com.apple.Preferences
bin/agemu wait --id com.apple.settings.general
bin/agemu ui
bin/agemu tap --text General
bin/agemu screenshot -o shot.png
bin/agemu profile iphone-se-3   # switches to agemu-iphone-se-3

scripts/e2e-ios.sh              # end-to-end proof (uses bin/ios/agemu; AGEMU_BIN overrides)
bin/agemu-bench --platform ios --device ios:<udid> --agemu bin/agemu --n 20
go test ./internal/ios/...      # unit tests (no simulator needed)
```

The helper is found next to the `agemu` binary, or at `$AGEMU_IOS_HELPER`. Its source is
`ios-helper/` (`swift build -c release`).

## Credits

The private-API usage (Indigo message layout, framebuffer callbacks, the AXPTranslator token
delegate) follows the public reverse-engineering in
[facebook/idb](https://github.com/facebook/idb)'s FBSimulatorControl (MIT license, Copyright (c)
Meta Platforms, Inc. and affiliates). agemu's code is its own; where a byte layout comes from
idb's `Indigo.h` / `SimulatorIndigoHID.swift`, the source says so.
