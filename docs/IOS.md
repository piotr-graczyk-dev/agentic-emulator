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
| tap / swipe | no simctl command; Maestro starts a JVM and an XCUITest runner | Indigo HID touch messages sent through SimulatorKit's `SimDeviceLegacyHIDClient`, the same channel Simulator.app uses for mouse clicks. Pixels are converted to screen ratios. A swipe is a touch-down, one move per ~16 ms, and a touch-up. **A tap first waits until the screen has had no big change for 48 ms** (at most 1 s): UIKit ignores a tap that lands during a transition, and a tap on a list still coasting after a swipe only stops it. A background thread samples the framebuffer surface's seed every 8 ms and, on every new frame, a 48×104 grid of pixels: a change of more than 0.4% of the grid is *big* (scrolling, transitions); a blinking cursor, a spinner or a ticking countdown are not, so they no longer hold a tap for the full timeout. A tap returns once its first frame shows (at most 120 ms). **A tap by selector** settles once (at most 1.5 s), re-reads the tree until the element's frame repeats if the screen did not go still, and waits up to 3 s for a disabled element to become enabled. On a still screen the tap then keeps the helper's settle, which returns at once unless a transition started during the tree read. An element confirmed at rest by its repeating frame is tapped at once, since a second settle on a screen that keeps changing would only time out. **An element that is new since the last input** (an alert's button, a sheet's) is tapped only once its presentation has finished; see "Taps on just-presented UI". `AGEMU_IOS_TAP_SETTLE=0` turns the helper's settle off. |
| type | Maestro `inputText` | Inserts the text at the cursor of the focused field by **pasting** it (the helper's pasteboard connection, then Cmd-V; see "Test capabilities"): the app gets the whole text in one change, as with Android's set-text path. The pasteboard's previous content (every item and type) is saved before and put back after, so typing does not change what the app or `agemu clipboard` sees. A single character is one key event. `type --keys` sends one HID key event per character (US layout, paced: each acknowledged, then 10 ms). Both wait for a still screen first (no big change; at most 600 ms) and return once the app has drawn the change (at most 500 ms). A paste is confirmed by the focused text field's text or by the pasteboard daemon's log line that an app read it; it is repeated when no app read it, or once more when an app read it but a readable field had not changed 2 s later (see "Confirming a paste"). Key events were not reliable in React Native controlled inputs (Expo development build of a real app): characters were lost or doubled at every pace tried, and a lost character in an e-mail field signed in to a different account; the paste always arrived whole. |
| key | — | `home` and `lock` are the hardware buttons (`home` returns once SpringBoard has acted on the press, see "The home button"); `enter`, `delete`, `tab`, `escape`, arrows are keyboard keys; `volumeup/volumedown` are consumer-page HID usages. iOS has no back key: `back` taps the navigation bar's back button (UIKit's `BackButton` identifier) when the screen has one, and otherwise does a **left-edge swipe**, flagged as an edge gesture, which triggers the navigation controller's interactive pop like a thumb does. On the loaded test host one edge swipe was not recognized, hence the tap first. |
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

### Taps on just-presented UI

**Why UIKit drops these taps.** A presentation (an alert, an action sheet, a sheet) makes the
presenting window scene ignore interaction events until the presentation completes:
`-[UIWindowScene _beginIgnoringInteractionEventsForReason:]` with "beginning transition for
presentation", then `_endIgnoringInteractionEventsForReason:` with "completing transition". A
touch in between never reaches `-[UIApplication sendEvent:]`. This was seen with a probe app
(`TapLab`, iOS 26.5) that swizzles those methods and `sendEvent:`, taps through the helper with no
wait at a fixed delay after opening the alert, and traces every frame: every touch sent before
the completion was absent from the app's log, every touch after it arrived. The deprecated
`UIApplication.isIgnoringInteractionEvents` stays false throughout. The UIKit action sheet logs
no scene-ignore, but touches before its presentation completion were dropped the same way (taps
100 and 200 ms after opening it: 6 of 6 lost; 300 and 400 ms: 6 of 6 landed).

**Why agemu's waits did not cover it.** Measured from the moment UIKit began ignoring touches
(the action sheet: from its action), with the tap long after (load average 14-67; alerts n=22-23,
sheets n=22, action sheets n=21):

| | SwiftUI alert | UIKit alert | UIKit action sheet | SwiftUI sheet |
|---|---|---|---|---|
| first big frame shows | 41-104 ms later (median 49) | 42-114 (51) | 33-93 (46) | 32-146 (39) |
| touches accepted again (completion) | 423-460 ms (435) | 425-456 (429) | 231-260 (243) | 510-535 (525) |
| "no big change for 48 ms" (the old settle), relative to the completion | 69-80 ms after | 8-75 ms after | 106-150 ms **before** in 21 of 21 | up to 34 ms **before** in 19 of 22 |

- The new UI is in the accessibility tree as soon as the presentation begins, before its first
  frame. `tap ... --wait-text "<alert text>"` returns at that point, and the next `tap --text OK`
  found the screen still, so it tapped at once (15-36 ms for the call) and the touch was dropped.
  Through an instrumented helper (UIKit alert, 8 tries), the one lost touch went out 51 ms after
  the app's action; the 7 delivered ones went out 23-82 ms after the completion.
- A sheet's tail and an action sheet's whole slide change too little of the screen to count as
  a big change, so the settle also ended inside the presentation (last row of the table).
- An alert's or a sheet's last visible change came 20-33 ms after the completion (18 traces). An
  action sheet's glass kept changing for 243-258 ms after it (2 traces).

**What a tap by selector does now.** The iOS backend remembers which elements the UI tree held
before the last input agemu sent (a tap, swipe, key, typing, launch, URL). An element that was
not there (or moved) is *new*: the agent can only have seen it after that input, so it may still
be presenting. For a new element the helper waits, before its usual settle:

1. if neither the element's frame nor the screen (a big change) has changed since the last input,
   until one of them does (at most 500 ms): the presentation has begun but not drawn yet;
2. until nothing in the element's column (its x range over the screen's full height) has changed
   for 48 ms and the screen has had no big change for 48 ms. The column catches an action sheet
   that slides in below the button while the button's own frame still shows the dimmed background.
   Once the screen is still, the column may keep changing for at most 300 ms more (a spinner or
   a video in the column never stops); the whole wait is at most 1.5 s. At normal speed an
   action sheet's column never went quiet: 40 of its 50 measured waits ended on this 300 ms tail
   (372-426 ms in all), not by seeing it ready, which left roughly 150-200 ms after the
   completion. For an element as wide as the screen (a list row) the column is the whole
   screen, so any small continuous change anywhere (a spinner, a video, a loading cell) holds a
   new element's tap for the 300 ms tail; on a screen that never goes still, with an element
   the daemon saw moving, the wait can reach 1.5 s before the usual settle.

What counts as new is the element's role, label, id and frame. A presentation that shows a
button identical to one in the read before the last input (a second alert with "OK" at the same
place, two permission alerts of the same size in a row) is not new and gets no wait; every
measured try opened one presentation from a screen without one, so that case is untested.
Neither were taps on new elements after a push navigation, the most common agent step, nor the
cost of the wait for a new element on a still screen (the still-screen rows below measured
elements the agent had already seen).

Every change counts here, not only big ones: the display monitor keeps, for a 96×208 grid of
pixels, when each sample last changed, so "has this column changed in the last 48 ms?" is
answered without waiting, and an element the agent could already see is tapped exactly as before.
That grid costs the helper about 0.5% of a core while the screen changes at 60 frames/s
(0.35-0.38 against 0.20-0.22 CPU-s per 30 s, 3 windows each, both helpers watching the same
simulator) and nothing on a still screen. A tap at coordinates is unchanged.
`AGEMU_IOS_TAP_SETTLE=0` turns this wait off with the settle.

The daemon log names each tap that waited, e.g. `ios tap on <udid>: button "OK" was new; waited
499 ms for it (nothing shown yet: true, still changing: false)`.

Results: see "Lost taps on just-presented UI (round 4)" under Benchmarks.

**No retry.** A tap is never repeated. A touch UIKit dropped and a touch on a control that shows
nothing when it acts look the same from outside: TapLab's "silent" button (no highlight, its
action shows only 2 s later) showed no change in its frame within 500 ms of a delivered tap in
20 of 20 taps, so a "no visual response, tap again" rule would have activated it twice each
time. `tap --id silent` through agemu activated it exactly once in 20 of 20.

### The home button

The simulator's HID sender has no press-count or terminal-press support, so SpringBoard emulates
the double-press detection (its log: "appears to be synthesizing home button events without press
count/terminal support; SpringBoard is providing emulation for max count:2"). It acts on a single
press only once no second press has come: 305-323 ms after the button-up in 16 presses (8 on the
home screen, 8 with Settings in front; load average 125-218). `agemu key home` used to return in
5-207 ms, before that; an app launched in between came to the front and was then sent to the
background by the press.

`key home` now returns once SpringBoard has acted on the press: with an app in front, once
accessibility reports SpringBoard in front; in any case not before 650 ms after the press (twice
the longest delay measured); then once the screen has had no big change for 48 ms. Reset no longer
presses home when SpringBoard is already in front (see "Soft reset").

Costs and limits:
- `key home` takes 0.66-1.3 s instead of 5-23 ms, for every caller, although an agent that
  leaves seconds between calls never met the race.
- `key home` can now fail: when accessibility does not report SpringBoard in front within 15 s
  after the press (before, the key was fire-and-forget).
- The 650 ms count from just before the press is handed to the helper: a press that takes over
  about 330 ms to reach the simulator (extreme host load) can still be acted on after the call
  returned.
- Since reset skips the press when SpringBoard is in front, SpringBoard's own state survives a
  reset: the home-screen page, an open folder, Spotlight. Notification Center, Control Center
  and the lock screen were not checked, nor iPad.

### Confirming a paste

`type` pastes: the text goes on the device pasteboard, then Cmd-V. A paste can arrive late (the
app's main thread is busy, or its own read of the pasteboard is slow) or be lost (UIKit drops
Cmd-V while the field is not ready for it). A second Cmd-V repairs a lost paste and doubles a late
one. Up to round 4 agemu read the focused field right after Cmd-V and again 150 ms later, and
pasted again when it had not changed: a paste the app showed later than that appeared twice
(`Wallpaper Wallpaper` in Settings' search field at load average 203 in the round-4 e2e). A field
that does not read back as text (a secure field, a web view) was not checked at all, so a lost
paste there stayed lost.

agemu now pastes again only when no app read the paste. The device's pasteboard daemon, `pasted`,
logs the line `KeyCommand` for each item an app reads for a paste that Cmd-V started (iOS 26.5;
for a web view the app's UI process reads it), and agemu's log stream (see "Logs and crashes")
already carries `pasted`'s lines. A paste is confirmed when the focused field changed (as before;
only then is the pasteboard put back) or when `KeyCommand` was logged after the Cmd-V. For a
field that does not read back as text that read ends the wait; a readable field gets 2 s from
the read to show the paste and is pasted once more if it has not (below). It counts as lost only
when, since the Cmd-V:
- no `KeyCommand` was logged and the field did not change;
- the app answered two UI tree reads (a tree read waits while the app's main thread is blocked:
  one sent during a 2 s block returned after 1.87 s) and at least 150 ms passed;
- `pasted` logged a line at least 100 ms (or as long as the last tree read took) after the last
  read returned, so a read logged before it has been delivered. agemu reads the pasteboard through
  the helper to make `pasted` log one. This relies on the log stream delivering one process's
  lines in order.

Each of these waits grows with the load. A lost paste is pasted again, up to three Cmd-Vs per
call. A field that does not read back as text is done once the read is in the log. An app can
read the paste and still drop it: once in 1,400 pastes of an earlier build of this change (Cmd-V
0.11 s after the field got focus), the app read it and neither inserted it nor called the
field's `paste(_:)`; so a readable field that has
not changed 2 s after the read is pasted once more (a paste the app shows more than 2 s after
reading it would then appear twice; none was seen). Without a
running log capture (`AGEMU_IOS_LOG_CAPTURE=0`, or before an app was installed or launched
through agemu or `logs` was called), the old window decides. Until `pasted` has logged one
`KeyCommand` on the device, a paste with no read in the log follows the old window's rule after
the longer wait (an unchanged readable field is pasted once more; an unreadable one is not), so a
runtime whose `pasted` does not log the line cannot make every paste look lost. A log that has not
caught up 3 s after that point decides nothing: no second paste.

Measured on the SE simulator against the round-4 build (results in "Paste confirmation" under
Benchmarks). Limits:
- The signal is a private log line of iOS 26.5's `pasted`; other runtimes were not checked.
- A web view's paste takes 49 ms (load average 11-19) to 87 ms (load 3-6) longer (medians). Its
  read was logged 120-131 ms after `backboardd` routed Cmd-V (3 pastes), but reached agemu later
  than that; why it arrives later on a quieter host was not diagnosed. While the app's main
  thread is blocked the call waits for the read (the old build returned without knowing whether
  the paste arrived).
- An app that reads the paste and then rejects or filters it (a numeric-only field typed with
  letters, a field at its maximum length): a field that cannot be read is not pasted again; a
  readable one is pasted once more 2 s after the read and again waited for, so the app sees two
  pastes, as with the old build, and the call takes about 4.5 s instead of about 0.3 s
  (designed and unit-tested, not measured on a device).
- The premise that a `KeyCommand` read means the paste will arrive failed once (the read-then-drop
  above, an earlier build). A readable field is covered by the 2 s repeat; in a field that cannot
  be read such a paste stays lost, as before.
- `type` with no focused field: no app reads the paste, so it is sent 3 times (1 before), and the
  call took 1.6-2.6 s instead of about 0.5 s (3 trials, a web view input the tap had not focused).
- Once `pasted` has logged one `KeyCommand` on a device (any UIKit paste), every later paste on it
  follows the log. An input whose paste read is not logged as `KeyCommand` (another toolkit's text
  input, a programmatic `UIPasteboard` read) would get up to 3 pastes if it cannot be read back,
  where the old build sent 1. Only UIKit text fields, SwiftUI `SecureField` and a `WKWebView`
  input were tested.
- The double-paste guard assumes that the stream delivers `pasted`'s lines in order, that the
  margin covers the app's background read, and that log timestamps agree with the Mac's clock to
  within a few ms (`SecureField` reads were logged about 4 ms after Cmd-V; a read stamped before
  the Cmd-V would not count). A stream event lost under load would make a read look missing, and
  the paste would be repeated. Of the final build's 700 slowed calls, 38 ran above load average
  100; load around 200 was not tried.
- The pasteboard is still put back only after the field showed the paste; after a secure field, a
  web view or a repeated paste the typed text stays on it.
- The host load of the round-4 failure (203) was not reproduced; the probe's slowdowns stand in
  for it. All of the old build's failures (147 of 560) came from three of those slowdowns (slow
  paste, a text field and a secure field refusing pastes); Settings' search had no failure on
  either build.

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
without a display (`AGEMU_IOS_OVERLAP_SWITCH=1/0` forces the overlap on or off). See "Several
devices at once" for what that limit cost in round 5's measurement.

### Several devices at once

How many devices one Mac runs at a useful speed, measured on the M2 Pro (16 GB, 12 cores) while
another session's work ran on it (an Expo/Metro server, Gradle, a booted iPhone 17 Pro Max
simulator with an app, OrbStack; 7-11 GB of swap in use; not controlled). Each device ran one
fixed agent flow with no think time, one `agemu` CLI process per call:
- iOS, 15 calls: relaunch Settings and wait for General, find and tap General (waiting for
  About), read the tree, tap About (waiting for its version row), screenshot; relaunch the test
  app, tap Increment twice (each waiting for the count), tap the name field, type, wait for the
  greeting, find it, screenshot, read the tree;
- Android (agemu-lab), 16 calls: terminate Settings and its search, launch Settings (waiting
  for "Network & internet"), find and tap it (waiting for Internet), read the tree, `key back`,
  tap the search field, type, wait for results, screenshot; terminate and launch the probe app,
  tap Refresh, read the tree, screenshot.

All active devices ran the flow at the same time, each at least 4 times per configuration; a
device that finished first ran more flows, not counted, until the others had (every counted
flow overlapped the other devices' flows by at least 99.5%). Speed is a device's median flow
time alone (only it booted; 5.7 s Android, 6.1 s iPad mini, 7.0-7.1 s iPhones) over its median
in the configuration; the total is the sum over the devices, in units of one device alone.
Devices were added and removed one at a time (for example 1, 2, 3, 4, 4, 3, 2, 1 devices),
each new device booted at least 60 s before, in sessions A1 (round 4's build plus parking), B1
(f4286db), A2 (as A1), then P and Q; host load average 4-61 (`.lab/r5/r5-s1-density`;
`AGEMU_IOS_MAX_BOOTED=6` throughout):

| devices running the flow | each device's speed (per session) | total | flows per minute | macOS memory pressure (max) |
|---|---|---|---|---|
| 1 | 1 | 1 | 8.4-10.6 | normal |
| 2: iPhone 17 Pro, Android | 0.97-1.00 | 1.98 (A1), 1.99 (Q) | 18.7-18.9 | normal |
| 3: + iPhone SE | 0.86-0.87 (A1), 0.62-0.80 (A2, one configuration of 4 flows), 0.88-0.91 (Q) | 2.58, 2.17, 2.68 | 20.1-24.7 | warn in 7 of 9 configurations |
| 4: + iPad mini | 0.57-0.67 (A1), 0.51-0.58 (A2) | 2.43, 2.27 | 22.8, 21.2 | warn |
| 5: + a new iPhone 17 | 0.49-0.54 | 2.59 | 23.8 | warn |
| 4, f4286db (B1) | 0.58-0.65 | 2.42 | 25.5 | warn |

Per-session medians of 4-12 flows per device and row. The 2-device row of A1 has 7 flows for the
iPhone 17 Pro.
- **The total levels off at about 2.2-2.7 one-device units from 3 devices on**: a further
  device divides it. A flow cost each device more CPU when others ran (an iPhone's process tree
  15 CPU-s per flow alone and about 16-21 CPU-s with 2-5 devices running, the iPad mini 21-24;
  the emulator 7.6 alone and 8.7-14 CPU-s; 1.2-2.3 cores per device during its flows), the host
  paged (9,900-42,000 pages swapped in per configuration of about a minute with 4 devices; with
  one device 80-1,500 in 17 of 19 configurations, 4,400 and 58,000 in the two right after five
  devices had run), and agemu's daemon with its helper and log streams took 0.1-0.8 CPU-s per
  flow. With 4 devices the device trees used 6.2-6.3 of 12 cores while the load average was
  18-61; the other session's CPU, compression and paging were not measured, so device CPU and
  host-wide contention are both consistent with the data.
- **Rounds 1-4's leaner devices did not move the curve.** The accepted build's simulators held
  1.8-2.5 GB each after a configuration (a fifth, the new iPhone 17, 3.3 GB), f4286db's 2.7-3.1
  GB. With 4 devices the bracketed sessions A1 and B1 gave 2.43 against 2.42, and A2 2.27 (A2
  had no solo flows of its own, used A1's and P's, and paged more: 32,000-42,000 swap-ins per
  configuration against 9,900-23,500). f4286db completed more flows per minute (25.5 against
  21.2-22.8): about 70% of that gap is its Android flow being faster alone (3.8 against 5.7 s:
  the tap and screenshot waits of "Selector taps" in ANDROID.md at zero think time), the rest its
  iOS flows being 3-13% faster at 4 devices. The builds were not alternated per configuration,
  and an effect under about 10% cannot be excluded at these n.
- **Parking idle devices did not speed up the active ones**: with 3 devices running the flow
  and two more booted, parked (0.0% of a core each) or idle (6-40%), the three ran at 0.83-0.86
  and 0.76-0.86 (8 flows per device per arm, alternated idle, parked, parked, idle). The two
  idle devices took 0.2-0.55 of a core together, the three running ones 5.2-5.6 cores; parking
  frees CPU, not memory (see Parking).
- **Memory pressure and the simulator limit**: with one simulator and the emulator booted,
  macOS reported normal pressure (once warn, right after the SE shut down) and 41-54% of memory
  available (`kern.memorystatus_level`); with two simulators and the emulator, warn in 7 of 9
  configurations (2 stayed normal) and 29-41% available. agemu keeps one simulator booted while
  memory is short (warn or critical, or less than half available), so without
  `AGEMU_IOS_MAX_BOOTED` it would have shut one of the two simulators down when the second
  booted or at a later `boot` call. Alternating the two (2, 3, 3, 2, 2, 3 devices; 12 flows per
  device and arm), the two simulators and the emulator made 24.7 flows per minute against 18.9
  (totals 2.68 and 1.99). The rule is unchanged: this is one 16 GB host at zero think time;
  critical pressure did not occur, the load-~400 case that made the rule (see "End-to-end run")
  was not retried, and 8 GB Macs were not measured.
- **Taps on a list that changes after the read**: 13 of the 570 flows (warm-up and uncounted
  ones included) failed, all at the tap on Settings' General: 8 on the iPhone SE and 5 on the
  iPhone 17 Pro, 3 of them with f4286db, 5 in the first flow after a boot. In the 8 failures with
  a capture, Settings had inserted a "Ready for Apple Intelligence" row above General after the
  tree read, which moved General down (to y=1141 on the 17 Pro, y=713 on the SE): the tap at
  General's old position hit the inserted row, and 6 of the captures showed Apple Intelligence &
  Siri's page, 2 the Settings list. A tap by selector does not check that the element is still
  where it was read.

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
4. checks that accessibility reports SpringBoard (`com.apple.springboard`) as the frontmost app
   (on iPad too, found by the hit test above); only when it does not, presses home and waits
   for it (see "The home button"). Then it waits until the screen has been still for 300 ms (at
   most 2 s): an app launched while a terminated process was still exiting failed to launch.
   `launch` also retries a "failed to launch" twice (after 0.3 s and 0.7 s) when the app is
   installed. Reset used to press home with SpringBoard already in front; SpringBoard acts on
   that press about 0.3 s later, and an app launched in between went to the background (see
   "The home button" for the measurements). SpringBoard's own state (the home screen page, an
   open folder, Spotlight) is no longer touched by reset.

The daemon log has each phase, e.g. `ios reset <udid>: 1736 ms (app lists 1, terminate 25 apps
31, uninstall [] 0, appearance/location/pasteboard/privacy 40, simctl keychain done at 158, home
1578)`.

It does **not** reset what `simctl erase` would: data stored by built-in apps (Settings search
history, Safari, Contacts, Photos...), other system settings an agent changed (language...),
notification permissions given to built-in apps in their system alert, and media added with
`simctl addmedia`. Use `agemu shutdown` + `xcrun simctl erase <udid>` for a full wipe.

### Parking

`agemu park` stops a booted simulator in place: it sends SIGSTOP to its `launchd_sim`, then to
every process `launchd_sim` started (all simulator processes are its direct children on iOS
26.5; about 120-150 processes), looking again until none is left running. `agemu resume`
sends SIGCONT to the children, then to `launchd_sim`, and returns once the device answers as
after a boot (the helper's readiness check: accessibility, framebuffer, HID). Any other call on
a parked device resumes it first (`logs` and `crashes` excepted, they read what was captured),
so an agent never acts on a stopped device; `devices` lists it as `parked`. A daemon that
restarts finds parked agemu simulators (a stopped `launchd_sim`) on its first look; a stopped
simulator that is not agemu's is left alone, and a call naming it is refused as any other call
is. The resume continues only what the park stopped: a process that was stopped before (by a
debugger, say) stays stopped, except after a daemon restart, which does not know them. Shutting
down a parked simulator, also to make room for another boot, continues it first. A post-boot
lean pass that falls into a park waits for the resume, and the passes after it move by as long.
By default nothing parks a device by itself; `AGEMU_AUTO_PARK=<duration>` makes the daemon park
the devices it serves after that long without a call (see Auto-park below).

Measured on agemu-iphone-17-pro (iOS 26.5) with the test app's Park screen (`-park`: a counter,
a 0.5 s timer, a TCP connection to a host server that writes a line a second, and a local
notification due 20 s after it was scheduled), 60 s parks, arms interleaved, host load average
3-35 (`.lab/r5/r5-s2-parking`). CPU is `.lab/meter.py` over the simulator's process tree
(`launchd_sim` and its children), which a park stops by construction; agemu's helper, the
daemon and CoreSimulatorService were not measured in either arm:

| | parked (SIGSTOP) | running idle | shutdown + cold boot |
|---|---|---|---|
| simulator CPU over 52 s | **0.0 CPU-s** (17 of 17; and 0.0 over 592 s of a 600 s park) | 6.5 CPU-s, 12.5% of a core (10.5-57%, n=11) | none while shut down |
| `agemu park` | 87 ms (82-106, n=11; 121-152 processes) | - | `shutdown` 0.3 s |
| back to agent-ready, the app on screen | **133 ms** (81-209, n=10) through `agemu resume` or the next call; 138 ms (72-358, n=17) counting the lab script's SIGCONT arm | first `ui` 32 ms (18-55, n=11) | **9.7 s** (8.3-15.1, n=7): boot 8.4 s, then the app's launch |
| after a 600 s park | 490 ms (n=1) | - | - |
| the app's state (counter, process) | kept, 18 of 18 | kept | lost (new process) |

What the app saw after a resume (18 parks, 17 of 60 s and one of 600 s):
- **Clocks**: the timer's next tick came one park later, by the wall clock and by the uptime
  clock alike (60.0-60.5 s; 600.4 s). Unlike a device that sleeps, `mach_absolute_time` runs on
  through a park: the simulator's processes are host processes and the host's clock does not
  stop.
- **Network**: the TCP connection to the host stayed open in 16 of 18 parks; the lines the
  server wrote during the park arrived at once on resume (the newest line's age peaked at
  59.4-60.0 s; 599 s). In the other 2 the connection had ended: in one it was open before the
  park and at end of file after it (reconnects 0 -> 1), in the other it was already at end of
  file when the park began (reconnects 3 -> 4, only 4 lines). The host server's log was not
  kept, so whether the server or the park ended them is not known.
- **A pending local notification** due 20 s into the park was shown on resume, 41.5-41.7 s late
  (582 s in the 600 s park); in the idle arm it was 18-40 ms late.
- **No crash, no watchdog kill**: the same SpringBoard, backboardd, `launchd_sim` and app pids
  before and after in 18 of 18, no crash report or unexpected exit of the app (`agemu crashes`),
  no log line of a watchdog firing, a jetsam kill or a termination with a reason. `maild` logged
  "Remote service has not checked in recently. Re-starting" after 1 of the 17 parks whose log
  was checked (0 of 11 idle windows). One park record right after a cold boot also lists
  Calendar killed, most likely agemu's own post-boot stop (the lab's parks did not tell the
  daemon). The UI tree read the app's screen and a tap landed after every resume.
- **Memory**: a parked simulator keeps its memory; the host can compress or swap it out because
  no process touches it. On this host, which was already swapping, a running idle simulator
  held 0.31-0.46 GB of its 1.6-2.2 GB footprint uncompressed in RAM (1.0-1.2 GB in the first
  minutes after a cold boot; `footprint --swapped`: dirty minus swapped, summed over the
  simulator's processes); parked, that fell by a median 0.054 GB over 52 s (0.003-0.64 GB, n=18;
  idle in the same series: 0.041 GB) and to 0.19 GB after 10 minutes.

When memory ran short (agemu-iphone-se-3 and agemu-ipad-mini booted while the test app's
simulator waited, the host's free memory down to 0.11-0.39 GB, 3 trials per arm,
interleaved; the parks here were made by the lab's own SIGSTOP script, not `agemu park`): the
parked simulator kept 0.095-0.174 GB uncompressed (it had 0.19-1.00 GB), the running idle one
0.169-0.300 GB (it had 0.23-0.34 GB); medians 0.108 against 0.208 GB. The simulator went from
130 to 70 processes during this series (agemu's lean pass stopped 10; others exited, 10 of them
while it was parked), and pids were not checked in these trials. After the other two shut
down, the parked one was agent-ready 99-135 ms after the resume, with the app's state; the idle
one's first `ui` took 43-56 ms. So on this host parking returned about 0.1 GB more to the host
than leaving the simulator idle (n=3, the ranges almost touch); the rest of an idle simulator's
memory was already compressed.

`agemu shutdown` frees all of a simulator's memory; parking keeps the app as it is and is
agent-ready 70x sooner than a cold boot plus the app's launch.

#### Auto-park (2026-11-08)

With `AGEMU_AUTO_PARK=<duration>` (at least 10 s) the daemon counts every call on a device from its
start to its end and parks a simulator it has served once it has had no call for that long; it
looks every duration/20 (1-10 s). It parks only booted simulators named `agemu-*` (not booting,
not parked already) that it has served since it started; it does not see another daemon's,
`simctl`'s, Simulator.app's or a person's use of a simulator, so one driven that way after this
daemon's last call to it is parked under them, and their calls then fail until this daemon
resumes it (a second daemon calling `ui` every 2 s: parked after 10.6 s, its 5 later calls each
failed after 5.0 s, "accessibility service did not answer"; one run by the review). `logs` and `crashes` reset the idle time but
do not resume. A call that starts while the parker stops the simulator waits for it (74-127 ms,
median 93, n=47 parks) and resumes it. Device ids are compared in CoreSimulator's spelling (the
UDID upper case), so `ios:<lower-case udid>` shares the count, lock and park state. While a
profile switch runs, no simulator is parked (it may boot any of them); before it the daemon
resumes the simulators it parked, and the switch resumes the simulator it switches to if it is
parked (also after an agent's `agemu park`; before, the switch waited on its stopped processes). A park no longer counts as a use of the simulator for the
least-recently-used order that a boot shuts simulators down by (before, a park moved it to most
recently used); a park and a shutdown of the same simulator wait for each other. Before that, a park
started at the same moment as a shutdown (offsets -30 to +250 ms, the opt-in
`TestIntegrationParkDuringShutdown`, run by this lane and by an independent review on 3
simulators) hung the shutdown for 33 s in 3 of 19 rounds on 00106a2, against 0 of 24 with the lock
(Fisher one-sided p ≈ 0.08; the base rounds ran at higher host load, 51-53 against about 3 in one
pair of blocks, load the test's own boots make). After both hangs in the review the simulator
never became agent-ready again: one had `containermanagerd` crash on every later boot (25
reports), the other ended each of 2 later boots with no frontmost application after 180 s, and
both were deleted. Why is not established; CoreSimulator force-killing processes that are still
stopped when its shutdown times out is a suspicion. On 00106a2 without auto-park this needs an
agent's `agemu park` to overlap a boot that shuts the same simulator down to make room. Stopping the daemon
(SIGTERM) resumes the simulators it parked; after a SIGKILL they stay stopped and the next
daemon finds them parked (a stopped `launchd_sim`), as after `agemu park`.

Measured on agemu-r6-autopark-a (iPhone 17 Pro, iOS 26.5, lean), host load average 2.5-8
(`.lab/r6/autopark`):
- Parked 15.0-16.0 s after the last call with `AGEMU_AUTO_PARK=15s` (n=6), every child of
  `launchd_sim` stopped (166-178); the first `ui` after it took 66-94 ms.
- 18 of 18 calls of every kind (ui, find, screenshot, wait, tap, apps, appearance, clipboard, key,
  launch, terminate, boot, resume) resumed the simulator and worked; logs, crashes and park left
  it parked. No park during a 45 s `wait`; parked 15.8 s after it. 12 rounds of 6 calls at once
  at gaps of 14.5-16.1 s: 72 calls, no error, 4 parks (Android: another 72, 5 parks).
- On the test app's Park screen (50 s without a call), auto-park against `agemu park` 20 s into
  the wait, alternated: back to the app's screen in 155 ms (108-181) against 150 ms (115-215), n=4
  each; count, pids (the app, SpringBoard, backboardd, `launchd_sim`) and the TCP connection to
  the host (0 reconnects) kept in 8 of 8; no crash, no watchdog or "has not checked in" line;
  0.0 CPU-s while parked in both arms.
- Device CPU running idle, 0 parked: on the home screen 0.9-2.1% of a core per simulator (two
  iPhone 17 Pro, 1.48 CPU-s per minute together, 1.16-2.49, 4 blocks) and 2.5-4.4% on an iPhone
  16e (the review, n=3, one more window of 78.7% a minute after a boot left out); with the test
  app in front 4.4% (4.2-4.7, n=4, 20 s windows alternated, a new iPhone 17 Pro; an earlier 8.3%,
  7.4-8.4, n=3, came from the simulator later damaged by the base race runs); r5-s2's Park screen,
  with a 0.5 s timer and a TCP stream, 12.5%. So auto-park saves about 0.5-7.5 CPU-s per idle
  simulator-minute, depending on the device and what its screen runs.
- It did not speed up a simulator in use: with two idle simulators parked or not (4 blocks of 5
  flows per arm, alternated, `AGEMU_AUTO_PARK=30s`), the active one's flow took 7.31 s against
  7.22 s (block medians, 7.25-7.52 and 7.16-7.44), 4.44 against 4.28 CPU-s per flow, host CPU 4.17
  against 4.15 busy cores; footprints, free memory and swap-ins did not change.
- Profile switches onto a parked simulator (a new iPhone 17 Pro and iPhone Air, the only agemu
  simulators of their types; switching to the profile iphone-air from the 17 Pro): with
  `AGEMU_AUTO_PARK=10s`, the Air parked 10.2-11.1 s after its last call while the 17 Pro was in
  use, and the switch resumed it and returned in 60-125 ms (4 of 4; the daemon's resume before the
  switch); with auto-park off and the Air parked by `agemu park`, 52-59 ms (3 of 3; the backend's
  resume). Each time the Air was running afterwards and its screenshot was 1260x2736. Before the
  fix such a switch was still waiting after 60 s (2 of 2, the review).
- `ios:<lower-case udid>`: a call under it resumed a park made under the upper-case id (131-196
  ms, 3 of 3), and a 25 s `wait` under it held off the park (0 stopped samples in 44 polls, 3 of 3).
- `scripts/e2e-ios.sh` with the device names changed to the lane's simulator and the profile
  switches left out (they use the shared agemu-iphone-se-3 and agemu-ipad-mini): 85 of 85 with
  auto-park off (twice) and with `AGEMU_AUTO_PARK=10s` (twice); one more run with 10 s failed 2
  checks when the tap on General hit an "Apple Intelligence & Siri" row inserted above it (see
  "Taps on a list that changes after the read"), with no park in that run until its end.

It stays opt-in: the CPU it frees did not make the devices in use faster, and a park changes what
the app sees (clocks, timers, see above) without the agent asking.

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
  focused field shows the paste. When the field cannot show it (a secure field, a web view or
  custom input that does not read back as text, or a paste that had to be sent again), the typed
  text stays on the pasteboard, as before: the app may not have finished reading it, and a restore
  that won that race would make it paste the old content. (Such a paste is now confirmed by the
  pasteboard daemon's log, see "Confirming a paste"; the log shows when the read started, not
  when it ended, so the restore rule is unchanged.) The restore was checked with one plain-text
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

## Logs and crashes

`logs`, `crashes` and the crash notice of [CAPABILITIES.md](CAPABILITIES.md#logs-and-crashes)
(`internal/ios/logs.go`, `crashes.go`, `procwatch_darwin.go`). The Simulator's processes log to
the simulator's own unified log, which the Mac's `log` does not show, and its apps are Mac
processes, which the daemon can watch directly.

| Need | Mechanism |
|---|---|
| log lines | one `simctl spawn <udid> log stream --style ndjson --level info` per booted simulator, for the whole device, read line by line into two rings of 2 MiB (by `LogEntry.Size`): user apps' processes, and everything else. It starts when an app is installed or launched through agemu, or at the first `logs` call, and runs until the device shuts down or the daemon exits. A `logs` call reads the rings: nothing runs on the device per call. |
| stopping it | the stream runs inside the simulator; simctl only relays it. A relay killed with SIGKILL left the simulator's `log` process running in 1 of 2 checks, and 19 such leftovers of an earlier build (from daemon restarts) used about 1.6% of a core each. agemu stops the relay with SIGTERM, which simctl passes on (SIGKILL after 2 s), and marks its stream with `--timeout <1000000 + daemon pid>m` (a valid timeout of about two years): a capture start ends the marked streams on the device whose daemon is gone, or that its own daemon no longer reads. Checked: after the daemon was stopped with SIGTERM (3) or SIGKILL (3), no stream was left in the device, and the next start ran exactly one. |
| lines from before the stream | once per capture, `log show` of user apps' processes (and runningboardd's exit lines) from 30 s before the start to the stream's first line, put in front of the rings (at most 4 MiB kept). The unified log's store keeps default, error and fault lines, not info lines, so the window before `capturedFrom` stays `partial`. The stream delivered its first line 0.85-3.1 s after it was started (n=4, load average about 12), and once 11 s (load about 250); the backfill read took 1.3-4.3 s (14 starts) and held up to about 5300 lines. |
| an app's lines | the executable is inside the app's bundle (`Containers/Bundle/Application/<UUID>/<App>.app/...`, extensions included); bundle ids come from the device's installed apps (built-in apps from CoreSimulator). Every install of the app seen during the capture counts, so lines from before a reinstall (`clear-data`) stay the app's. |
| the app ended | kqueue `EVFILT_PROC`/`NOTE_EXIT` on the pid agemu's launch returned (the exit time, within milliseconds); then runningboardd's line in the stream says how: `exited with context <RBSProcessExitContext\| ... domain:signal(2) code:SIGSEGV(11)>`, `voluntary` for `exit()`. Processes agemu did not launch (an app opened from its icon) are known from runningboardd's lines alone. |
| why | the app's own last line: CoreFoundation's `*** Terminating app due to uncaught exception 'X', reason: 'Y'` with the first throw call stack, or the Swift runtime's `App.swift:399: Fatal error: message`. |
| the full report | the `.ips` the Mac's ReportCrash writes to `~/Library/Logs/DiagnosticReports` (bug type 309): only reports whose `procPath` is in this device, parsed once, never deleted. Frames are the faulting thread's (an uncaught NSException: its `lastExceptionBacktrace`), symbolicated and demangled. |

How an end is reported (`kind`, `exception` until the `.ips` adds more):

| runningboardd's exit context, or the app's last line | kind | exception |
|---|---|---|
| uncaught NSException (the app's line) | crash | its name; reason and frames from the line |
| Swift runtime error (the app's line) | crash | `Fatal error` (or `Precondition failed`); reason: the message and `file:line` |
| `signal` SIGSEGV, SIGABRT, SIGTRAP, SIGBUS, ... | crash | the signal; with the `.ips`: `EXC_BAD_ACCESS (SIGSEGV)` and its subtype (`KERN_INVALID_ADDRESS at 0x10`) |
| `signal` SIGKILL, SIGTERM | killed | the signal |
| `frontboard` 0x8badf00d | killed | `watchdog (0x8badf00d)` |
| other domains | killed | domain and code |
| `voluntary` | exited | - (reason: it exited on its own) |
| an exit seen by kqueue before runningboardd's line | exited | - (upgraded when the line comes) |
| `frontboard` force-quit 0xfbfbfbfb (agemu's terminate and relaunch, `simctl terminate`, a quit in the app switcher), `tcc` (a privacy permission change) | not reported | |

Exits agemu causes are not reported: an exit of the app's processes from the start of
`terminate`, a relaunch, `install` (over a running instance: runningboardd reports `runningboard`
code 0), `uninstall`, `clear-data`, `permission` or `reset` until 5 s after the call returned,
except processes agemu launched after the call began (the one a relaunch starts, or an app
launched right after an install that crashes at once), and except any end with crash evidence: a
signal other than SIGKILL or SIGTERM, the app's own uncaught-exception or Swift-error line, or an
`.ips` for the process. The exits agemu's calls were seen to cause carry none (a force quit,
runningboard code 0, tcc), so an app that crashes while agemu changes a permission, or that is
opened by a link right after an install and crashes, is reported. (An earlier build ignored how
the process ended inside that window, and hid such crashes, the `.ips` included; a unit test
now crashes a process launched before a `permission` call inside its window.) An exit kqueue
sees before runningboardd's line still counts as agemu's until the line comes, so such a crash
is told by the next call. Right after an exit, the watcher waits up to 150 ms for runningboardd's
line, so the first notice already says how the app ended. runningboardd's lines are believed
only from the system's runningboardd (not from an executable of that name in an app bundle).

**Crash to agemu's report** (`internal/ios`, test app on agemu-r4logs, iPhone 17 Pro, iOS 26.5;
each trial launches the app through agemu with `-crash <kind>`, then `wait`s for an element that
never appears, timeout 15 s, over the daemon socket; the app writes the time just before it
crashes; 10 rounds, kinds interleaved; load average 26-65). These ran on an earlier build of the
capture (a 2-minute backfill of the whole device), with 0-19 in-simulator log streams that
earlier daemon restarts had left running; the final build was checked by a smoke run (below), not
re-measured:

| kind | the wait ended after the crash (median, range, n) | the error and notice carried |
|---|---|---|
| uncaught NSException | 178 ms (78-328, n=10) | `NSInvalidArgumentException`, its reason, 3 frames (the notice's limit; `crashes` has 4-6 from the log line) |
| Swift `fatalError` | 195 ms (58-454, n=10) | `Fatal error`, `agemu probe fatalError (AgemuTestApp/App.swift:399)` |
| bad memory access | 215 ms (74-319, n=10) | `SIGSEGV` |
| `abort()` | 201 ms (151-403, n=10) | `SIGABRT` |
| `exit(0)` | 193 ms (79-791, n=10) | `exited` |
| SIGKILL from outside | 186 ms (79-507, n=10) | `killed`, `SIGKILL` |

60 of 60 waits failed at once with code `app_crashed` (CLI exit 4), all within 791 ms. The same
trials on a daemon without the iOS watcher (the logs skeleton build; run separately about 70
minutes later, not interleaved): 10 of 10 waits (NSException and bad access, 5 each) ran to their
15 s timeout, 11.3-12.0 s after the crash, with `timeout after 15s waiting for
{"id":"never-there"} to appear`; a wait for an element that never appears always times out, so
that arm only shows what an agent learned without the watcher. The latency includes the wait's
own polling (it re-reads the UI tree between checks). The watcher's own cost per call:
`AppState` 0.49 µs (474.6-501.6 ns, 5 runs of `BenchmarkAppState`, no allocations).

For an NSException, the notice's 3 frames are the exception machinery's (`__exceptionPreprocess`,
`objc_exception_throw`, `-[NSException init]`); the app's own frame is the 4th, in `crashes`,
with its Swift name mangled. Signal and Swift crashes carry no frames in the notice.

**The .ips comes later, and not always.** ReportCrash wrote 19 of the 40 crashes' reports, 1.7 to
42.9 s after the crash (median 10.4 s: NSException 14.8 s, fatalError 10.4 s, bad access 7.0 s,
abort 4.9 s; the 42.9 s was an NSException); outside these runs, four earlier reports came
74-131 s after their crash (load average 14-135). Then it stopped: the Mac's
`osanalyticshelper` logged `Saved type '309' report (25 of max 25)` for the test app and wrote
none of its next 21 crash reports that day. So frames of a signal crash or a Swift runtime error
come only with the `.ips`: for 13 of 30 such crashes, 1.7-17.5 s later, and for the other 17
never; the crash itself is still reported at once. An NSException's name, reason and first throw call
stack come from the app's own line (the unified log cuts that message at about 1 KB, so 4-6
frames, Swift names mangled).

**Reading logs** (12 rounds, arms interleaved; each round logs one line per level and every read
must contain it; load ~15; the same earlier build): `logs dev.agemu.testapp --since 1m` over the
daemon socket 1.11 ms (median; 0.79-19.9, n=12; 0.79 ms in the daemon), through the CLI 8.84 ms
(6.1-47.0, n=12), against `xcrun simctl spawn <udid> log show --last 1m --predicate 'process ==
"AgemuTestApp"'` 961 ms (885-1604, n=12). 12 of 12 reads found the line in every arm. The
workloads differ: agemu returned at most 200 lines (the default limit) and filters by the app's
bundle, `log show` a median 648 lines by process name; process start-up dominates its time.

**Completeness**: the app logged 1000 numbered lines as fast as it could (38-74 ms): agemu held
1000 of 1000, 5 of 5 bursts (the store too), and 12 of 12 more in the fidelity runs below.
Bursts of 10,000 lines (414-488 ms): all 10,000 held, 3 of 3 (a 1 MiB `logs` result returned the
newest 8456 and counted 1544 as omitted).

**Cost** (capture on: the daemon as shipped, its stream running; off: a daemon with
`AGEMU_IOS_LOG_CAPTURE=0`; the test app in front, relaunched for each 30 s window; arms ABBA,
4 windows each; `.lab/meter.py` on the simulator's processes, which include the stream's `log`
process, and on the daemon's, which include the helper and the stream's simctl relay; load
average 8-24):

| | simulator CPU, off / on | daemon CPU, off / on | daemon memory, off / on | simulator memory, off / on |
|---|---|---|---|---|
| app idle (6-37 lines/s on the device) | 8.2% / 7.5% of a core (ranges 7.0-15.6 / 6.4-9.3) | 0.0% / 0.0% (0-0.2) | 20.1 / 35.1 MB | 1547 / 1555 MB (ranges overlap) |
| app logging 200 lines/s (208-289 lines/s on the device) | 11.3% / 14.6% (10.8-18.1 / 13.0-21.9) | 0.0% / 0.8% (0.7-1.0) | 20.1 / 41.8 MB | 1556 / 1571 MB (ranges overlap) |

So at idle the capture costs no measurable CPU and about 15 MB in the daemon's processes (the
relay 8.5 MB; `agemud` itself 7.5 MB without the stream, 18-27 MB with it while the app logged 200
lines/s, n=6 samples each). While the app logs 200 lines/s it costs about 4% of a core (3.3 points
in the simulator, where the stream's `log` process formats every line as JSON, 0.8 in the daemon,
which parses them: 3.8 µs per 1.4 KB line in `BenchmarkIngest`) and about 22 MB in the daemon's
processes. A read of the app's lines out of 20,000 held: 0.50-0.57 ms (`BenchmarkQueryApp`).
The thesis' limit was 1% of a core: at 200 lines/s the capture is above it.

Starting the capture is not free either: the first launch or install after a boot (or a daemon
start) waits while agemu lists the Mac's processes (`ps`, about 30 ms here, to end streams nobody
reads) and starts simctl, and an install first reads the bundle id with `plutil`. The one-time
backfill (`log show`, 1.3-4.3 s) runs inside the simulator during the app's first seconds; its
effect on the app's start-up was not measured.

**Fidelity**:
- The app's own logging is slower while the stream runs: 1000 lines took 29.2 ms (27.4-33.9)
  with capture on and 24.3 ms (21.6-34.1) with it off: 4 ABBA windows per arm of 3 bursts each
  (the bursts within a window are not independent); all 4 "on" window medians (28.96-29.42 ms)
  are above all 4 "off" ones (22.13-25.67 ms). Two earlier ABBA sets showed no difference (59
  against 64 ms, 100 against 96 ms); they were set aside because their "off" windows also had
  streams that earlier daemons had left in the simulator (see "stopping it"), a plausible but
  untested explanation, so one more clean set would make the result firm.
- A line at each level (`Logger` debug, info, notice, warning, error, critical; `os_log`
  default; `NSLog`; `print`; a three-line message): info and notice read as `info`, warning and
  error as `error`, critical as `fatal`, `NSLog` is tagged `Foundation`, the multi-line message is
  one entry; debug and `print()` lines are not captured.
- The app's `os_log_type_enabled(.debug)` reads false with agemu's capture running, as without
  it; with a `log stream --level debug` on the device it read true, also in an app already
  running (and false again once that stream ended). That is why agemu streams at info level.
- On the Simulator, FrontBoard gives a launching app 599.82 s before its scene-create watchdog
  fires (its log: `Made scene-create policy ... timeValue: 599.82s`): a launch whose main thread
  hung 25 s was not killed (its screen showed after 26.6 s; no crash, no notice). Jetsam was not
  tried (the Mac's memory would run out first).
- `crashes` reads the Mac's reports; ReportCrash, not agemu, decides whether one is written.

**Device checks** (`.lab` smoke script through the CLI on agemu-r4logs, the same commands as the
block added to `scripts/e2e-ios.sh`, plus `--app`, cursors, the exits agemu causes, reset and a
reboot): 40 of 40 on the final build. Two earlier runs failed checks that led to fixes: an install
over the running app was reported as `killed` (`runningboard 0`), and a crash 4 s after an
install was hidden by the install's 5 s window; one check read too few lines. Covered: each level,
`--app` holds only the app's processes, a cursor returns only newer lines, `since launch` holds the
startup line, the crash fails the wait with exit 4 at once and `crashes` has the exception and the
app's frame, the next call carries no second notice, relaunch / terminate / permission revoke /
clear-data / permission reset give no notice, reset empties `logs` (no app line) and `crashes`,
and after shutdown and boot the capture restarts (startup line held), an old cursor is partial and
a crash is noticed.

**Merged build** (agemu-iphone-17-pro, iOS 26.5, through the CLI; load average 20-67):
`scripts/e2e-ios.sh` passed 125 of 127 checks; its logs and crash checks all passed, and the two
failures were one event, a double paste in Settings' search field (see "Running"). Smoke:
- `wait` for a missing element after `launch -- -crash <kind> -crash-delay-ms 1500`: exit 4
  9 of 9, 145.5 ms (NSException), 131.9 ms (`fatalError`), 114.3 ms (bad access) after the
  app's own crash time (medians, n=3 each, load 60-67).
- A crash inside agemu's own call window: the app launched through agemu with a bad access 2.5 s
  later, and 1 s after the launch `permission grant <app> location` (iOS kept the app running;
  a camera grant ends it, `tcc`, before it can crash). The merged build reported 3 of 4: the
  next call carried the note and `crashes` listed the SIGSEGV; in the fourth (the first trial
  after a daemon restart) no exit of the app had been seen when it was checked 4 s later. The
  same build without the crash-evidence rule, alternated with it: 0 of 3, although each of those
  trials logged runningboardd's `signal ... SIGSEGV(11)` exit.

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
3. **From the second boot on, the wallpaper gallery does not start** (`internal/ios/posterboard.go`).
   Why PosterBoard starts it at every boot: it records when it last refreshed each provider's
   wallpaper suggestions in `TransientInfo-*.plist` files in the device's `tmp/PFTemporaryDirectory`,
   and those of one boot were gone at the next, so at every boot its log reports each provider's
   suggestions as expired (an 8-hour interval) and it starts all 14 providers (the lock screen's
   among them) to fetch them again, then renders the gallery's snapshots. Stopping the extensions
   (item 2) comes after they have rendered. PosterBoard has a switch for this, its keynote (demo)
   mode: `KEYNOTE_MODE` in `com.apple.PosterBoard.unprotectedUserDefaults`. With it on, PosterBoard
   skipped its boot-time update (its update log, `PBFExecuteUpdateHistory`: "Keynote mode is enabled;
   we aren't going to prewarm anything") and started only the lock screen's provider. agemu turns it
   on with the device's `defaults` during the gallery passes of a cold boot, once PosterBoard has
   logged a successful update in that boot for the iOS build of its data store; on the first boot
   of 5 new iPhone 17 Pro devices that was at the 45 s pass on 4 (45.8-46.1 s after agent-ready)
   and at 70.8 s on the fifth (a 55 s boot on a loaded host). The passes go on (every 15 s, up to
   5 min) until then. `AGEMU_IOS_LEAN=0` deletes the setting (a failure to delete it is logged and
   tried again at the next boot; the launchd jobs are turned back on regardless). PosterBoard's
   data store migration, which runs when the iOS build changes, deletes it too according to its
   log text (not tried here); agemu then sets it again after PosterBoard has rebuilt the gallery.

   What stays the same, checked through agemu after 9 boots with the setting and 4 without: the lock
   screen draws its wallpaper and clock (screenshots of 226-229 KB in both arms), the poster
   switcher (long press on the lock screen) opens, its "+" opens the wallpaper gallery (Emoji,
   Colour, Collections, ...), an Emoji wallpaper from it opens in the editor (its provider starts
   then; 13 of 13), and Cancel goes back to the switcher (12 of 13; in the first check the script
   tapped the editor's emoji picker closed too early). The home screen was looked at in only 2
   screenshots with the setting (wallpaper and icons; its widgets are empty, as lean mode already
   makes them). The first time the gallery is opened on a new device (at its second boot), both
   builds render its previews on demand: the first preview showed after 17.8 and 18.9 s and all 9
   on screen after 19.5-19.8 s without the setting (2 new devices, host load 27-53), after
   4.5-17.0 s and 16.0-17.5 s with it (5 new devices, load 9-42; all 9 timed on 3 of them; the
   two fast ones, 4.5 and 7.8 s, are not explained), so "not slower" rests on thin, unevenly
   loaded data. On a device where it had been opened before, its first preview showed after 0.9
   and 1.1 s with the setting (n=2) and 1.8 s without (n=1). This simulator's Settings has no
   Wallpaper row either way (16-17 rows in its root list in all 13 checks). What changes:
   PosterBoard does not refresh the gallery's wallpaper suggestions at boot, so the gallery
   shows those it fetched when it was built. Not checked with the setting: adding a new
   wallpaper, the contact poster editor and contact posters in an app's
   `CNContactViewController`, appearance or locale changes, an iOS build change, anything later
   than about 200 s after boot, and the iPhone SE and iPad beyond round 5's merge e2e (the mode
   was set on both and their next boots ran only the lock screen's provider; the e2e does not
   look at the lock screen or the gallery).
4. **Background work started after boot is stopped once it is idle** (`internal/ios/background.go`).
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
   1 s later by SpringBoard's emulated home press, a race of the home key fixed since, see "The
   home button"). In 2 boots its
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
5. **Daemons that stay idle after boot are ended, as a phone's jetsam would end them**
   (`internal/ios/idle.go`). Most of iOS's on-demand daemons tell launchd they may be ended when
   idle (`EnablePressuredExit` in their job), and the kernel's dirty tracking says when they are:
   "tracked, allows idle exit, clean" (no open transaction, not being launched). The simulator's
   processes are Mac processes, so agemu reads that state on the host (`proc_info`, as libproc's
   `proc_get_dirty` does), and their CPU time and start time too (`proc_pid_rusage`). A phone ends
   such clean daemons first when it needs memory (`JETSAM_REASON_MEMORY_IDLE_EXIT`), and launchd
   starts them again at the next request; launchd_sim does the same only when the Mac runs short of
   memory (the idle exits below). After a cold boot iOS starts about 60 of them for one-off work,
   and they then sit idle at 6-24 MB each. agemu looks every 30 s from 1 to 5 minutes after the
   device is ready. A daemon of the simulator runtime is ended when 3 passes in a row (30 s apart) found
   it idle-exitable and its CPU time, read in the kernel's own units, did not change at all between
   them: serving an app's call costs CPU, so a daemon that an app calls more often than once a
   minute is never ended. An interval shorter than 15 s (a pass that ran late, followed at once by
   the next) does not count toward the 3, and the quiet observations must span at least 58 s from
   the first to the last. Only daemons that started before a cut-off may be ended at all; the
   cut-off is the earliest of the first agemu call of this boot that can start app work (`install`,
   `launch`, `open`, `push`, `clear_app_data`, which reinstalls the app), and the earliest app process a pass saw: an installed app's or its
   extension's, or the frontmost app's when that is not SpringBoard (a system app such as Safari or
   Settings, also one opened from its icon). So nothing an install, a launch, an opened URL or an
   app brought up, or that iOS started after them, is ended. (An app opened from its icon and
   closed again between two passes leaves no mark; the daemons it started can then be ended once
   idle.) Ending is SIGKILL, as jetsam's idle exit and launchd's pressured exit do, sent right
   after a recheck has found the same process (its start time) still idle-exitable and with no CPU
   used since the pass, milliseconds earlier. (An earlier version sent SIGTERM first: most of these
   daemons ignored it, and those then kept using a little CPU.) A pass does nothing, and the count
   starts again, while an agemu call acts on the device's apps (install, launch, open, push,
   uninstall, terminate, clear-data, reset, permissions, location) or the device is parked or being
   parked, by `agemu park` or by auto-park (both mark the device before they stop anything), and
   the first pass after a resume does nothing either (a daemon frozen for the park used no CPU and
   would otherwise count as idle for that interval: without this, a pass ended a daemon 0.25 s after
   an auto-park's resume). A pass
   ends its daemons holding the device's park lock: a park that starts meanwhile waits for the pass
   (a few milliseconds), and a pass that finds a park or a shutdown holding the lock ends nothing,
   so no daemon is signalled while its simulator is being parked. A daemon that is running again at the next pass, or that turned busy before the SIGKILL,
   is left alone for the rest of the boot; one that comes back later (device management's on-reboot
   task at +120..200 s starts about 20 of them again) may be ended again once it is idle, at most 3
   times per boot. The passes belong to the boot agemu made: when the simulator's launchd_sim
   changes (it was shut down and booted again outside agemu, by `simctl` or Xcode, within those 5
   minutes) or is gone, they stop. The daemon log lists what each pass ended. Which daemons: by
   default all idle daemons except the never-ended ones and the `appFacing` list in `idle.go`, the
   daemons found to serve a public API an app can call (Contacts, EventKit, HealthKit,
   UserNotifications, MapKit, privacy prompts, keychain and TLS trust, UIPasteboard, AlarmKit,
   ActivityKit, Focus status, WatchConnectivity and the Watch pairing stack, GameKit, App Intents
   and INInteraction donations, speech voices, CallKit, Now Playing, CloudKit and the iCloud
   key-value store, StoreKit and account daemons, CoreSpotlight, Photos, FinanceKit, file
   coordination, File Provider and iCloud Drive, WeatherKit, AdServices, ManagedSettings and
   FamilyControls, Verify with Wallet, Background Assets, Extensible SSO, GSS, AccessorySetupKit,
   App Clips, launch snapshots, WebKit's privacy daemon, VideoSubscriberAccount, managed app
   configuration, Live Caller ID Lookup, app installs). That leaves daemons such as maild,
   jetpackassetd, managedassetsd, appleaccountd, cdpd, budd, remotemanagementd (device management),
   sleepd, parsecd, peopled, systemstatusd, countryd, featureaccessd and localizationswitcherd.
   Some of the daemons that are ended may still serve a public API that was not identified; for the
   13 public APIs a probe app called (AdServices, SKAdNetwork, WeatherKit, ManagedSettings,
   FamilyControls, iCloud ubiquity, File Provider, Verify with Wallet, INInteraction, Sign in with
   Apple state, Focus status, CoreLocation, EventKit sources) the first call after their daemons
   had been ended at +90 s by an earlier, wider build was not slower. Ended app-facing daemons did
   cost time: an app's first call waits for launchd to start the daemon again, and Contacts and
   EventKit took about 2 s instead of 0.15 s, StoreKit, WatchConnectivity and the speech voice list
   about 0.9 s, MapKit directions up to 3 s (one probe run each way; the probe reads its result
   about every 0.7 s). `AGEMU_IOS_IDLE_EXIT=all` ends the `appFacing` daemons too, for the lowest
   memory when that first-call delay does not matter (with axassetsd gone, the 4 speech synthesis
   extensions, 38 MB, that its retries kept starting go too, and with callservicesd the FaceTime
   and CallKit helpers). Not touched in either mode: SpringBoard, backboardd, PosterBoard and its
   lock screen extension, passd, locationd, runningboardd, apsd, sharingd, apps and their
   extensions, WebKit's processes and the other processes that do not allow idle exit or are busy.
   `AGEMU_IOS_IDLE_KEEP=name,...` keeps more, `AGEMU_IOS_IDLE_EXIT=0` keeps them all running. Never
   ended in either mode: `diagnosticd` and `logd_helper` (agemu's log stream; they came back within
   seconds); `dasd` (it schedules background tasks, an app's `BGTaskScheduler` work among them:
   ended, it came back 6 minutes later and iOS's deferred boot work, device management's task among
   it, ran at +567 s instead of +131..197 s, after the background stop's window); `followupd`
   (Settings' follow-up rows: Settings started it again and its "Ready for Apple Intelligence" row
   came in above General after the list had drawn, so a tap on General landed on that row, 2 of 2
   flows; the same race failed 13 of 570 agent flows in an earlier density run without this stop: a
   tap by selector does not check that the element is still where it was read); `iconservicesagent`
   (ended, the home key from Settings took 0.3 s longer, 8 of 8 boots); and `gamecontrollerd`
   (started again at once, it then used 0.17-0.19 s of CPU a minute, with diagnosticd and
   backboardd 0.3 s more, in 4 of 4 boots; the original process is sometimes busy like that too,
   with or without the stop). See "Ending idle daemons" below for what was checked and measured.

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
  cannot be run from it;
- after the first boot, refreshed wallpaper suggestions: the wallpaper gallery keeps those of the
  boot that built it (PosterBoard's keynote mode, item 3 above).

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

**Stopping the background work** (item 4) against the same lean setting without it, on one new
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

**Keeping the gallery from starting** (item 3) against the build before it (items 1, 2 and 4
only), on one `iphone-17-pro` simulator after its first boots, 6 pairs of cold boots in
counterbalanced order (VB, BV, VB, BV, BV, VB), each arm in its steady state (the setting written
into the shut-down device's preferences file before its boot, standing for what the build had
written at an earlier boot; without it, deleted), a freshly started daemon for every boot
(2026-11-07, 12:18-13:04; host load 7.5-23.8 at the start of a boot, 6.5-11.0 GB of swap in use;
medians, range in brackets; the same meter, its first-minute window starting when `agemu boot`
returned). That the build sets the key itself is shown by the first boots of new devices
(above). A boot of the round-4 build with the key written by hand took 38.9 CPU-s, so the effect
is the key's:

| | before (n=6) | gallery kept from starting (n=6) |
|---|---|---|
| simulator CPU in the first minute after agent-ready | 67.9 s [58.5-74.8] | **41.4 s** [32.4-47.6] (-39%) |
| of which processes that exited in the minute | 13.3 s [10.4-18.9] | 0.8 s [0.7-1.6] |
| simulator CPU in the second minute | 2.0 s [1.9-4.6] | 2.1 s [1.9-3.0] |
| memory 1 min after agent-ready | 1.99 GB [1.91-1.99] | **1.89 GB** [1.88-1.89] |
| memory 2 min after agent-ready | 1.86 GB [1.64-1.86] | **1.75 GB** [1.75-1.76] |
| processes 2 min after agent-ready | 118 [100-119] | 114 |
| wallpaper extension processes started in the first 200 s | 15 [12-15] | 1 (the lock screen's) |
| cold boot until agent-ready | 10.0 s [8.7-12.1] | 10.1 s [8.5-14.9] |

Every boot with it took less first-minute CPU than every boot without it; per pair -26.6 s
(median; -10.9 to -40.4 s, -18.7% to -55.5%). At 1 minute every boot with it was at least 22 MB
below every boot without it (per pair -103 MB, median). One boot without it had 100 processes and
1.64 GB at 2 minutes (the others 118-119 and 1.84-1.86 GB): launchd_sim idle exits
(`JETSAM_REASON_MEMORY_IDLE_EXIT`, 22 log lines) at +75..130 s of that boot. Two boots with it had
such exits too, at +134..234 s, after their measured 2 minutes; none fell into a first minute.

The first boot of a new device is not changed: PosterBoard builds the gallery in it, and the
setting comes only at its end (new devices, first boot: 165.1 and 146.1 s of first-minute CPU
before, 161.6 s with it; 2.42-2.49 GB at 2 minutes in all three). The second boot of the device
whose first boot set it took 42.4 s and started only the lock screen's provider.

**Ending idle daemons** (item 5) against the same lean setting without it
(`AGEMU_IOS_IDLE_EXIT=0`), on one `iphone-17-pro` simulator after its first boot (build bc11fc1;
the shipped one differs in when it may end a daemon after an app was installed, launched or opened,
and in sending SIGKILL without SIGTERM first, and ended the same 15 daemons in 2 boots without an
app), 4 pairs of cold boots in ABBA order with a freshly started daemon for each
(2026-11-08, 22:03-22:42; host load 3.1-4.2 at the start of a boot, 5.2 GB of swap in use;
medians, range in brackets; the experiments' meter); AgemuTestApp launched at +300 s, then a timed
agent flow (Settings: General, About, back; home; AgemuTestApp: two taps, typing, a screenshot and
a UI tree) at +315 s:

| | without (n=4) | idle daemons ended (n=4) |
|---|---|---|
| memory 2 min after agent-ready | 1.74 GB [1.73-1.74] | 1.69 GB [1.69-1.70] |
| memory 4 min after agent-ready | 1.76 GB [1.76-1.77] | **1.62 GB** [1.61-1.62] (-8%) |
| processes 4 min after agent-ready | 117.5 [117-118] | 103 |
| memory, AgemuTestApp open (+315 s) | 1.84 GB [1.84-1.85] | **1.69 GB** [1.69-1.70] |
| simulator CPU in the first minute after agent-ready | 35.7 s [32.7-39.9] | 33.5 s [28.3-37.2] |
| simulator CPU +240..300 s (% of a core) | 2.8 [2.7-2.8] | 2.8 [2.3-3.3] |
| agent flows that passed | 4 of 4 | 4 of 4 |
| cold boot until agent-ready | 7.2 s [6.8-8.1] | 7.7 s [7.1-8.4] |

Per pair -149 MB [-150 to -144] at 4 minutes and -147 MB [-149 to -145] with the app open; every
boot with the stop was at least 140 MB below every boot without it. It ended the same 15 daemons
in each boot (appleaccountd, appprotectiond, budd, cdpd, containermanagerd_system, countryd,
featureaccessd, ind, localizationswitcherd, managedassetsd, parsecd, peopled, remotemanagementd,
sleepd, systemstatusd), at +120..210 s. The first minute is not changed by design (the first end
is at +120 s), nor the boot until agent-ready, which the stop does not reach (per pair +0.2 s
[-0.1 to +1.2], the ranges overlap). The agent flow took 10.5 s [10.3-10.9] against 11.0 s
[9.4-11.6]; no difference is claimed.

The build an independent re-check measured first (bb2bb8c: no CPU rule beyond < 20 ms between two
passes, a shorter keep list) took 1.64 GB [1.64-1.64] against 2.01 GB [2.00-2.04] on its own
device (4 pairs), -0.37 GB. It also ended daemons an app was using: with a probe app calling 16
public APIs every 20 s, the pass after its launch ended `fileproviderd` and `coreidvd` in 4 of 4
boots, and the next File Provider and Verify with Wallet calls took 236-749 ms instead of 0-10 ms;
it ended daemons behind public APIs too (WeatherKit, AdServices, ManagedSettings, ...). The
present rules come from that: the CPU must not change at all over 60 s, nothing that started
after an app is ended, and those daemons are kept by name. That costs about 0.15 GB of the
saving: about 110 MB for the daemons now kept by name and about 33 MB for those that the stricter
test finds busy (maild, jetpackassetd, geoanalyticsd, biomesyncd; sizes at 4 minutes without the
stop).

With the same probe app (`.lab` copy of the re-check's SkepProbe, its call interval made
configurable), 5 cold boots of the shipped build: the app installed and launched at +245 s with
calls every 20 s; at +100 s with calls every 20 s and every 50 s; and with
`AGEMU_IOS_IDLE_EXIT=all` at +40 s with calls every 20 s and every 50 s. No call was slower than
150 ms where that call's median was under 20 ms (0 such calls of 16 kinds in all 5 boots; File
Provider 0-2 ms, Verify with Wallet 1-17 ms, ManagedSettings 1-12 ms, AdServices 0 ms), and
nothing was ended after the app started in the default runs. In the two `all` runs, with the app
calling from +40 s, the passes still ended about 45 daemons per boot (weatherd, FamilyControlsAgent,
dataaccessd, bird, biomed, healthd, contactsd, ...): their CPU had not changed, so they were not
serving the app's calls, and no call got slower; `fileproviderd` and `coreidvd`, which the app
used, were not ended.

Also with the shipped build: a 15-minute soak without interaction took 1.66 GB at 4 and 5 minutes
and 1.69 GB at 10 and 15 (one boot; the soak without the stop ran on another simulator of the same
type earlier, 1.77-1.79 GB, so this is no comparison); no process but `xpcproxy_sim` started more
than once, no crash report came from the simulator, and the same 15 daemons were ended. The
end-to-end run on this device (`E2E_IOS_DEVICE`, profile switches skipped with
`E2E_IOS_PROFILES=0`), started after the passes: 85 of 85. The probe app (`testapps/ios/LeanProbe`,
40 checks plus work started 15 s after agent-ready and checked at +330 s): a local notification
due at +165 s was delivered, an AlarmKit timer due at +195 s alerted, a Live Activity stayed active
and the pasteboard kept its text across the passes; HealthKit authorization and a step-count write
and read, the document picker (open and save), "Save to Files" from the share sheet,
`PHPickerViewController`, data detectors, CoreSpotlight, `WKWebView`, MapKit, ActivityKit,
AlarmKit, Focus status, notifications, Contacts, EventKit, speech, typing, PassKit, StoreKit and
`WCSession` worked. Four sheet steps of the probe's own driver (coordinate taps into system sheets)
missed in one run at host load 21-24 (sheet not up yet, a notification banner over a button, the
picker reopened in a folder) and passed when run again on a new boot.

On a new iPhone SE (bb2bb8c, one boot each way): 1.47 GB against 1.74 GB 4 minutes after
agent-ready. The iPad mini was not checked.

A second re-check found daemons ended under an app in bc11fc1: two that `agemu install` started
before the app's process existed (privacyaccountingd, mapssyncd, ended at +150/+210 s in 2 of 2
boots), and three that Safari started after `agemu open` (com.apple.Safari.History,
contentlinkingd, nanosystemsettingsd; Safari.History was running again about 1 s after its end).
Hence the cut-off at the first install, launch, open or push, and the frontmost app counting as an
app. The shipped build, 2 cold boots each way, with the re-check's watcher (every 2 s: each
process's start time and CPU): with the probe app installed at +70 s and launched (calls every
75 s), 41 and 38 daemons started after the install and none was ended (mapssyncd, started at
+75 s, ran to the end of the watch on one pid); with Safari opened at +65 s and Settings launched
at +130 s, 102 and 55 daemons started after the open and none was ended (com.apple.Safari.History,
contentlinkingd, nanosystemsettingsd and medialibraryd each ran to the end on one pid). The 13
daemons ended in each of those boots had all started before the boot was ready. The end-to-end
run passed 85 of 85 on the shipped build from a cold boot and again after the passes.

With `AGEMU_IOS_IDLE_EXIT=all`, measured only on an earlier build (which also ended
`iconservicesagent`, `gamecontrollerd`, `nanoregistrylaunchd` and `brookcompaniond`; not
re-measured): 0.99 GB [0.95-1.08] against 1.76 GB [1.76-1.77] 4 minutes after agent-ready, 8 pairs,
and an app's first call to an ended app-facing daemon was slower (Contacts and EventKit about 2 s
against 0.15 s). Earlier builds of this round also showed why the never-ended daemons are kept
(item 5): with `followupd` ended, 2 of 2 agent flows tapped "Apple Intelligence & Siri" instead of
General; with `iconservicesagent` ended, the home key from Settings took 1.48 s [1.44-1.57]
against 1.19 s [1.15-1.25] (8 boots each), 1.15 and 1.16 s in 2 boots with it kept; with
`nanoregistrylaunchd` and `brookcompaniond` ended, `pairedsyncd` started and exited 4 times
between +600 and +900 s (about 14 s of CPU). `gamecontrollerd`, kept, was sometimes busy (0.17-0.2
s of CPU a minute, logging about 40 lines a minute) on builds with and without the stop alike (1 of
4 boots each in the re-check).

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
  at 0.05 in 2 of 6 and at 0.1 in 7 of 8; 100 ms or more later, none was lost (0 of 26). (Round
  4 found the cause and fixed it for taps by selector: see "Taps on just-presented UI".)
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

The final run below and `bench/final/ios.md` (both early on 2026-11-06) measured the first
version's tap and type: a tap returned once its touch was sent, and `type hello` sent HID key
events. Later that day a tap began to wait until its first frame shows (at most 120 ms) and `type`
to paste and wait until the app has drawn the text (see "How each operation works"); those rows
were not run again against Maestro. Measured since: a whole `agemu type <word>` 118 ms (CLI
median, see "Test capabilities"), and a tap by selector on a label that draws nothing 159-160 ms,
most of it that 120 ms wait (see "Lost taps on just-presented UI").

### Paste confirmation (round 5, 2026-11-07)

`agemu type <6 random letters>` through the CLI on agemu-iphone-se-3 (iOS 26.5), each call right
after a tap on the field, against the round-4 build (r4). Ground truth: a probe app (UIKit text
field, SwiftUI `SecureField`, UIKit secure text field, `<input>` in a `WKWebView`) writes every
change of its fields to a file in its data container, read once it had not changed for 0.6 s
(r4 and v2 sessions) or 1.2 s after the tap and then once quiet for 0.6 s (the final build's
sessions); Settings' search field is read through the UI tree 1.2 s after the call (it had no
slowdown).
A double paste is the text twice (UIKit's smart insert puts a space between). The probe makes
itself slow on purpose: `block` holds its main thread 100-800 ms, starting 0-300 ms after the
field got focus; `slow paste` inserts a paste it has read 100-800 ms later (`UITextPasteDelegate`)
with the main thread free; `refuse` makes the field refuse pastes for 100-400 ms after it got
focus (UIKit then drops Cmd-V). r4 and v2 alternated in blocks (40 r4, 100 v2, 40 r4, 100 v2
per scenario); the final build (100 per scenario, which adds the repeat 2 s after an unshown read
and drops a redundant check) ran afterwards, with no r4 arm and the longer harness wait, so only
v2 is a matched comparison. r4's failures are structural (it never repeats into an unreadable
field, and a refused paste never arrives late), so the shorter wait of its runs can only have
undercounted its doubles. Host load average 2.6-126.4, other sessions' work included.

| scenario | r4: wrong | new: wrong (v2 / final) | median call, r4 / v2 / final |
|---|---|---|---|
| UIKit field, block | 0 of 80 | 0 of 200 / 0 of 100 | 424 / 454 / 424 ms |
| UIKit field, slow paste | 53 of 80 (double) | 1 of 200 (lost) / 0 of 100 | 578 / 627 / 665 ms |
| UIKit field, refuse | 14 of 80 (lost) | 0 of 200 / 0 of 100 | 475 / 473 / 479 ms |
| SwiftUI `SecureField`, block | 0 of 80 | 1 of 200 (lost) / 0 of 100 | 175 / 188 / 196 ms |
| UIKit secure field, refuse | 80 of 80 (lost) | 0 of 200 / 0 of 100 | 174 / 466 / 465 ms |
| `WKWebView` input, block | 0 of 80 | 0 of 200 / 0 of 100 | 522 / 876 / 887 ms |
| Settings' search field | 0 of 80 | 0 of 200 / 0 of 100 | 1170 / 1164 / 1159 ms |

The v2 slow-paste failure is the dropped read described in "Confirming a paste" (the final build
pastes again in that case). The v2 `SecureField` failure was read by the app 4 ms after Cmd-V
and was most likely recorded while the probe was still blocked (that run read the file once it
had been quiet for 0.6 s, shorter than a block; not proven); the final build's runs wait 1.2 s
after the tap. Three trials right after a
relaunch, whose tap did not focus the web view's input (no focus event), were not counted: no
app read their pastes and the new build pasted three times into nothing. Medians in the slow
scenarios are not comparable between builds (different loads; r4 returned before the paste was
confirmed in the secure and web cases, and r4's lost pastes returned early).

On a responsive probe (no slowdown), builds alternated in blocks of 25 (n=50 per build and
scenario, every call correct): median call time new minus r4, bootstrap 95% interval:

| field | v2 (load 11.4-40.0) | final (load 2.6-6.6) |
|---|---|---|
| UIKit text field | -9.2 ms (-21.7..-0.6) | +0.5 ms (-10.0..+12.7) |
| SwiftUI `SecureField` | -4.3 ms (-15.5..+3.8) | +0.7 ms (-7.3..+7.9) |
| `WKWebView` input | +48.9 ms (+42.6..+59.0) | +87.3 ms (+81.1..+91.7) |
| Settings' search field | -7.0 ms (-19.6..+4.5) | not run |

### Lost taps on just-presented UI (round 4, 2026-11-07)

TapLab (a probe app) on its own iPhone 17 Pro simulator, iOS 26.5, lean. Each try is what an
agent does: `tap --id <opener> --wait-text <the new UI's text>`, then at once `tap --text
<button>` (the call measured), then `wait` up to 3 s for the button's effect in the UI tree. A
try counts as lost when the effect did not show; a lost tap was then tapped again (not counted).
The two daemons (before: the round-3 build; now: this one) drove the same simulator, alternating
try by try (ABBA), 50 tries each. Load average 10-32. The permission row resets the microphone
grant and relaunches the app before every try.

| just presented | lost before | lost now | tap call before, landed taps only (median, range) | tap call now |
|---|---|---|---|---|
| SwiftUI alert, OK | 38 / 50 | **0 / 50** | 495 ms (455-507), n=12 | 532 ms (496-1753) |
| UIKit alert, Done | 34 / 50 | **0 / 50** | 490 ms (332-505), n=16 | 522 ms (489-545) |
| UIKit action sheet, an action | 49 / 50 | **0 / 50** | 202 ms, n=1 | 507 ms (484-531) |
| SwiftUI sheet, its close button | 39 / 50 | **0 / 50** | 605 ms (563-641), n=11 | 606 ms (498-639) |
| system permission alert (SpringBoard), Allow | 26 / 50 | **0 / 50** | 512 ms (45-552), n=24 | 524 ms (398-596) |

Before, every lost tap on the two alerts and the permission alert came from a call of 15-36 ms:
the tap went out before the presentation had drawn. The sheet lost 24 taps that way (13-34 ms)
and 15 after waiting 486-495 ms (the settle ended in its tail); the action sheet lost all 49
after 48-95 ms (the settle ended after the dimming, while the sheet slid in). A lost tap costs an
agent its wait timeout (10 s by default).

The "before" column counts only the taps that landed. Against all of the round-3 build's tap
calls (most returned in 15-95 ms and were lost), the wait adds about 420-510 ms per tap (medians:
SwiftUI alert 21 -> 532 ms, UIKit alert 25 -> 522, action sheet 87 -> 507, permission alert
36 -> 524). Against the landed ones it adds 1-37 ms, except the action sheet: 507 ms against its
single landed tap's 202 ms (+305 ms), because its wait ends on the 300 ms tail (see above).

With `AGEMU_IOS_ANIMATIONS=0` in both daemons (same method, load 14-113), lost before / now: the
SwiftUI alert 10 / **0** of 50, the UIKit alert 21 / **0**, the action sheet 0 / 0, the sheet
(whose speed the setting does not change) 35 / **0**, the permission alert 17 / **0** with
SpringBoard still at normal speed (agemu applies the setting after a boot; SpringBoard reads it at
its next start) and 3 / **0** after a second boot with SpringBoard fast too. The tap calls took
-3 to +20 ms more than the landed ones before (medians: 164 against 159, 149 against 129, 139
against 137, 598 against 597, 521 against 510, 132 against 135 ms).

The home button, `launch com.apple.Preferences` right after the call (30 tries per row, ABBA,
load 13-64); lost = Settings not in front 2 s later:

| | lost before | lost now | the call before | the call now |
|---|---|---|---|---|
| `key home` with an app in front | 15 / 15 | **0 / 15** | 9 ms (6-19) | 1120 ms (1095-1301) |
| `key home` on the home screen | 15 / 15 | **0 / 15** | 10 ms (5-23) | 664 ms (661-709) |
| `reset` with the home screen in front (the e2e's case) | 29 / 30 | **0 / 30** | 161 ms (137-809) | 160 ms (129-455) |
| `reset` with Settings in front | 0 / 30 | 0 / 30 | 1747 ms (1685-2073) | 1734 ms (1681-1903) |

`key home` now takes as long as SpringBoard needs to act on the press. Reset is not slower. With
`AGEMU_IOS_ANIMATIONS=0` (load 10-51) the same rows lost 15, 15, 30 and 1 before and 0, 0, 0 and
0 now.

Where nothing is presented, a tap by selector costs the same (30 per row, ABBA, load 14-24; a
tap on a label that does nothing, 1 s after the screen was read): an element read before the
last input 159 ms (36-247) before, 160 ms (47-312) now; a second row, meant as an element new
since a relaunch of the app but long drawn, 153 ms (44-205) before, 155 ms (46-206) now. That
second row did not test what it meant to: terminate and launch read no UI tree, so the read
before the launch already held the label at the same place, and the tap took the path for an
element the agent had seen. So both rows measure that path; the cost of the wait for a new
element on a still screen rests on the code (step 1 ends at once when the screen has changed
since the input, step 2 when the column has been still for 48 ms), not on a measurement. Most
of each call is the tap's wait of up to 120 ms for a frame that shows it, which a label never
draws.

Merged build smoke (agemu-iphone-17-pro, the same TapLab steps through the CLI, 3 tries per
kind, load 51-69): 15 of 15 taps landed (tap calls 478-617 ms); `key home` with TapLab in front
took 1095-1131 ms and Settings launched right after was in front 2 s later 3 of 3 times.

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

Round 5's merge (2026-11-07, the three shared simulators): `scripts/e2e-ios.sh` (138 checks)
passed 138/138 (load average 4.9 at the start, 71 at the end). In those boots the daemon set
PosterBoard's keynote mode on all three. Run again after they were shut down, so that they
booted with it: 137/138 (load 34-39); the one failure was the SE's tap on General landing on the
"Apple Intelligence" row Settings inserts after the tree read (see "Several devices at once"),
and each simulator then ran only the lock screen's poster extension (`MercuryPosterExtension`).
The park check stopped 146 and 138 processes in 77 and 75 ms and was agent-ready 39 and 35 ms
after the resume. No paste was repeated in either run.

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
- These runs, and the earlier series below, used agemu as of the agent-loop work merged on
  2026-11-06 (pull request #3), before the lean simulators and the later waits for just-presented
  UI, the home button and paste confirmation (sections above); they were not repeated since.

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
  UI-tree read that has the next screen. Argent is closest. It is faster than the compound variant
  on three steps (than plain agemu on the first and the last):
  - entering the e-mail (`fill` 790 ms vs a verified paste, 960 ms);
  - search -> results (1996 against 2169 ms; plain agemu 1944 ms);
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
  touches become unreliable (`dtuhidd` replaces it). SimulatorKit is loaded from
  `Contents/Developer/Library/PrivateFrameworks` (Xcode up to 26) or `Contents/SharedFrameworks`
  (Xcode 27). `agemu-ios-helper` reports the framework versions and the SimulatorKit path it
  loaded (`hello`); `agemu-ios-helper --check` loads the frameworks and exits, and `agemu doctor`
  runs it.
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

The round-4 e2e's one flake, a double paste in Settings' search field at load average 203
(`Wallpaper Wallpaper`), came from the fixed 150 ms paste window; a paste is now repeated only
when the pasteboard daemon's log shows no app read it (see "Confirming a paste"). The daemon log
says "pasting again: ..." with the reason when it repeats one. In the lane a step like the e2e's
Settings search (tap the field, type, read the field) was run 200 times on v2 and 100 times on
the final build on the SE simulator, all correct (see "Paste confirmation" under Benchmarks);
round 5's merge then ran the full e2e (see "End-to-end run").

## Credits

The private-API usage (Indigo message layout, framebuffer callbacks, the AXPTranslator token
delegate) follows the public reverse-engineering in
[facebook/idb](https://github.com/facebook/idb)'s FBSimulatorControl (MIT license, Copyright (c)
Meta Platforms, Inc. and affiliates). agemu's code is its own; where a byte layout comes from
idb's `Indigo.h` / `SimulatorIndigoHID.swift`, the source says so.
