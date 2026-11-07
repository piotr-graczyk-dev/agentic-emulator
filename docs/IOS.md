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
| type | Maestro `inputText` | Inserts the text at the cursor of the focused field by **pasting** it (the helper's pasteboard connection, then Cmd-V; see "Test capabilities"): the app gets the whole text in one change, as with Android's set-text path. The pasteboard's previous content (every item and type) is saved before and put back after, so typing does not change what the app or `agemu clipboard` sees. A single character is one key event. `type --keys` sends one HID key event per character (US layout, paced: each acknowledged, then 10 ms). Both wait for a still screen first (no big change; at most 600 ms) and return once the app has drawn the change (at most 500 ms). A paste is checked against the focused text field's text and repeated once if it did not arrive. Key events were not reliable in React Native controlled inputs (Expo development build of a real app): characters were lost or doubled at every pace tried, and a lost character in an e-mail field signed in to a different account; the paste always arrived whole. |
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
2. uninstalls every user-installed app, which deletes its data and app-group containers and
   its privacy grants (a reinstalled app reads "not determined" again),
3. resets the keychain (`simctl keychain reset`, in parallel with 1-2), and puts back what the
   test capabilities change, in parallel: light appearance, no simulated location, an empty
   pasteboard, and every remaining privacy grant of a service agemu's permission names cover
   (found in the device's `TCC.db` and locationd's `clients.plist`, reset one by one with
   CoreSimulator's `resetPrivacyAccessForService`; rows whose client is an executable path, not
   a bundle, are skipped). `simctl privacy reset all` without a bundle id, which reset used
   before, changed nothing on iOS 26.5 (the experimenter checked it with the probe app and in
   `TCC.db`; no log of that check was kept); resetting every service of every app would also
   drop the baseline's own system grants (33 rows: CloudKit and iCloud Drive access of system
   services). Reset does not tell a baseline row from a later one in a covered service: a new
   iPhone 17 Pro (iOS 26.5) had none, so nothing of the baseline is lost there; other device
   types and runtimes were not checked. Checked on that new device: grants to built-in apps
   (contacts for Maps, calendar and microphone for Health, a camera revoke for Health: 4 rows)
   were reset and the baseline's `kTCCServiceLiverpool` rows kept; Maps' location authorization
   was reset too,
4. presses home, waits until accessibility reports SpringBoard (`com.apple.springboard`) as the
   frontmost app (on iPad too, found by the hit test above), then until the screen has been
   still for 300 ms. The screen moves for ~1.3 s
   after the press; an app launched sooner (the old fixed 300 ms) sometimes started in the
   background, or failed to launch while a terminated process was still exiting. `launch` also
   retries a "failed to launch" twice (after 0.3 s and 0.7 s) when the app is installed.
   10 of 10 reset-then-launch rounds brought Settings to the front.

The daemon log has each phase, e.g. `ios reset <udid>: 1736 ms (app lists 1, terminate 25 apps
31, uninstall [] 0, appearance/location/pasteboard/privacy 40, simctl keychain done at 158, home
1578)`.

It does **not** reset what `simctl erase` would: data stored by built-in apps (Settings search
history, Safari, Contacts, Photos...), other system settings an agent changed (language...),
notification permissions given to built-in apps in their system alert, and media added with
`simctl addmedia`. Use `agemu shutdown` + `xcrun simctl erase <udid>` for a full wipe.

### Bounded calls

No agent-facing call waits silently. Every helper command has a timeout (launch 60 s, install
3 min, terminate 20 s, UI read 20 s, screenshot 15 s, tap 15 s, ...) unless the caller set an
earlier deadline, and fails with an error that names what it waited for, e.g. `launch <udid>:
gave up after 60.0 s waiting for CoreSimulator to launch com.apple.Preferences; the simulator is
overloaded or stuck (the request may still complete in the background)`. `wait` bounds each UI
read by its own deadline (at least 1 s) and reports the last read error on timeout. Only a cold
boot may take up to 10 minutes (a first boot migrates data).

## Test capabilities

The capabilities of [CAPABILITIES.md](CAPABILITIES.md) (`open`, `permission`, `location`,
`clipboard`, `appearance`, `apps`, `uninstall`, `clear-data`, `push`) are CoreSimulator's own
calls, the ones `simctl` makes, issued by the long-lived helper (`ios-helper/Sources/AgemuSim/
AGCapabilities.m`, `Capabilities.swift`; Go side `internal/ios/capabilities.go`). simctl is a new
process that loads CoreSimulator and finds the device for every call (~105-175 ms here); the
helper has both loaded.

| Capability | CoreSimulator call | simctl equivalent |
|---|---|---|
| open | `-[SimDevice openURL:error:]` | `simctl openurl` |
| permission | `setPrivacyAccessForService:bundleID:granted:error:`, `resetPrivacyAccessForService:bundleID:error:` with the service names simctl maps to (`kTCCServiceCamera`, `kTCCServiceAddressBook`, ..., `__CoreLocationWhenInUse`, `__CoreLocationAlways`, `kTCCServiceAll`) | `simctl privacy` |
| location | `setLocationWithLatitude:andLongitude:error:`, `clearSimulatedLocationWithError:` | `simctl location set/clear` |
| clipboard | SimPasteboardPlus: a `SimPasteboardInterface` connected to the device's `com.apple.CoreSimulator.SimPasteboardInterface` service mirrors the device pasteboard into a private `NSPasteboard` of the helper (`pull` / `push`); kept per device and boot. `-[SimDevice pasteboard]` is nil on Xcode 26. | `simctl pbcopy/pbpaste` (the same interface) |
| appearance | `setUIInterfaceStyle:` (1 light, 2 dark), `currentUIInterfaceStyle` | `simctl ui appearance` |
| apps | Info.plists of `Containers/Bundle/Application/*/*.app` in the data directory | `simctl listapps` |
| uninstall, app checks | `uninstallApplication:`, `propertiesOfApplication:` (path, containers, type) | `simctl uninstall`, `simctl appinfo` |
| clear-data | clone the installed bundle (APFS `clonefile`), uninstall, install the clone | `simctl uninstall` + `simctl install` |
| push | `sendPushNotificationForBundleID:jsonPayload:` (an `NSDictionary`) | `simctl push` |

**Latency** (iPhone 17 Pro simulator, iOS 26.5, lean; medians of 12 calls per arm, interleaved
ABC/CBA per round; load average 14-27, and 20-101 during clear-data; the raw runs are kept with
the experiment, outside the repository):

| Operation | simctl | agemu CLI | daemon (MCP) |
|---|---|---|---|
| open a URL | 120 ms | 11.9 ms (10x) | 6.7 ms (18x) |
| grant/revoke camera | 130 ms | 10.0 ms (13x) | 4.7 ms (28x) |
| grant/revoke location | 116 ms | 6.1 ms (19x) | 1.5 ms (78x) |
| set location | 109 ms | 5.3 ms (21x) | 0.8 ms (142x) |
| clear location | 107 ms | 5.1 ms (21x) | 0.5 ms (218x) |
| set clipboard | 133 ms | 5.8 ms (23x) | 0.8 ms (159x) |
| read clipboard | 161 ms | 13.8 ms (12x) | 11.7 ms (14x) |
| read appearance | 175 ms | 10.8 ms (16x) | 1.0 ms (174x) |
| list user apps (agemu reads user apps' Info.plists on the host; simctl lists every app through CoreSimulator) | 173 ms | 6.5 ms (27x) | 0.7 ms (248x) |
| push | 133 ms | 21.4 ms (6x) | 15.8 ms (8x) |
| uninstall | 194 ms | 80 ms (2.4x) | 83 ms (2.3x) |
| clear-data (vs uninstall + install) | 538 ms | 269 ms (2.0x) | 258 ms (2.1x) |
| set appearance, until a screenshot shows it | 670 ms | 699 ms (1.0x) | 694 ms (1.0x) |
| set appearance, the call itself | 130 ms | - | 684 ms (5.3x slower) |
| set appearance, until the app's trait shows in the UI tree | 257 ms | - | 736 ms (2.9x slower) |

Uninstall and clear-data are bounded by installd (70-90 ms to uninstall, 150 ms to install the
probe app), so agemu saves only simctl's process starts. A 143 MB bundle was measured only with
an earlier in-place wipe (287 ms) and a plain uninstall + install (288 ms), not with the final
clone and reinstall. Setting the appearance waits for iOS's own crossfade (next list): a
screenshot right after the call shows the new appearance, but an agent that reads `ui` instead
gets it about 3x later than after `simctl ui appearance`, whose screenshot would still show the
old one.

**Checked as an app sees it**, with the probe app (`testapps/ios`, its Capabilities screen),
and what differs from simctl or a device. The final run passed 110 of 110 checks; two checks
that failed in earlier runs (content-available delivery, and a running app losing its fix after
`location clear`) had been turned into notes, and Siri was never read back:

- **open**: the app's `onOpenURL` gets the URL, with the app in front and from a cold start;
  `https://` opens Safari; an unhandled scheme is an error (`LSApplicationWorkspaceErrorDomain
  115`). iOS can ask "Open in <app>?" first, as for a link in Safari: no prompt in the 36
  benchmark opens or the first verification runs, one when a probe tool issued two opens of the
  same URL 140 ms apart, and one for a single open of a just-installed app in front on a new
  simulator (the r3-merge smoke; it was tapped). agemu does not answer it, so follow `open`
  with a `wait` or tap Open. The handling app is not reported.
- **permission**: grant, revoke and reset of camera, microphone, photos-add, location,
  location-always, contacts, calendar, reminders, motion and media-library read back as
  authorized / denied / not determined in the app. Revoking either location name denies
  location; granting `location` after `location-always` steps down to while-in-use; a reset
  without names (`kTCCServiceAll` for the bundle) also resets location. Not implemented, with
  the reason in the error:
  - `photos` grant: the Simulator writes a TCC record (`auth_version` 1) that Photos ignores on
    iOS 26.5: the app still reads not determined (`simctl privacy grant photos` too). The
    system dialog writes version 2. Revoke and reset work.
  - `contacts-limited`: the Simulator's service of that name is basic contact info; the app
    reads full authorization, not iOS 18's limited access.
  - `notifications`: no privacy service. The system alert shows in `agemu ui` (SpringBoard)
    and is answered with a tap.
  - raw Android names.
  - `siri` is mapped (`kTCCServiceSiri`) but could not be read back: `INPreferences` aborts in
    an app without the Siri entitlement.
  A grant is refused when the app's Info.plist lacks the usage description
  (`location-always` needs both location keys); the probe has no `NSSiriUsageDescription`, and
  `permission grant ... siri` is refused. The app may be terminated, as by simctl.
- **location**: an app updating its location gets each set within the wait (two points, 2 of
  2). After `clear`, an app that starts updating gets `kCLErrorLocationUnknown`; an app that was
  already updating usually keeps its last fix (5 of 6 clears, the same with `simctl location
  clear`).
- **clipboard**: the app's `UIPasteboard` reads what `clipboard set` wrote, after iOS's own
  "Allow Paste" prompt, as on a device; `clipboard` reads what the app copied. `type` pastes
  through the pasteboard and no longer starts simctl: a whole `agemu type <word>` took 118 ms
  (CLI median, n=12), against 111 ms for the old path's `simctl pbcopy` step alone,
  interleaved. It saves the pasteboard (every item and type) first and puts it back once the
  focused field shows the paste. When that cannot be confirmed (a secure field, a web view or
  custom input that does not read back as text, or a paste that had to be sent twice), the typed
  text stays on the pasteboard, as before: the app may not have read it yet, and a restore that
  won that race would make it paste the old content. The restore was checked with one plain-text
  string, read back by agemu. The restored items come from the host, so an app that copied text
  itself may get iOS's "Allow Paste" prompt when it reads it back programmatically, and the
  pasteboard's `changeCount` moves; neither was checked. The helper's pasteboard uses a private
  CoreSimulator class (`SimPasteboardInterface`); if it is missing, `type` falls back to
  `simctl pbcopy` and leaves the text on the pasteboard. The helper's pasteboard is private, so
  the Mac clipboard never reaches the device; Simulator.app, if someone opens it, syncs the Mac
  pasteboard to the devices it shows.
- **appearance**: the app's trait collection changes at once. iOS then crossfades the whole
  screen for ~500 ms, starting 50-100 ms after the call (screenshots every 25 ms; once about
  650 ms), so a screenshot right after `simctl ui appearance` shows the old appearance (6 of 6).
  `agemu appearance` returns once a big screen change has begun (it waits up to 1 s for it) and
  ended (120 ms still, at most 1.5 s in all): a screenshot right after showed the new appearance
  8 of 8 times. When no change shows within the 1 s (an app in front that keeps its own
  appearance), the call returns then and the daemon logs it.
- **push**: with the app in front, its notification delegate (`willPresent`) gets every push
  (36 of 36 in the benchmark), with or without notification permission. With the app in the
  background, the banner appears ~10.5 s later on this lean simulator: SpringBoard waits for a
  notification-intelligence inference that times out (log: `Waiting for inference`, then a
  failure after 10.5 s), the same with `simctl push`. Lean mode turns off
  `intelligenceplatformd` and other Apple Intelligence jobs, which may be why; this was not
  checked on a full simulator. The banner
  is not in agemu's UI tree (SpringBoard's banner window): tap it by position; the app's
  `didReceive` then gets it. `content-available` pushes were not delivered (simctl the same;
  the probe has no `aps-environment` entitlement).
- **clear-data**: the probe's defaults, Documents and Caches files, app-group file and app-group
  defaults are gone; its keychain item stays; camera and notification permissions read not
  determined. Wiping the containers in place (with `defaults delete` so cfprefsd drops its
  cache, and a privacy reset) took as long (~290 ms) but kept the notification permission, so
  clear-data reinstalls. Deleting the files alone left app-group defaults readable.
- **apps / uninstall**: name and version come from the Info.plist; built-in apps are refused
  (`... is a built-in (system) app`), as is an app that is not installed.

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

1. **49 launchd jobs are off** (`internal/ios/lean.go`, grouped by feature). agemu runs
   `launchctl disable` inside the device, so the override persists and later boots start without
   the jobs, and `launchctl bootout` for the ones running at that moment. It runs in the background
   after boot. The first time on a device it takes about 10-20 s, 36 s on a host at load 90 (the
   helper runs launchctl in the device, 8 at a time), and agemu reads the overrides back before it
   counts the device as lean.
   Later boots check it with one call. `simctl boot --disabledJob` would be the documented way, but on
   CoreSimulator 1051 it had no effect: 25 of the first 32 jobs still ran. Six Apple Watch sync
   jobs (`NPKCompanionAgent`, `bulletindistributord`, `addressbooksyncd`, `eventkitsyncd`,
   `appconduitd`, `companionappd`) are not in the list: `nanoregistrylaunchd` turned them back on
   and started them at every boot (76 MB together), also seconds after agemu had turned them off.
2. **The wallpaper gallery is stopped after a cold boot.** PosterBoard starts one extension per
   wallpaper provider in its gallery (9-15 processes of 35-75 MB) within seconds of boot and keeps
   them. They live in several places in the runtime (`/System/Library/ExtensionKit/Extensions`,
   poster apps such as `/Applications/EmojiPoster.app/Extensions` and
   `/System/Library/CoreServices/AegirProxyApp.app/Extensions`, `AmbientUI.framework/PlugIns`).
   agemu stops them 3, 8, 20 and 45 s after the device is ready, and then every 15 s (up to 5 min)
   while a pass still found one, except the providers of the posters in use, which it reads
   from PosterBoard's data store in the device: the lock screen's (`com.apple.MercuryPoster` on
   the iOS 26.5 iPhone 17 Pro, iPhone SE and iPad mini simulators checked) and those of contact
   posters (the incoming-call role; the stores checked had none). agemu reads the store with the
   host's `/usr/bin/sqlite3` in read-only mode; if that fails, or the store has no lock screen
   poster yet, nothing is stopped. It stops only extensions in those places: not an app's own
   extensions, not Messages' backgrounds, and not the contact poster providers.
   `MonogramPosterExtension` was seen running (the contact poster editor stayed up through the
   passes); `AvatarPosterExtension` and `PhotosPosterProvider` are kept by design, the latter
   although it is also a gallery wallpaper, because it renders photo contact posters too. The
   daemon log names the providers it stopped. In the first 45 s PosterBoard starts some stopped
   providers again, so a boot takes 13-27 stops;
   after that they stayed stopped, also through two lock and unlock cycles in 8 minutes, and the
   lock screen drew its wallpaper, clock and the app's notification. On the first boot of a new
   iPhone 17 Pro PosterBoard wrote the lock screen's poster to its store 26 s after the device was
   ready, while the gallery was starting (up to 21 extensions); on new iPhone SE and iPad mini
   devices (67-98 s to agent-ready) it was there before. Until the store has it nothing is
   stopped, and the passes go on until it does. Stopping the lock screen's own provider (what
   agemu did before, when it matched only `ExtensionKit/Extensions`) made iOS relaunch it and
   PosterBoard start part of the gallery again. PosterBoard itself stays
   on: with PosterBoard disabled, the lock screen was not drawn (the UI tree reported the lock
   screen while the screenshot showed the home screen). `pluginkit -e ignore` on a gallery provider
   did not keep PosterBoard from starting it.
3. **Background work started after boot is stopped once it is idle** (`internal/ios/background.go`).
   In the first minutes after a cold boot iOS starts some apps, extensions and services for one-off
   work and then keeps them idle; a phone's jetsam reclaims such processes when it needs memory,
   the simulator only when the Mac runs short of memory. agemu stops these (sizes and times
   measured on the iPhone 17 Pro, in 12 boots, times after agent-ready):
   - the Calendar app, 83 MB: `calaccessd` opens it in the background after changes to the
     calendar database, which it makes after a boot (possibly rebuilding the Birthdays calendar;
     that cause is not verified), at +19..71 s, and it stayed. It did so in all 12 boots of the measurement below (in an earlier
     set, 2 of 4 boots had not by 3 minutes); in 2 later boots of the same device, after the
     checks below had used Calendar, and on a new iPad mini, it exited again 4-6 s after it was
     opened;
   - the App Intents extensions of Calendar, Safari and Messages, 47 MB: `linkd` starts them at
     +17..67 s;
   - device management (MDM and declarative management): a one-off on-reboot task of the
     `remotemanagementd` launchd job starts the daemon (7 MB, which stays: it did not exit on
     SIGTERM) and 10 subscriber XPC services (106 MB), which launchd starts on demand, at
     +131..197 s. Only these 10 are matched by name; the framework's other 3 subscribers
     (`MDMAppSubscriber`, `PowerSubscriber`, `SoftwareUpdateSubscriber`) were never seen and are
     left alone.

   agemu looks every 30 s from 1 to 5 minutes after the device is ready and stops each of these at
   the pass after the one that first found it, so it has run for at least 30 s: SIGTERM, and for
   Calendar a CoreSimulator terminate. A Calendar that a pass found in front (opened by the user or
   a test), or that was running at a pass where the frontmost app could not be told, is never
   stopped, also after it goes to the background; without that rule, a Calendar opened at +20 s
   and sent home at +62..64 s was terminated at the +90 s pass and lost an unsaved new event on
   return (3 of 3 boots). A Calendar session that is not in front at any pass is still stopped:
   one opened and left before the first pass (up to 60 s after agent-ready, when agents start
   working), or between two passes (under 30 s), including a Calendar that `calaccessd` had
   started and the user then brought to the front (same process). The frontmost app is read up
   to about 3 s before the terminate, and on iPad a Calendar in Split View that is not reported
   as frontmost is not protected. The daemon log lists what each pass stopped. Calendar, the
   extensions and the subscribers were not seen to start again up to +300 s, when the watcher
   stopped (it can miss a start; the footprint at +300 s showed only `remotemanagementd` left in
   every boot where it was taken). Others were tried and are not
   stopped because iOS starts them again: `WidgetRenderer_Default` (SpringBoard relaunched it at
   once), the Spotlight UI (67 MB; SpringBoard starts it again whenever the home screen shows, for
   example after unlock) and the speech synthesis extensions (38 MB; `axassetsd` retries its voice
   database load, which fails on a lean simulator, and started one of them again 2 minutes after it
   was stopped).

   Checked inside the 5-minute window, in 7 boots (2 without the stop, 3 with the version that
   had no rule for a Calendar in front, 2 with the final one): a configuration profile with a root
   certificate (as a proxy's CA profile has), downloaded in Safari and installed in Settings at
   +65..83 s, installed in all 7. A second one was installed later in 6 of them (in the 7th it was
   not downloaded: Safari, launched right after `agemu key home`, was sent to the background about
   1 s later by SpringBoard's emulated home press, a known race of the home key). In 2 boots its
   final Install was tapped 0.9 s before the pass that stopped the subscribers, and the install
   finished. All 13 of these installs were in the device's profile store afterwards. No
   subscriber was running during the early installs (they start at +131..202 s), and in the
   final version's boots none started during the late install: installing a profile through
   Settings does not use the stopped subscribers. So this shows that profile installs do not
   need them, not that stopping them in the middle of their own work is harmless. A Calendar
   opened at +20 s with an unsaved new event, sent home at +62..64 s and opened
   again at +125 s still had the event without the stop (2 of 2) and with the final version
   (2 of 2). An App Intents action could not be run: on a lean simulator the Shortcuts app shows
   no actions at all (its daemon, `siriactionsd`, is one of the lean jobs), and the stop runs only
   on lean simulators. Checked after the window in an earlier round, with a binary that also
   stopped the 3 unseen subscribers and had no rule for a Calendar in front: the stopped processes
   stayed stopped through a 10-minute session with three lock and unlock cycles; Spotlight search
   found Calendar, tapping its icon opened the day view (a cold launch now), `AVSpeechSynthesizer`
   spoke, an app saved and read back an EventKit event (after which `calaccessd` opened Calendar in
   the background again), and `calshow:` opened Calendar on the event.

**What a lean simulator does not do.** These need `AGEMU_IOS_LEAN=0`; the jobs are back from the
next boot:
- home screen widgets (they show empty) and WidgetKit timelines;
- Siri, App Intents donations, text-to-speech voices and speech recognition;
- on-demand system assets (downloadable fonts, dictation and linguistic models). On a first boot
  `mobileassetd` would also stage about 1 GB into the device;
- News, HomeKit, Screen Time, significant locations and Apple Intelligence models;
- photo analysis (memories, people), iCloud Mail and the music library;
- settings sync to a paired Apple Watch, Continuity with other devices and CarPlay;
- Screen Time usage tracking, Ask to Buy, Shared with You, Memoji, Find My, Personal Voice,
  Image Playground and Genmoji;
- Maps turn-by-turn navigation, Safari password monitoring and iCloud bookmark sync;
- Shortcuts: the app opens but lists no actions (`siriactionsd` is off), so App Intents actions
  cannot be run from it.

HealthKit (`healthd`), CoreSpotlight indexing, the share sheet, Wallet and Apple Pay (`passd`),
notifications and push, Game Center, StoreKit, CloudKit, Watch pairing and WatchConnectivity
(`nanoregistryd`, `wcd`), MapKit (`geod`) and Sign in with Apple stay on: their jobs are not in
the list. Game Center, CloudKit, Sign in with Apple and the share sheet were not exercised on this
setting.

Checked with a probe app, on the lean device only (no check was repeated on the previous lean
setting or with all jobs, so a difference against them is not ruled out). The probe ran on an
iPhone 17 Pro with a binary that listed 55 jobs; the device turned 6 of them (the Watch sync jobs
above) back on at every boot, so the state matched the 49 jobs. With the gallery stop: a
local notification with permission granted shows as a banner in the foreground (through the app's
`willPresent`); in the background it is delivered (`getDeliveredNotifications`) and SpringBoard's
UI tree showed its banner in 1 of 2 tries; it is listed on the lock screen. Contacts and EventKit
(calendar and reminders) read back what the app wrote; `MKLocalSearch` and `MKDirections` return
results; `PKPaymentAuthorizationController` presents the Apple Pay sheet with the simulated card;
a StoreKit purchase opens the Apple Account sign-in and fails with "cancelled" when it is
cancelled (`Product.products` returns none for unknown identifiers); `WCSession` activates (not
paired); the software keyboard and the emoji keyboard type into a field; the contact poster editor
(Contacts, Contact Photo & Poster) draws. `PKAddPassesViewController` was not checked (it needs a
signed pass), nor the share sheet, a photo contact poster opened in the first minute after boot,
remote notifications or Live Activities.

**Measured with the previous lean setting** (32 jobs and the old gallery stop). The same
simulator, booted 3 times each way, interleaved, with the zuapp flow (see "A real app" below) run
right after boot and once more, then the app left open:

| | all jobs (`AGEMU_IOS_LEAN=0`) | previous lean (32 jobs) |
|---|---|---|
| memory, the app open | 4.82 GB | **3.90 GB** |
| processes | 219 | 169 |
| simulator CPU during the flow right after boot (45 s window) | 148 s | **101 s** |
| simulator CPU during the flow, settled | 34 s | 33 s |
| cold boot until agent-ready | 9.3 s | 8.4 s |
| the flow | 25.7 s | 25.9 s (the app's pace) |

Without an app, in single runs: 4.0 GB and 20% of a core idle with all jobs; 2.5 GB and 1.5%
with the previous lean setting. The first minute after such a boot took 26-116% of a core,
depending on how many wallpaper extensions that boot started.

All 11 runs of the zuapp flow on simulators with the previous lean setting passed; 1 of them
needed a retry (a tap with no effect, see "Taps that did nothing"). With all jobs, 9 of 10 passed
and 1 failed: right after boot the app showed an empty screen for 30 s.

**The current lean setting** (49 jobs and the lock-screen-aware gallery stop) against the
previous one (32 jobs and the old gallery stop), on one `iphone-17-pro`, 4 pairs of cold boots in
ABBA order (2026-11-06, 22:28-22:53; host load 19-96 at the start of a run; medians, range in
brackets; the phys_footprint summed over the device's processes by the experiments' meter
script, which is not in the repository, its CPU counting processes that exit inside the window). The new arm ran a binary that listed 55 jobs, in the equivalent state
described above; the final 49-job binary was measured only in single first boots:

| | previous lean: 32 jobs, old gallery stop (n=4) | lean: 49 jobs, gallery stop (n=4) |
|---|---|---|
| memory 2 min after agent-ready | 2.42 GB [2.36-2.48] | **1.97 GB** [1.88-1.99] (-19%) |
| memory, AgemuTestApp open (the launch added 1-2 processes) | 2.41 GB (n=1) | **2.00 GB** [1.94-2.05] (n=2) |
| memory, AgemuTestApp open (the launch added 5-13 processes) | 2.61 GB [2.50-2.67] (n=3) | **2.14 GB** [2.13-2.15] (n=2) |
| processes 2 min after agent-ready | 148 | 122 |
| simulator CPU in the first minute after agent-ready | 91 s [34-95] | 63 s [46-98] |
| cold boot until agent-ready | 13.0 s [9.8-74.3] | 11.6 s [8.8-14.4] |

Every run of the new setting was at least 360 MB below every run of the previous one. A
first-minute CPU saving was not shown: in 2 of the 4 pairs the new setting took more (98 s against
95 s; 57 s against 34 s, where the 34 s run is a boot that took 74 s at host load 50 and moved its
work into the second minute, 81% of a core there). Stopped gallery extensions count in it: the new
setting stops 13-19 per boot, and they ran for a while first (6-37 s of CPU of exited processes in
the minute, against 7-14 s). These runs also stopped `PhotosPosterProvider`, which is kept now
(one more extension of 35-75 MB may stay running).

**Stopping the background work** (item 3) against the same lean setting without it, on one new
`iphone-17-pro` simulator, 6 pairs of cold boots in ABBA order, each with a freshly started daemon
(2026-11-07, 02:25-03:29; host load 10-100 at the start of a boot, median 14-49 during it; 11.4-16.9
GB of swap in use; host memory pressure "warn" 82% of the time, never "critical"; medians, range
in brackets; the same meter):

| | without (n=6) | with the stop (n=6) |
|---|---|---|
| memory 2 min after agent-ready | 1.96 GB [1.96-1.98] | 1.83 GB [1.83-1.86] |
| memory 4 min after agent-ready | 2.09 GB [2.09-2.12] | **1.85 GB** [1.85-1.90] |
| memory 5 min after agent-ready | 2.09 GB [2.09-2.12] | **1.86 GB** [1.85-1.89] |
| processes 4 min after agent-ready | 135 | 121 |
| simulator CPU in the first minute after agent-ready | 60 s [47-68] | 58 s [44-69] |
| cold boot until agent-ready | 12.0 s [11.0-18.9] | 12.9 s [10.5-14.5] |
| boots in which `calaccessd` opened Calendar | 6 | 6 |
| boots with launchd_sim idle exits | 0 | 0 |

Every boot with the stop was at least 190 MB below every boot without it at 4 minutes; per pair
-237 MB [-255 to -195]. The processes it stops held 243 MB at 5 minutes in the boots without it
(Calendar 83, extensions 47, subscribers 106, `remotemanagementd` 7, which stays). Calendar was
opened and stayed open in all 12 boots, so the measured 0.24 GB applies when that happens. A boot
without it was not measured; from those sizes the stop saves about 150 MB when Calendar is not
open (an estimate). In 3 of 4 later boots (2 on the reused iPhone and the new iPad mini), iOS
opened Calendar and it exited by itself 4-6 s later, so on reused devices, as agemu is normally
used, the saving may be closer to the estimated 0.15 GB.

On a new iPhone SE and a new iPad mini (one cold boot each after their first boot, final version,
no comparison boot): the passes stopped the 3 extensions at +90 s and the 10 subscribers at +180 s,
and on the SE also Calendar at +90 s (on the iPad mini it had exited by itself); nothing came back,
there were no idle exits, and the device took 1.85 GB (SE) and 1.84 GB (iPad mini) 4 minutes after
agent-ready.

Idle exits. In an earlier round, 3 of 4 boots with an earlier version of the stop (it also
SIGTERMed `remotemanagementd` and matched all 13 subscribers) had launchd_sim exit 6-44 idle
daemons (`JETSAM_REASON_MEMORY_IDLE_EXIT`), against 0 of 8 boots without its device-management
part (6 of them with no stop at all). launchd_sim does that for services that allow it
(`EnablePressuredExit`) when it decides memory is short; the daemons start again on demand. This
time, 0 of 14 boots with the stop had idle exits (the table, the checks above and the final
version's own boots), and 1 of 8 boots without the background stop (but with the wallpaper
gallery stop) had them: 12 daemons at +72..99 s, about 4 s after Safari's WebKit processes
started for a profile download, while host memory pressure stayed at "warn" (sampled every
second). Changes of the host's pressure level between "normal" and "warn" (26 in the 12 boots of
the table) did not set one off. What does is not known. Cascades happen without the stop, and
there is no sign that the current stop causes them; 0 of 14 against 1 of 8 cannot show whether
it changes how often they happen, and the earlier variant (which also stopped
`remotemanagementd`) is not cleared. The idle-exit counts come from `log show`; the 2 s
process-count series shows no other drop of 5 or more processes within 30 s outside the stop
passes, which supports the zeros.

Turning off 18 more jobs (Game Center, weather, Mail, Exchange accounts, sleep
schedule, family, Managed Settings, Watch pairing, ...) brought it to 1.57 GB but broke the
device: SpringBoard, healthd and mobiletimerd spun at 4.7 cores, the boot took 2 minutes and
apps did not launch. FinanceKit's `financed` off made passd (Wallet) crash every 20 s.

## Fast animations (opt-in)

iOS has no user setting that turns animations off. UIKit, however, multiplies the duration of
its animations by `UIAnimationDragCoefficient()`: Simulator.app's Debug > Slow Animations makes
it return 10 (through the `com.apple.UIKit.SimulatorSlowMotionAnimationState` notification
state). Read from the iOS 26.5 UIKitCore disassembly, the function also returns UIKit's internal
preference of the same name when that is above 0. UIKit reads its internal preferences (from the
`com.apple.UIKit` defaults domain) only on internal builds or with the `UIKit/UIPreferences`
feature flag on, which is why setting the default alone did nothing.

`AGEMU_IOS_ANIMATIONS` takes the values of `AGEMU_ANDROID_ANIMATIONS`: unset, `1`, `on`, `true`
or `yes` is normal speed; `0` (also `0.0`), `off`, `false` or `no` (and `fast`) makes animations
as short as iOS allows, a coefficient of 0.05 (UIKit treats 0 as unset); a number between 0.05
and 1 is taken as the coefficient. Anything else (`nan`, `0.01`, `2`) is logged and leaves
animations at normal speed. After each boot agemu makes (once the device is ready, before `boot`
returns; `defaults` needs a booted device) and before the first launch this daemon makes on a
device, agemu (`internal/ios/animations.go`):
1. turns the feature flag on with an override file in the device's data directory
   (`Library/Preferences/FeatureFlags/Domain/UIKit.plist`, read by every simulator process when
   it starts);
2. writes the coefficient with `defaults write com.apple.UIKit UIAnimationDragCoefficient` in the
   device, and posts `com.apple.UIKit.InternalPreferences`, on which running UIKit processes read
   their internal preferences again.

Apps launched after that use the coefficient. SpringBoard has already started by then, so it
picks the setting up from the next boot. Without the variable, agemu deletes the coefficient and
removes the override (checked: no override file, no key, a push back at 1461 ms). The daemon logs
what it applied, or why it could not (for example a helper that does not report the data
directory).

Checked end to end on 2026-11-07 (`agemu-iphone-se-3`, lean): a fresh daemon with
`AGEMU_IOS_ANIMATIONS=0` booted the shut-down device and logged `UIKit animation coefficient 0.05
(apps launched from now on; SpringBoard from the next boot)`; `defaults read` in the device gave
0.05 and the override file was there. A daemon without the variable logged `back to normal
speed` at its first launch, and the key and the file were gone. Time from the `tap`/`key` call to
the last screen change, from agemu screenshots taken for 3.5 s after it, Settings, 10 rounds per
arm in blocks of 5 (fast, normal, fast, normal), load average 14-68, median (range): General push
1195 ms (1166-1811) against 2085 ms (2077-2590) at normal speed; back 199 ms (185-270) against
988 ms (964-1248).

**Measured** (Apple M2 Pro, `agemu-r2i4`, iOS 26.5, lean). Arms: normal, the feature flag
alone, and the flag with coefficients 0.2, 0.1 and 0.05, 5 rounds each with the order rotated
and reversed every round; load average 9-64 (other work on the host). For every step a private
helper taps the element's center (read from the UI tree) on a screen that has been still for
600 ms, and reads the time of the last big frame change after the touch from its frame monitor
(more than 0.4% of a 48x104 pixel grid between two frames; for the 1 s animations, which move a
small box, the last frame of any size). Each step was then checked in the UI tree. Median
[range], ms:

| step | normal | flag only | 0.2 | 0.1 | 0.05 |
|---|---|---|---|---|---|
| SwiftUI `NavigationStack` push | 1466 [1440-1575] | 1458 | 387 | 283 | 172 [158-199] |
| back | 542 [530-543] | 535 | 177 | 119 | 93 [83-94] |
| tab switch / back | 532 / 577 | 525 / 596 | 193 / 181 | 123 / 155 | 159 / 155 |
| `.alert` appears / OK | 543 / 409 | 545 / 412 | 220 / 147 | 190 / 99 | 177 / 87 |
| UIKit `pushViewController` / back | 1434 / 532 | 1415 / 521 | 311 / 153 | 191 / 105 | 132 / 88 |
| `UIAlertController` appears / button | 495 / 430 | 467 / 421 | 145 / 146 | 107 / 100 | 94 / 78 |
| Settings back to the root (2 panes) | 680 / 738 | 631 / 710 | 257 / 259 | 214 / 193 | 250 / 219 |
| Settings push General / Accessibility | 2462 / 3233 | 2296 / 2653 | 931 / 1370 | 648 / 1517 | 1190 / 1749 |
| sheet appears / is dismissed | 580 / 592 | 591 / 575 | 577 / 576 | 579 / 579 | 582 / 577 |
| 1 s `UIView.animate` / `withAnimation` | 1046 / 1074 | 1043 / 1068 | 244 / 289 | 145 / 189 | 101 / 140 |

Summed per group (normal -> 0.05): the 6 SwiftUI steps 4069 -> 843 ms (4.8x), the 4 UIKit
steps 2891 -> 392 ms (7.4x), the 2 Settings backs 1418 -> 469 ms (3.0x), the 2 Settings pushes
5695 -> 2939 ms (1.9x; 0.1: 2.6x). A Settings push is mostly Settings loading the pane; its
first row also moved after launch, so 1-4 of 5 General pushes per arm opened another pane and
were left out (normal speed included), so the General push has n=3-4 per arm. The flag alone
changed nothing (1.0x). 0.05 was not slower than normal speed in any step (sheets: equal); 0.01,
measured before, made Settings' back slower, so 0.05 is the smallest value accepted. 0.1 beat
0.05 on all 4 Settings steps; the 0.05 arm ran at the highest mean load (38-42 against 29-30 at
normal speed), so this may be load or a U-shape. The ratios depend on the 0.4% threshold: counting
the last frame change of any size, they are 3.6x (SwiftUI), 6.5x (UIKit) and 2.4x (Settings back).

- **Sheets keep their speed** at every coefficient. `SheetResponse` and `SheetDampingRatio`,
  internal preferences named in UIKitCore, changed nothing either (3 sheets each, 543-588 ms).
- **An agent flow** of 15 agemu calls (Settings push/back twice, then in the test app push,
  back, tab and back, alert and OK, both 1 s animations, focus a field, type, enter; each call
  acting and waiting for its result) took 13.26 s [10.96-20.85] normally, 12.27 s with the flag
  alone, and 6.87 s [6.23-7.25] at 0.05 (1.9x; medians of sums of call times, 5 runs each). One
  normal-speed run failed on a Settings mis-tap (a 10 s timeout); over the other 4 the median is
  12.51 s (1.8x). None of the five 0.05 runs lost the alert tap of the next point; at 0.1 and 0.2,
  where 2 of 5 runs each lost it, the medians were 12.86 and 7.90 s. These flows ran with the
  previous daemon on a device set up by hand, not through agemu's boot path.
- **A tap on a just-presented alert can be lost, at normal speed too.** This is a timing hazard
  that exists without the setting; fast mode changes when it happens, and it is not shown to be a
  breakage that only fast mode causes. In a loop of repeated alerts (agemu taps), the first tap
  on OK was lost in 7 of 12 trials at normal speed and 6 of 12 at 0.05. In the agent flow,
  `tap --text OK` after `tap --id show-alert --wait-text "Lab alert"` was lost in 4 of 10 runs at
  0.1 or 0.2, in 1 of 8 at 0.05 and in 0 of 13 at normal speed (flag alone included; for 0.05
  against normal speed, one-sided p about 0.38). agemu does not repeat such a tap, so the step
  timed out after 10 s. Holding every tap until the screen had been still for 300 ms halved the
  losses in the loop (3 of 12) but still lost the tap in 2 of 3 flows, so agemu does not do that.
  With the app launched afresh before every alert, the same two calls lost no tap (0 of 5 at
  normal speed, at 0.1 and at 0.05): the loss depends on what came before (in the flow, a tab
  switch). A tap sent 48 ms after the alert was drawn at rest was lost at normal speed in 5 of 6,
  at 0.05 in 2 of 6 and at 0.1 in 7 of 8; 100 ms or more later, none was lost (0 of 26). Check
  the effect of such a tap (`--wait-text`) and repeat it.
- **Typing: not slower in a targeted probe** (n=3 per arm, load about 20): `type hello` right
  after focusing a field took 288, 293 and 1073 ms normally, 257-281 ms at 0.1 and 242-294 ms at
  0.05, and the blinking caret drew the same 11-13 frames per second in every arm. In the agent
  flow above, though, the `type` step's median was higher in every fast arm (0.05: 719 ms,
  0.1: 1406, 0.2: 815, against 512 normally). The 0.05 arm ran at the highest mean load, so
  this is confounded and not settled.

**What an app sees.** UIKit and SwiftUI animations end sooner and their completion handlers run
sooner; every step reached the same screen (checked in the UI tree). Measured in the test
app at 0.05 (2 rounds, the same both times):
- **Animation delays shrink too.** A toast that fades in over 0.3 s, waits with
  `UIView.animate(withDuration:delay: 2.0)` and fades out was on screen from 311-313 ms to
  2631-2632 ms after the tap normally, and from 30-31 to 166-167 ms at 0.05, so an agent that
  checks for it a moment later can miss it (in one of three checks it did). The same toast hidden
  by `asyncAfter(2 s)` stayed until 2148-2150 ms (only the fades are shorter).
- **Timers and Core Animation keep their clock.** A 1 s `UIView` animation, a 0.5 s `Timer`, a
  1 s `CABasicAnimation`, and `asyncAfter(1.2 s)` followed by a 0.5 s `UIView` animation ended at
  1002 / 500-501 / 1001-1002 / 1761-1762 ms normally, and at 51 / 500 / 1001 / 1285 ms at 0.05.
  The `UIView` animation now ends before the timer fires: code that relies on that order behaves
  differently.
- **Other processes.** The share sheet (`UIActivityViewController`, another process) and the
  microphone permission alert (SpringBoard) were not faster in the first boot after the setting
  (share sheet 1027 / 1450 ms, alert 746 / 563 ms, its answer 401 / 416 ms, one run each; the
  share sheet's content was not verified in the UI tree).
  After a reboot with the setting in place (one boot each way, not counterbalanced: the
  normal-speed boot ran at load average 60-84, the fast one at 11-60, so a first look): pressing
  home in an app until the home screen was still took 461-559 ms against
  1023-1189 ms normally, tapping the app's icon until its screen was still 137-191 against
  508-580 ms, the microphone alert 108-338 against 518-713 ms and its answer 73-79 against
  406-430 ms; `agemu reset` took 710-1239 against 2259-4643 ms.
- Not measured: scrolling and swipes (deceleration, paging, pull to refresh), the software
  keyboard (agemu types with the hardware keyboard, so it did not come up), interactive
  transitions, and animations that UIKit does not drive (React Native's JS-driven `Animated`,
  Flutter, Lottie, `CADisplayLink` loops), which keep their speed while native transitions
  speed up.
- With the flag on, every key in the device's `com.apple.UIKit` domain takes effect, whoever
  wrote it; agemu writes only `UIAnimationDragCoefficient`.

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
- **Speeding up animations** with `UIAnimationDragCoefficient` set in the defaults alone: no
  effect on iOS 26.5, because UIKit reads that preference only with its internal preferences
  turned on. With them on it works; see "Fast animations (opt-in)".
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
