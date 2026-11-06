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
| tap / swipe | no simctl command; Maestro starts a JVM and an XCUITest runner | Indigo HID touch messages sent through SimulatorKit's `SimDeviceLegacyHIDClient`, the same channel Simulator.app uses for mouse clicks. Pixels are converted to screen ratios. A swipe is a touch-down, one move per ~16 ms, and a touch-up. **A tap first waits until the screen has been still for 48 ms** (at most 1 s): UIKit ignores a tap that lands during a transition (an e2e run lost one ~0.3 s after a navigation pop). A background thread samples the framebuffer surface's seed every 8 ms, so on a still screen the wait is 0 ms. `AGEMU_IOS_TAP_SETTLE=0` turns it off. |
| type | Maestro `inputText` | HID keyboard events (US layout, shift handled). Text the US keyboard cannot produce (accents, emoji, other scripts) is put on the simulator pasteboard (`simctl pbcopy`) and pasted with Cmd-V. |
| key | — | `home` and `lock` are the hardware buttons; `enter`, `delete`, `tab`, `escape`, arrows are keyboard keys; `volumeup/volumedown` are consumer-page HID usages. iOS has no back key: `back` taps the navigation bar's back button (UIKit's `BackButton` identifier) when the screen has one, and otherwise does a **left-edge swipe**, flagged as an edge gesture, which triggers the navigation controller's interactive pop like a thumb does. On the loaded test host one edge swipe was not recognized, hence the tap first. |
| UI tree | `maestro hierarchy` (JVM + XCUITest snapshot) | `AXPTranslator` from AccessibilityPlatformTranslation (the framework Simulator.app's accessibility support uses), with agemu as its token delegate: every translator request is forwarded to the device with `-[SimDevice sendAccessibilityRequestAsync:...]`. **All attributes of an element come in one request** (`accessibilityMultipleAttributes:`, translator request type 5, which runs `AXUIElementCopyMultipleAttributeValues` inside the simulator) instead of one request per attribute: 2 requests per element (the batch plus the translator's own remote-element check) instead of 11, with byte-identical output. The app's bundle id is one more request (AXP attribute 6), so the tree names the frontmost app without `launchctl`. On iPad the frontmost app the bridge reports is checked with a hit test on the screen (see "iPad: the app that owns the screen"). Every read has a deadline (20 s, or the caller's; 5 s per request) and fails with an error rather than return a partial tree. Off-screen elements are still read (and dropped in Go): pruning them by frame could drop an on-screen child of an off-screen container. |
| launch / terminate / install | `simctl launch` etc. | The same CoreSimulator calls, made in-process by the helper. |
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
