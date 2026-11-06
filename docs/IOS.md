# iOS backend

agemu drives the **real iOS Simulator runtime** through CoreSimulator, the same runtime
Simulator.app and `xcrun simctl` use. Apps run unmodified. No Simulator.app window is opened:
devices boot headless, and agents see the screen only through screenshots and the UI tree.
All the speed comes from the control plane.

```
agemu CLI / MCP ──unix socket──► agemud daemon (Go, internal/ios)
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
| screenshot | `simctl io screenshot`: new process, connects to the device, renders, encodes | The helper keeps the main display's framebuffer **IOSurface** (and its change callbacks) for the life of the process. A screenshot copies the surface (~2 ms) and encodes the copy: PNG with agemu's parallel encoder (`AGPNG.c`: the image is cut into 12 bands that are filtered and deflated on separate cores at zlib level 1, then joined into one valid zlib stream, as pigz does), JPEG with Core Image's encoder (~7 ms for 1206×2622 vs ~10 ms with ImageIO; its distance to the lossless frame, mean abs. difference 0.30, matches simctl's own JPEG at 0.29; `AGEMU_IOS_JPEG=imageio` selects ImageIO). Core Image is warmed up when the helper starts. |
| tap / swipe | no simctl command; Maestro starts a JVM and an XCUITest runner | Indigo HID touch messages sent through SimulatorKit's `SimDeviceLegacyHIDClient`, the same channel Simulator.app uses for mouse clicks. Pixels are converted to screen ratios. A swipe is a touch-down, one move per ~16 ms, and a touch-up. **A tap first waits until the screen has had no big change for 48 ms** (at most 1 s): UIKit ignores a tap that lands during a transition, and a tap on a list still coasting after a swipe only stops it. A background thread samples the framebuffer surface's seed every 8 ms and, on every new frame, a 48×104 grid of pixels: a change of more than 0.4% of the grid is *big* (scrolling, transitions); a blinking cursor, a spinner or a ticking countdown are not, so they no longer hold a tap for the full timeout. A tap returns once its first frame shows (at most 120 ms). **A tap by selector** settles once (at most 1.5 s), re-reads the tree until the element's frame repeats if the screen did not go still, and waits up to 3 s for a disabled element to become enabled. On a still screen the tap then keeps the helper's settle, which returns at once unless a transition started during the tree read. An element confirmed at rest by its repeating frame is tapped at once, since a second settle on a screen that keeps changing would only time out. `AGEMU_IOS_TAP_SETTLE=0` turns the helper's settle off. |
| type | Maestro `inputText` | Inserts the text at the cursor of the focused field by **pasting** it (`simctl pbcopy`, then Cmd-V): the app gets the whole text in one change, as with Android's set-text path. A single character is one key event. `type --keys` sends one HID key event per character (US layout, paced: each acknowledged, then 10 ms). Both wait for a still screen first (no big change; at most 600 ms) and return once the app has drawn the change (at most 500 ms). A paste is checked against the focused text field's text and repeated once if it did not arrive. Key events were not reliable in React Native controlled inputs (Expo development build of a real app): characters were lost or doubled at every pace tried, and a lost character in an e-mail field signed in to a different account; the paste always arrived whole. |
| key | — | `home` and `lock` are the hardware buttons; `enter`, `delete`, `tab`, `escape`, arrows are keyboard keys; `volumeup/volumedown` are consumer-page HID usages. iOS has no back key: `back` taps the navigation bar's back button (UIKit's `BackButton` identifier) when the screen has one, and otherwise does a **left-edge swipe**, flagged as an edge gesture, which triggers the navigation controller's interactive pop like a thumb does. On the loaded test host one edge swipe was not recognized, hence the tap first. |
| UI tree | `maestro hierarchy` (JVM + XCUITest snapshot) | `AXPTranslator` from AccessibilityPlatformTranslation (the framework Simulator.app's accessibility support uses), with agemu as its token delegate: every translator request is forwarded to the device with `-[SimDevice sendAccessibilityRequestAsync:...]`. **All attributes of an element come in one request** (`accessibilityMultipleAttributes:`, translator request type 5, which runs `AXUIElementCopyMultipleAttributeValues` inside the simulator) instead of one request per attribute: 2 requests per element (the batch plus the translator's own remote-element check) instead of 11, with byte-identical output. The app's bundle id is one more request (AXP attribute 6), so the tree names the frontmost app without `launchctl`. On iPad the frontmost app the bridge reports is checked with a hit test on the screen (see "iPad: the app that owns the screen"). Every read has a deadline (20 s, or the caller's; 5 s per request) and fails with an error rather than return a partial tree. Off-screen elements are still read (and dropped in Go): pruning them by frame could drop an on-screen child of an off-screen container. |
| launch / terminate / install | `simctl launch` etc. | The same CoreSimulator calls, made in-process by the helper. `launch` takes process arguments and `--relaunch` (`agemu launch <bundle> --relaunch -- --initialUrl http://127.0.0.1:8081` opens an Expo development client on its Metro), and the MCP tool also takes an environment. |
| boot | `simctl boot` + `simctl bootstatus -b` | `-[SimDevice bootWithOptions:]` (persisted, headless), then agemu returns once the **home screen is drawn**: CoreSimulator's boot status is "finished" (what `bootstatus` waits for), the framebuffer is readable, the HID client is connected and SpringBoard's accessibility tree answers (see "Cold boot"; `AGEMU_IOS_BOOT_WAIT_AX=0` returns before the last step). A device that is already booted is ready in milliseconds, and agemu keeps devices booted (see below). |
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
and zero-sized leaves are dropped. Three quirks of the iOS 26 accessibility tree are handled:

- Some containers report no children although they have some (the toolbar hosting the
  Settings search field reports empty `AXChildren` and `AXChildrenInNavigationOrder`). For an
  `AXGroup` with no children the helper hit-tests three points along its centre line and adds
  the distinct elements it finds inside the group.
- Some collection views are **opaque element providers**: they report no `AXChildren` (batched
  or one attribute at a time) and expose their cells only through
  `AXPAttributeVisibleOpaqueElements` (AXP attribute 58; `AXPAttributeIsOpaqueElementProvider`,
  29, answers 1). The iPad Settings sidebar is one: its rows name it as their parent, but the
  centre-line hit tests found only the one row on that line; attribute 58 returns its 16
  visible rows in one request. A childless `AXGroup` taller than 132 pt (three 44 pt rows) is
  asked for its opaque elements first; shorter ones (the iPhone toolbar, 86 pt) keep the
  hit-test sampling alone, so the iPhone read did not change (38 requests, byte-identical
  output).
- Apps load their accessibility runtime only while `ApplicationAccessibilityEnabled` is on in
  the simulator. XCUITest-based tools (Maestro, `xcodebuild test`) switch it on for their
  session and **off** when they finish, after which newly launched apps expose an empty tree.
  Whenever a live app exposes an empty root, agemu switches it on and posts `com.apple.accessibility.cache.app.ax` / `com.apple.accessibility.cache.ax` so the
  running app loads its accessibility runtime without a relaunch. Any UI automation tool,
  XCUITest included, does the same.

### Accessibility bridge: what was slow and how it is repaired

Inside the simulator, `CoreSimulatorBridge` answers accessibility requests. Its iOS-side
translator asks SpringBoard (the "system app") for its focused applications (iOS attribute
1102) and returns the first one. Measured, and read from the disassembly of
`-[AXPTranslator_iOS processFrontMostApp:withiOSAttribute:]` and
`-[AXPTranslator_iOS _attemptToResetSystemWideElement]`:

- Until SpringBoard's own accessibility server (`com.apple.accessibility.AXSpringBoardServer`)
  is up, the bridge answers "no frontmost application" within milliseconds. It retries finding
  SpringBoard **every 5 s** by itself (`_resetSystemWideElementAfterDelay`: `dispatch_after`
  5 000 000 000 ns), so accessibility starts answering up to 5 s after SpringBoard's server is
  up, typically 1.5-3 s after boot finished.
- In one boot under heavy load (load average ~300) data migration ran, SpringBoard was
  relaunched during the boot, and the bridge answered "no frontmost application" for ~30 s
  after its first request, to every client including a freshly started process. Restarting
  the bridge (`launchctl kickstart -k`, 1-3 s, up to 9 s under load) clears that.
- The old agemu polled accessibility from the moment the device was booted and restarted the
  bridge after a fixed wait: cold boots took 33-40 s.

What agemu does now:

- It does not ask accessibility before CoreSimulator's boot status is finished
  (`-[SimDevice bootStatus]`, `SimDeviceBootInfo`); then it probes every 100 ms with a 1.5 s
  budget.
- It restarts the bridge only if accessibility still says "no frontmost application" 6 s after
  boot finished (after the bridge's own 5 s retry had its chance), then every 10 s, at most 3
  times. Measured: restarting as soon as the first probe failed made boot-finished to
  accessibility 4.0-6.9 s (3 boots); waiting made it 1.5-2.8 s (3 boots, no restart needed).
- After boot, failures are noted and checked in the background: when accessibility has said
  "no frontmost application" for 2 s while SpringBoard has run for 2 s, the bridge is restarted
  (at most twice per SpringBoard instance, not within 15 s of the previous restart). A failing
  read returns its error at once instead of waiting for `launchctl`.

### iPad: the app that owns the screen

The bridge's frontmost application is the **first** of SpringBoard's focused applications
(iOS attribute 1102; `-[AXPTranslator_iOS processFrontMostApp:]` passes 0x44e to
`processFrontMostApp:withiOSAttribute:`, which takes `firstObject`). On iPad (iOS 26.5) that
list starts with the dock's `com.apple.DocumentManager.DockFolderViewService`: always on the
home screen (3 of 3 tries) and for about a second after an app comes to the front, after
which the app is first (3 of 3 launches). Its tree is one empty application element, so
`agemu ui` was empty, `tap --text` found nothing and `reset` could not confirm SpringBoard.
On iPhones the bridge reports the app itself.

A **system-wide hit test** (translator request type 6, `objectAtPoint:displayId:`) is answered
by the process whose window is at that point: on the iPad home screen SpringBoard (a home
screen icon, the page indicator, a dock icon) or, over a widget, the widget's renderer
(`com.apple.chrono.WidgetRenderer-Default`); in front of Settings, Settings. (The old note that
hit tests also go to the dock's service was wrong.) An application element for any pid is one
request (type 1 with parameters `{pid}`, as idb's FBSimulatorAX sends for an explicit pid;
`-[AXPTranslator translationApplicationObjectForPid:]` returned nil through a token delegate).
Measured application frames: SpringBoard and Settings 744x1133 pt (the whole iPad mini screen),
the widget renderer 260x260 pt, a suspended app 0x0.

So on iPad the helper:

1. asks for the frontmost application (1 request), as on iPhone,
2. hit-tests the screen centre (1 request). If the hit belongs to that process, it is kept,
3. otherwise reads the hit process's application element and its frame (3 requests: the
   element, the frame, the translator's remote-element check) and uses
   it if it covers at least 40% of the screen (a widget or other hosted content does not; the
   next of four more points, at 30% and 70% across and down, is tried),
4. finds nothing (right after a launch every hit test answers nothing for about a second):
   keeps the reported application and marks the read unconfirmed. `ui` then reads again every
   100 ms, at most 3 s within its budget, while the unconfirmed tree is empty. `ui` right after
   `launch com.apple.Preferences` took 2.7-4.6 s and returned Settings (3 of 3); Settings'
   own first reads took 0.7-1.1 s of that. Before, it returned the empty tree at once.

iPhones skip steps 2-4. `reset` waits for SpringBoard on iPad as on iPhone; the old iPad
exception (accept a frontmost `...ViewService` once the screen was still) is gone.

## Fast boot and reset

There are no snapshots for iOS Simulators: a cold boot costs what CoreSimulator and iOS take,
and 10x on a cold boot is not possible. **The 10x path is keeping devices booted**: `agemu
boot` on a running device answers in 0.3 ms through the daemon and 4.5 ms through the CLI
(400x and 24x `simctl bootstatus -b`), and agemu never shuts a device down on its own except to
make room (below). Boot once, then `reset` between tasks.

### Cold boot

`agemu boot` of a shut-down device runs these steps in parallel and logs their timeline to the
daemon log (`ios boot <name>: ... timeline: ...`):

| step | ms since the boot call (final benchmark run, 5 boots) |
|---|---|
| `bootWithOptions` returns, state Booted | 275-300 |
| HID client connected | Booted + 4-6 |
| boot status: waiting on data migration, then on SpringBoard | 1250-2900 |
| boot status finished (what `simctl bootstatus -b` waits for) | 4350-10550 |
| framebuffer connected | finished + 10-120 |
| interactive (screenshots, taps, launches work) | framebuffer + 0-6 |
| **accessibility answers (SpringBoard drawn): `boot` returns** | finished + 1.5-5.5 s |

- The framebuffer is connected only once boot finished: a framebuffer client makes the
  simulator render every boot frame. In one comparison round, boot finished at 12.7 s with the
  framebuffer connected during boot and at 4.7 s with it connected after; after boot the
  connection takes 10-200 ms. (`AGEMU_IOS_CONNECT_AFTER_BOOT=0` connects during boot.)
- `boot` returns when SpringBoard's accessibility tree answers, i.e. the home screen is drawn.
  Returning at "interactive" instead was 1.5-5.5 s faster, but a screenshot taken right after
  showed the boot spinner (seen in the integration e2e on the iPhone SE), which misleads an
  agent. `AGEMU_IOS_BOOT_WAIT_AX=0` restores the early return for callers that only launch apps.
- `boot` of a device that is already booted asks accessibility once (one request), or not at
  all when it answered for this boot in the last 5 s.
- A boot that comes up without a display (seen once, after an overlapped profile switch) is
  shut down and booted again once.

Cold boots of the same device, alternating agemu and `simctl boot + bootstatus -b` (4 rounds,
load 38-73): agemu 6.75, 4.85, 11.85, 5.71 s (median 6.2 s); simctl 5.24, 11.10, 12.92,
11.80 s (median 11.5 s). Either way the time is iOS's own boot.

### Profile switch

A profile switch to another device type is a cold boot of that type's simulator. The other
agemu simulators beyond the limit are shut down first. The limit is `AGEMU_IOS_MAX_BOOTED`, or
2, or **1 while memory is short** (macOS memory pressure warn or critical, or less than half of
the memory available): on the test host an iPhone SE booted in 18.5 s next to a running iPhone
17 Pro and in 6.3 s alone. With memory to spare, the old device's shutdown overlaps the new
boot, but only after the new device's boot call returned: a boot call issued while another
simulator was shutting down failed with "Invalid argument", and that device's next boot came up
without a display (`AGEMU_IOS_OVERLAP_SWITCH=1/0` forces the overlap on or off).

### Soft reset

`simctl erase` needs a shutdown, an erase and a full first boot (data migration). `agemu reset`
cleans the running device instead:

1. terminates every app (system and user), 8 at a time. Terminating an app that is not running
   fails in about a millisecond, so this is cheaper than asking launchctl what runs. The
   built-in app list is fixed per runtime and cached (the CoreSimulator query takes 1-2 s);
   user apps are found by reading `Containers/Bundle/Application/*/*.app/Info.plist` in the
   device's data directory on the host,
2. uninstalls every user-installed app, which deletes its data and app-group containers,
3. resets privacy permissions (`simctl privacy reset all`), the keychain
   (`simctl keychain reset`) and the pasteboard, in parallel with 1-2,
4. presses home, waits until accessibility reports SpringBoard (`com.apple.springboard`) as the
   frontmost app (on iPad too, found by the hit test above), then until the screen has been
   still for 300 ms. The screen moves for ~1.3 s
   after the press; an app launched sooner (the old fixed 300 ms) sometimes started in the
   background, or failed to launch while a terminated process was still exiting. `launch` also
   retries a "failed to launch" twice (after 0.3 s and 0.7 s) when the app is installed.
   10 of 10 reset-then-launch rounds brought Settings to the front.

The daemon log has each phase, e.g. `ios reset <udid>: 1736 ms (app lists 1, terminate 25 apps
31, uninstall [] 0, simctl privacy/keychain/pbcopy done at 158, home 1578)`.

It does **not** reset what `simctl erase` would: data stored by built-in apps (Settings search
history, Safari, Contacts, Photos...), system settings an agent changed (language, appearance,
location...), and media added with `simctl addmedia`. Use `agemu shutdown` +
`xcrun simctl erase <udid>` for a full wipe.

### Bounded calls

No agent-facing call waits silently. Every helper command has a timeout (launch 60 s, install
3 min, terminate 20 s, UI read 20 s, screenshot 15 s, tap 15 s, ...) unless the caller set an
earlier deadline, and fails with an error that names what it waited for, e.g. `launch <udid>:
gave up after 60.0 s waiting for CoreSimulator to launch com.apple.Preferences; the simulator is
overloaded or stuck (the request may still complete in the background)`. `wait` bounds each UI
read by its own deadline (at least 1 s) and reports the last read error on timeout. Only a cold
boot may take up to 10 minutes (a first boot migrates data).

## Lean simulators

A booted iOS 26.5 simulator runs the whole iOS userland. Measured headless on
`agemu-iphone-17-pro` with no app:
- about 240 processes and **4.0 GB** of memory (the sum of phys_footprint, which is what macOS
  memory pressure counts);
- 1.6 cores busy for the first minute after boot (media analysis, diagnostics, asset downloads,
  Siri voices, the wallpaper gallery);
- 20% of a core when idle.

agemu's own helper and daemon take 21 MB and 0.1% of a core. Most of the rest serves features an
app under test does not use, so agemu turns them off in its own simulators (`agemu-*`):

1. **32 launchd jobs are off** (`internal/ios/lean.go`, grouped by feature). agemu runs
   `launchctl disable` inside the device, so the override persists and later boots start without
   the jobs, and `launchctl bootout` for the ones running at that moment. The first time on a
   device this takes about 6 s (the helper runs launchctl in the device, 8 at a time); later
   boots check it with one call. `simctl boot --disabledJob` would be the documented way, but on
   CoreSimulator 1051 it had no effect: 25 of the 32 jobs still ran.
2. **The wallpaper gallery is stopped after a cold boot.** PosterBoard starts one extension per
   wallpaper in its gallery (6-16 processes of 35-75 MB) within seconds of boot. agemu stops
   them 3, 8, 20 and 45 s after the boot, and iOS relaunches the one the lock screen needs: after
   a stop, the lock screen drew its wallpaper, clock and notifications. PosterBoard itself stays
   on. With PosterBoard disabled, the lock screen was not drawn: the UI tree reported the lock
   screen while the screenshot showed the home screen. PosterBoard starts some gallery
   extensions again later for a while, and iOS ends them again.

**What a lean simulator does not do.** These need `AGEMU_IOS_LEAN=0`; the jobs are back from the
next boot:
- home screen widgets (they show empty) and WidgetKit timelines;
- Siri, App Intents donations, text-to-speech voices and speech recognition;
- on-demand system assets (downloadable fonts, dictation and linguistic models). On a first boot
  `mobileassetd` would also stage about 1 GB into the device;
- News, HomeKit, Screen Time, significant locations and Apple Intelligence models;
- photo analysis (memories, people), iCloud Mail and the music library.

HealthKit (`healthd`), CoreSpotlight indexing, the share sheet, Wallet, notifications and push
stay on.

**Measured.** The same simulator, booted 3 times each way, interleaved, with the zuapp flow (see
"A real app" below) run right after boot and once more, then the app left open:

| | all jobs (`AGEMU_IOS_LEAN=0`) | lean |
|---|---|---|
| memory, the app open | 4.82 GB | **3.90 GB** |
| processes | 219 | 169 |
| simulator CPU during the flow right after boot (45 s window) | 148 s | **101 s** |
| simulator CPU during the flow, settled | 34 s | 33 s |
| cold boot until agent-ready | 9.3 s | 8.4 s |
| the flow | 25.7 s | 25.9 s (the app's pace) |

Without an app, in single runs: 4.0 GB and 20% of a core idle with all jobs; 2.5 GB and 1.5%
lean. The first minute after a lean boot took 26-116% of a core, depending on how many
wallpaper extensions that boot started.

All 11 runs of the zuapp flow on lean simulators passed; 1 of them needed a retry (a tap with
no effect, see "Taps that did nothing"). With all jobs, 9 of 10 passed and 1 failed: right after
boot the app showed an empty screen for 30 s.

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

All numbers are real output of `bin/ios/agemu-bench --platform ios --device ios:<udid> --agemu
bin/ios/agemu --n 20` on one `agemu-iphone-17-pro` simulator (iOS 26.5), Xcode 26.6, Apple M2
Pro (16 GB). Baseline and agemu run back to back on the same device. The Mac was loaded by
unrelated work the whole time (an Android emulator, browsers, other sessions' simulators;
load average 47-78 during this run, swap nearly full), so every number is noisy; each row
compares the two tools under the same conditions. Rows with n=2 (cold boot, profile switch,
reset) are the slower of two runs.

### Final run (2026-11-06, commit eab26c6)

| Operation | Baseline tool | Baseline | agemu CLI | agemu daemon/MCP | Speedup (CLI) | Speedup (daemon) |
|---|---|---|---|---|---|---|
| screenshot | `xcrun simctl io <udid> screenshot (png)` | 146.1 ms (p95 265.9, n=20) | 9.7 ms (p95 12.2, n=20) | 5.6 ms (p95 8.4, n=20) | **15.0×** | **25.9×** |
| screenshot-jpeg | `xcrun simctl io <udid> screenshot --type=jpeg` | 124.4 ms (p95 139.5, n=20) | 11.2 ms (p95 15.1, n=20) | 6.6 ms (p95 10.4, n=20) | **11.1×** | **18.9×** |
| ui-tree | `maestro hierarchy (JVM + XCUITest runner per call)` | 9750.5 ms (p95 15018.0, n=3) | 32.0 ms (p95 96.9, n=20) | 25.7 ms (p95 54.5, n=20) | **304.2×** | **378.8×** |
| find-element x10 | `one maestro flow: 10x assertVisible` | 10529.3 ms (p95 10699.7, n=3) | 335.0 ms (p95 508.7, n=3) | 250.5 ms (p95 266.1, n=3) | **31.4×** | **42.0×** |
| tap | `maestro test (1x tapOn point)` | 9804.9 ms (p95 10243.8, n=3) | 4.8 ms (p95 78.3, n=20) | 0.6 ms (p95 3.3, n=20) | **2053.4×** | **15865.6×** |
| tap x10 | `one maestro flow: 10x tapOn point` | 18392.5 ms (p95 19613.9, n=3) | 44.6 ms (p95 74.5, n=3) | 8.5 ms (p95 18.2, n=3) | **412.6×** | **2175.6×** |
| swipe (300 ms gesture) | `maestro test (1x swipe, 300 ms)` | 11325.9 ms (p95 11724.3, n=3) | 417.9 ms (p95 522.1, n=10) | 419.1 ms (p95 430.4, n=10) | **27.1×** | **27.0×** |
| type text | `maestro test (inputText "hello")` | 9947.1 ms (p95 10999.8, n=3) | 4.8 ms (p95 38.4, n=20) | 1.6 ms (p95 3.8, n=20) | **2088.4×** | **6098.8×** |
| launch app | `xcrun simctl launch <udid> com.apple.Preferences` | 323.5 ms (p95 337.5, n=10) | 329.1 ms (p95 347.6, n=10) | 332.4 ms (p95 347.0, n=10) | **1.0×** | **1.0×** |
| launch app + first screen | `` | - | 1938.3 ms (p95 3383.8, n=5) | - | - | - |
| boot (device kept booted by agemu) | `xcrun simctl bootstatus <udid> -b` | 108.9 ms (p95 223.6, n=10) | 4.5 ms (p95 6.9, n=10) | 0.3 ms (p95 0.4, n=10) | **24.3×** | **403.2×** |
| boot (cold from shutdown) | `xcrun simctl boot + simctl bootstatus -b` | 6845.7 ms (p95 6845.7, n=2) | 5859.5 ms (p95 5859.5, n=2) | - | **1.2×** | - |
| boot (cold) + first UI tree | `` | - | 12597.8 ms (p95 12597.8, n=2) | - | - | - |
| profile switch to iphone-se-3 (cold) | `simctl shutdown <this> + simctl boot <se> + simctl bootstatus <se> -b` | 14172.9 ms (p95 14172.9, n=2) | 8261.6 ms (p95 8261.6, n=2) | - | **1.7×** | - |
| reset to clean state | `simctl shutdown + erase + boot + bootstatus -b` | 38151.7 ms (p95 38151.7, n=2) | 3177.6 ms (p95 3177.6, n=2) | - | **12.0×** | - |

`boot (cold) + first UI tree` is `agemu boot` followed by `agemu ui`: the UI read waits for
accessibility to finish warming up. The profile switch row starts with the iPhone 17 Pro booted
and the iPhone SE shut down; both tools shut the 17 Pro down and boot the SE (agemu does so
because memory was short, see "Profile switch").

### Before and after this round of work

| Operation | Before (2026-11-05) | After (2026-11-06) |
|---|---|---|
| boot, cold, until `boot` returns | 33-40 s (0.3x simctl): waited for accessibility, bridge restarted after a fixed wait | 5.9 s bench (1.2x); 4.9-11.9 s interleaved vs simctl 5.2-12.9 s (median 6.2 vs 11.5 s) |
| boot, cold, until the UI tree is read | 33-40 s | 7.8-13.8 s |
| boot, device kept booted (CLI / daemon) | 17.7 ms / 3.6 ms | 4.5 ms / 0.3 ms |
| profile switch (cold boot of another device type) | 44-70 s in the e2e | 8.3 s bench (1.7x simctl shutdown+boot+bootstatus); 7.8-8.8 s in the final e2e |
| reset | 11-12 s in the e2e (load ~400), ~1 s warm | 1.7-3.2 s (the 1.3 s wait for a still screen after home is new, see "Soft reset") |
| UI tree (Settings root): translator requests | 149-181 | 21-37 (identical output, byte for byte) |
| UI tree, daemon | 190 ms (load ~400), 46-69 ms quieter | 25.7 ms |
| screenshot PNG, CLI / daemon | 21.7 / 12.6 ms (10.7x / 18.5x) | 9.7 / 5.6 ms (15.0x / 25.9x) |
| screenshot JPEG, CLI / daemon | 17.2 / 11.1 ms | 11.2 / 6.6 ms (11.1x / 18.9x) |
| launch | 1.1-1.3x | 1.0x (bounded by the CoreSimulator call, see Caveats) |

The "before" UI-tree and screenshot numbers came from a more loaded host (load ~400 at times)
than the "after" ones (load 47-78); the request counts and the per-step timelines are what
attribute the gains to agemu.

### iPad UI tree (2026-11-06, commit 60c8a22)

`ui` 20 times through the CLI and 20 times over the daemon socket, on the same screens of
`agemu-ipad-mini`, the build of 01237d3 and the build of 60c8a22 back to back (load average
5-8). The "before" trees were the wrong app or incomplete, so they are cheaper:

| screen | before: tree | before: daemon / CLI | after: tree | after: daemon / CLI |
|---|---|---|---|---|
| home screen | DockFolderViewService, 1 element | 2.8 / 7.7 ms | SpringBoard, 16 elements (icons, widgets, dock) | 24.2 / 29.8 ms |
| Settings root | Settings, 17 elements (1 of 16 sidebar rows) | 38.3 / 44.8 ms | Settings, 32 elements (all sidebar rows) | 58.9 / 62.1 ms |

On `agemu-iphone-17-pro` (Settings root, load 34-52) the two builds read the same 16 elements
with the same 38 requests and byte-identical `ui --json`: daemon 26.1 / 25.1 ms before and
25.3 / 24.5 ms after (two rounds), CLI 31.5 / 30.4 ms before and 30.8 / 30.8 ms after.

### End-to-end run

`scripts/e2e-ios.sh` (103 checks) passed on commit 60c8a22 (load average 17 at the start; the
agemu commands took 104 s in all). Its iPad section reads the home screen (SpringBoard, the
App Library button), launches Settings and checks the sidebar rows in the tree, taps
Accessibility by id and General by text (the sidebar row, at 300,725 of 1488x2266) and waits
for About, presses home and waits for the home screen, and resets (SpringBoard confirmed, 2.1
s). The iPhone resets took 2109 / 1823 ms and the profile switches to iphone-se-3 / ipad-mini
/ iphone-17-pro 8786 / 15764 / 8928 ms (cold boots; the iPad's was slower than the 8815 ms
of the run below).

Before that, the script (78 checks, with an unchecked UI-tree read on iPad) passed on commit
eab26c6 in about 1.5 minutes (load average 6 at the start, 57 at the end). The long tails of
the run before it (load ~400) against that one:

| step | earlier e2e | eab26c6 e2e |
|---|---|---|
| `reset` (first / after the profile loop) | 12483 / 11637 ms | 1766 / 1752 ms |
| `ui` right after reset | 27615 ms | 141 ms |
| `wait` for the typed text | 18619 ms | 942 ms |
| `launch com.apple.Preferences` on the SE right after its boot | 164400 ms | 901 ms |
| `launch dev.agemu.testapp` after the profile loop | 140034 ms | 641 ms |
| `profile iphone-se-3` / `ipad-mini` / `iphone-17-pro` | 44707 / 69718 / 50470 ms | 8780 / 8815 / 7830 ms |

What was agemu's part of the old tails (from the daemon log of that run and the measurements
above): every cold boot waited 11-43 s of "no frontmost application" before restarting the
bridge; tree reads made ~11 requests per element and each request could wait 5 s, with no
overall deadline (`wait --timeout-ms 5000` took 18.6 s); `ui` after reset spawned `launchctl`
to name the app; reset spawned `launchctl` for SpringBoard's pid and polled accessibility with
3 requests per poll; two simulators stayed booted on a swapping Mac. The 140-164 s launches
were CoreSimulator's own `launchApplicationWithID` call on a just-booted device next to
another booted simulator at load ~400; agemu now caps a launch at 60 s with an error, and
keeps one simulator booted while memory is short.

### A real app, against agent-device and Argent

**The flow.** 23 steps through a React Native app (an Expo development build loading its bundle
from Metro, staging backend, a test account whose one-time code is fixed):
- launch the app;
- sign in with e-mail and code;
- open the Discover tab, search, open a result and go back;
- open two more tabs and a list entry;
- scroll to the sign-out button and sign out.

Nine steps only observe: five UI-tree reads and four screenshots. Every acting step ends when
the element of the screen it leads to is there, so all tools do the same work.

**The tools.** All three drive the same `agemu-iphone-17-pro` simulator (iOS 26.5):
- **agent-device 0.21.31**, one CLI call per action, wait and observation;
- **Argent 0.32.0**, through its CLI (`argent run <tool>`), one call per action, wait and
  observation. Its taps take normalized coordinates, so each tap first reads `describe` to find
  the element. Text goes in with `fill`, waits use `await-ui-element`, and the scroll is
  `gesture-swipe` with `momentum: false`;
- **agemu**, one call per action, wait and observation (39 calls);
- **agemu compound**, with each action waiting for its effect in the same call (`--wait-*`; 26
  calls).

**How it was run.**
- 5 rounds; each round runs the four variants one after another, with 20 s pauses because the
  backend allows 3 codes a minute.
- Before each run, an untimed reset makes sure the app is signed out.
- A failed step is retried once after 3 s, as an agent following agent-device's hint would; the
  failed attempt's time counts.
- Apple M2 Pro. Other work ran on the host (load average 5.9-19.3). No recording during the
  timed runs.

| | agemu compound | agemu | Argent | agent-device |
|---|---|---|---|---|
| runs ok | 5/5 | 5/5 (1 retried step) | 5/5 | 5/5 |
| whole flow (median) | **29.7 s** | 32.6 s | 38.8 s | 65.5 s |
| min-max | 26.2-35.4 s | 28.7-60.2 s | 34.1-41.2 s | 60.0-69.2 s |
| after launch (launch step excluded) | **15.8 s** | 16.9 s | 21.1 s | 47.6 s |
| tool calls per run | **26** | 39 | 51 | 36 |
| agemu compound is faster by (whole / after launch) | - | 1.10× / 1.07× | **1.31× / 1.34×** | **2.21× / 3.01×** |

An earlier series of the same kind, at load 2.6-8.5 and one commit before the final code,
gave 28.2 s (compound), 27.9 s (agemu), 32.2 s (Argent) and 59.1 s (agent-device): compound
was 1.14× faster than Argent and 2.10× faster than agent-device (1.27× and 3.11× after launch).

**Time per step.** Medians; the observation rows are the tools' own cost.

| step | agemu compound | agemu | Argent | agent-device |
|---|---|---|---|---|
| launch app (dev client, Metro) -> login screen | 13946 ms | 13967 ms | 15075 ms | 16594 ms |
| observe: UI tree (login) | 24 ms | 29 ms | 188 ms | 324 ms |
| enter e-mail | 960 ms | 975 ms | 790 ms | 2681 ms |
| tap 'send code' -> code screen | 728 ms | 777 ms | 1208 ms | 2959 ms |
| enter code -> home | 2122 ms | 2218 ms | 2861 ms | 6960 ms |
| observe: UI tree (home) | 76 ms | 72 ms | 280 ms | 350 ms |
| observe: screenshot (home) | 22 ms | 21 ms | 212 ms | 700 ms |
| tab Discover -> search field | 1940 ms | 2280 ms | 2180 ms | 3800 ms |
| observe: UI tree (discover) | 122 ms | 136 ms | 513 ms | 455 ms |
| observe: screenshot (discover) | 18 ms | 25 ms | 161 ms | 510 ms |
| search -> results | 2169 ms | 1944 ms | 1996 ms | 5148 ms |
| open company -> profile | 1260 ms | 1449 ms | 2081 ms | 3790 ms |
| observe: UI tree (company) | 116 ms | 82 ms | 280 ms | 342 ms |
| observe: screenshot (company) | 26 ms | 16 ms | 159 ms | 579 ms |
| back -> discover | 650 ms | 840 ms | 853 ms | 2100 ms |
| tab Cases -> scopes | 954 ms | 938 ms | 1298 ms | 3181 ms |
| History -> past booking | 583 ms | 682 ms | 659 ms | 1693 ms |
| observe: UI tree (history) | 51 ms | 37 ms | 204 ms | 304 ms |
| tab Profile -> profile | 613 ms | 588 ms | 1144 ms | 2198 ms |
| scroll -> sign-out button | 399 ms | 388 ms | 662 ms | 1778 ms |
| tap sign-out -> confirm dialog | 948 ms | 1020 ms | 654 ms | 2317 ms |
| confirm -> login screen | 1202 ms | 848 ms | 1539 ms | 2534 ms |
| observe: screenshot (login) | 19 ms | 17 ms | 164 ms | 484 ms |

**What the numbers show.**
- **The launch is the app's.** 14-17 s is the development client loading its JavaScript bundle,
  for every tool.
- **The rest is mostly the app's.** After launch, agemu's acting steps are bounded by the app's
  transitions: a tap returns when the first changed frame shows, and the wait ends on the first
  UI-tree read that has the next screen. Argent is closest. It is faster on two steps:
  - entering the e-mail (`fill` 790 ms vs a verified paste, 960 ms);
  - opening the sign-out dialog. Argent's scroll has no momentum. agemu's swipe flings, and
    its selector tap waits for the list's bounce to end before it reads where the button is.
- **Fewer calls.** The compound variant saves 13 of 39 calls, and some wall time. Every call is
  one more turn for an agent, so its model time is multiplied by the call count. Not measured:
  at 2 s of model time per call the flow would take about 82 s (compound), 111 s (agemu), 141 s
  (Argent) and 138 s (agent-device).

**Before and after this round of work.** agemu from `main` (48246de) and from this work (one
commit before the final code), plain variant, interleaved: two series, 9 runs each, load
average 3.5-11.3.

| | main | this work |
|---|---|---|
| whole flow (median) | 31.6 s (28.3-37.6) | **29.5 s** (26.5-59.5) |
| after launch | 18.9 s | **15.2 s** |
| runs with a retried step | 0 | 1 |
| enter code -> home | 4708 ms | **1904 ms** |
| tab Profile -> profile | 999 ms | **554 ms** |
| History -> past booking | 752 ms | **529 ms** |
| enter e-mail | 1081 ms | **922 ms** |

The code step shows the main change. Before a tap or a paste, `main` waited for a screen with
no change at all, which a blinking cursor never gives. That cost the 1 s timeout on the tap and
again on the paste. Now only big changes count (see "How each operation works").

**Taps that did nothing.** In 52 runs of this work's builds, 5 had one tap that the app did not
act on. Each came right after a new view appeared:
- the Cases tab after going back;
- the History toggle right after the Cases tab opened;
- the sign-out confirmation.

Each time the retry worked, and the 30 s wait before it counts in the times above. For
comparison:
- `main`: none in 9 runs;
- Argent: 2 in 23 (the sign-out dialog);
- agent-device: none in 20. It is about twice as slow, so the screen has settled before each of
  its taps.

The cause is not known. Repeating those steps alone missed none: 25 tab taps after going back,
and 8 History taps sent 0-1.6 s after the tab opened.

**What did not help.**
- **Speeding up animations** with `UIAnimationDragCoefficient` (Simulator.app's slow-animation
  switch, set lower): no effect on iOS 26.5.
- **Waiting only for the tapped element's region to go still.** UIKit ignores touches for the
  whole length of a screen transition, also where the target region is already still. One step
  missed its tap and took 34 s with the retry, so taps wait for the whole screen again.
- **A swipe that holds still before lifting**, to end without momentum. It could not be shown to
  stop the app's lists, so it was not kept.

## Caveats

- **Taps by selector settle first.** Before it resolves a selector, agemu waits until the
  screen has had no big change for 48 ms (at most 1.5 s), so a button that is still scrolling
  into place is tapped where it comes to rest. A tap with a selector that matches nothing still
  fails at once; add `--wait-*` to the previous action, or `wait`, when the element comes later.
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
- **HID keyboard.** `type --keys` (and `type` of a single character) uses a US hardware
  keyboard layout, and iOS's hardware-keyboard auto-capitalization (on by default) applies, so
  `type --keys hello` into an empty field with sentence capitalization can produce "Hello",
  exactly as on a device with a keyboard. `type` of longer text pastes it. The software keyboard
  can stay on screen while agemu types (seen on iOS 26.5 with both paths), covering the lower
  part of a phone screen.
- **Disk.** Every simulator that has booted once costs 1-2 GB: on its first boot iOS stages
  about 1 GB of Siri, text-to-speech and linguistic assets into
  `data/private/var/MobileAsset` (`mobileassetd`; `simctl boot --disabledJob` does not stop it).
  `xcrun simctl erase <udid>` (on a shut-down device) returns it to ~17 MB.
- **First boot** of a new or erased simulator migrates data and took 1.5-3 minutes on this
  (heavily loaded) host; agemu waits up to 10 minutes. Later boots take seconds.
- **Signed helper.** `make ios-helper` replaces `agemu-ios-helper` instead of overwriting it in
  place: macOS kills (SIGKILL) a signed binary whose file was rewritten in place.
- **Launch time is the app's.** `agemu launch` makes the same CoreSimulator call as
  `simctl launch` (`-[SimDevice launchApplicationWithID:options:error:]`). Measured on the same
  device (8 launches each, Settings terminated before each): daemon round trip 224 ms, of which
  the server side 224 ms; agemu CLI 238 ms; `simctl launch` 253 ms. Besides the CoreSimulator
  call agemu does one 0.1-0.2 ms device lookup, so the CLI adds only its process start. The
  call is bounded by FrontBoard starting the process. What an app does after launch (Settings
  takes ~2 s to show its list on this host) is the app's own time.
- **iPad: one app per screen.** On iPad the bridge's frontmost application is often the dock's
  view service; agemu reads the app that owns the screen centre instead (see "iPad: the app
  that owns the screen"). With two apps on screen (Split View, Slide Over) `ui` shows the one
  at the centre, or the first of four more points that hits an app covering 40% of the screen;
  that was not tested.
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

The helper is found next to the `agemud` daemon binary, or at `$AGEMU_IOS_HELPER`. Its source is
`ios-helper/` (`swift build -c release`).

## Credits

The private-API usage (Indigo message layout, framebuffer callbacks, the AXPTranslator token
delegate) follows the public reverse-engineering in
[facebook/idb](https://github.com/facebook/idb)'s FBSimulatorControl (MIT license, Copyright (c)
Meta Platforms, Inc. and affiliates). agemu's code is its own; where a byte layout comes from
idb's `Indigo.h` / `SimulatorIndigoHID.swift`, the source says so.
