# Test capabilities

Beyond driving the UI, agents testing real apps need to set up the device: open a deep link,
grant a permission, fake a location, read what the app copied, switch to dark mode, start an
app from a clean state, deliver a push. This page defines that surface: the commands, what each
does on each platform, how far it matches a real device, and what `reset` restores.

Every capability is a call to the daemon, like the rest of agemu. The daemon checks the
arguments, takes the device lock and calls an optional backend interface
(`internal/backend/backend.go`). A platform without a faithful path answers **not
implemented**, and does not fake one.

## Commands

| Capability | CLI | MCP tool | Result |
|---|---|---|---|
| Open a URL / deep link | `open <url>` + `--wait-*`, `--ui` | `open_url {url, waitFor…, returnUi}` | `{"ok":true,"app":"<handler>"}` (`app` when known) |
| Permissions | `permission grant\|revoke <app> <name>...`<br>`permission reset <app> [<name>...]` | `permission {app, action, permissions[]}` | `{"ok":true}` |
| Location | `location set <lat> <lon>`, `location clear` | `location {action, latitude, longitude}` | `{"ok":true}` |
| Clipboard | `clipboard [get]`, `clipboard set <text>` | `clipboard {action, text}` | get: `{"text":"..."}` (the CLI prints the text) |
| Appearance | `appearance [light\|dark]` | `appearance {mode}` | `{"appearance":"light\|dark"}` |
| List user apps | `apps` | `apps {}` | `[{"id","name","version"}]` |
| Uninstall | `uninstall <app>` | `uninstall {app}` | `{"ok":true}` |
| Clear app data | `clear-data <app>` | `clear_app_data {app}` | `{"ok":true}` |
| Push notification | `push <app> <json\|file.json\|->` + `--wait-*`, `--ui` | `push {app, payload, waitFor…, returnUi}` | `{"ok":true}` |

All take `-d <device>` (MCP: `device`), optional when exactly one device is booted. The daemon
method has the MCP tool's name.

Defaults the agent can rely on:
- `permission`: names may be repeated or comma-separated, and are case-insensitive. `reset`
  without names resets all of the app's permissions.
- `location`: action `set` is implied by coordinates. Latitude is -90..90, longitude
  -180..180, in WGS84 degrees.
- `clipboard`: action `get` by default, `set` when text is given. `set` with empty text
  empties the clipboard.
- `appearance`: without `mode` it reads the current appearance.
- `push`: the payload is a JSON object with an `aps` object, at most 4096 bytes of compact JSON.
  The CLI takes it inline (`'{"aps":{...}}'`), from a file, or from stdin (`-`). MCP takes an
  object, or a string holding one.
- `open` and `push` take the action wait options, like `tap` and `launch`:
  `agemu open myapp://orders/7 --wait-id order-7 --ui`,
  `agemu push com.app '{"aps":{"alert":"Hi"}}' --wait-contains Hi`.

## Permission names

Names are platform-neutral. A name with a `.` is a raw platform permission
(`android.permission.READ_SMS`): Android passes it through, iOS refuses it.

| Name | Android (runtime permissions the app requests) | iOS privacy service | iOS Info.plist key a grant needs |
|---|---|---|---|
| `camera` | `CAMERA` | `camera` (`kTCCServiceCamera`; not in `simctl privacy`'s list, but it works) | `NSCameraUsageDescription` |
| `microphone` | `RECORD_AUDIO` | `microphone` | `NSMicrophoneUsageDescription` |
| `photos` | `READ_MEDIA_IMAGES`, `READ_MEDIA_VIDEO` (revoke/reset also `READ_MEDIA_VISUAL_USER_SELECTED`); `READ_EXTERNAL_STORAGE` for apps targeting API < 33 | `photos`: revoke and reset; grant not implemented (Photos ignores the Simulator's grant on iOS 26.5) | `NSPhotoLibraryUsageDescription` |
| `photos-add` | not implemented: apps add to MediaStore without a permission | `photos-add` | `NSPhotoLibraryAddUsageDescription` |
| `location` | `ACCESS_FINE_LOCATION`, `ACCESS_COARSE_LOCATION`; a grant also takes `ACCESS_BACKGROUND_LOCATION` away | `location` (while in use) | `NSLocationWhenInUseUsageDescription` |
| `location-always` | the above plus `ACCESS_BACKGROUND_LOCATION`, granted after the foreground pair | `location-always` | `NSLocationWhenInUseUsageDescription` and `NSLocationAlwaysAndWhenInUseUsageDescription` |
| `contacts` | `READ_CONTACTS`, `WRITE_CONTACTS` | `contacts` | `NSContactsUsageDescription` |
| `contacts-limited` | not implemented: no limited contacts access | not implemented: the Simulator's service is basic contact info, which the app reads as full access | - |
| `calendar` | `READ_CALENDAR`, `WRITE_CALENDAR` | `calendar` | `NSCalendarsFullAccessUsageDescription` (or the pre-iOS 17 `NSCalendarsUsageDescription`) |
| `reminders` | not implemented: no system reminders store | `reminders` | `NSRemindersFullAccessUsageDescription` (or `NSRemindersUsageDescription`) |
| `notifications` | `POST_NOTIFICATIONS` | not implemented: no privacy service; answer the app's system alert | - |
| `motion` | `ACTIVITY_RECOGNITION` | `motion` | `NSMotionUsageDescription` |
| `media-library` | `READ_MEDIA_AUDIO` (`READ_EXTERNAL_STORAGE` below API 33) | `media-library` | `NSAppleMusicUsageDescription` |
| `siri` | not implemented | `siri` (mapped, but not read back in an app: that needs the Siri entitlement) | `NSSiriUsageDescription` |
| `android.permission.*` | as given | not implemented | - |

The states (`api.PermissionAction`):
- **grant**: the app has access, as after the user tapped Allow. Android also sets the
  user-set flag, as the dialog does.
- **revoke**: denied, and the app cannot ask again, as on iOS after "Don't Allow" and on Android
  after a second denial (denied and user-fixed). To give back only while-in-use location, grant
  `location`; revoking either location name removes all location access. When one call names
  both location names, the later one wins (`grant location-always,location` ends at while in
  use).
- **reset**: not determined, so the app shows the system dialog on its next request.

A grant the app could not get on a device is refused with an error:
- Android: the app's manifest does not request any of the mapped permissions.
- iOS: the Info.plist usage description is missing; on a device the app would crash when it asks.

For a name that maps to several Android permissions, the backend acts on those the app requests.

## Reset contract

`reset` returns the device to its baseline, and that includes everything these capabilities
change. After `reset`:

| State | After reset | Android (baseline snapshot load) | iOS (soft reset) |
|---|---|---|---|
| appearance | light | restored by the snapshot (verified) | reset sets light (verified) |
| location | no simulated location (iOS); the emulator's default fix (Android) | the fix is emulator state that a snapshot does not hold: reset sends the default fix before the load and checks it after (verified with the probe's GPS listener) | reset clears the simulated location (verified: a relaunched app gets no fix) |
| clipboard | empty | restored by the snapshot; the emulator does not push its own copy back (verified, also 5 s later) | emptied (verified, as agemu reads it back) |
| permissions | not determined, for every app | restored by the snapshot (verified for a system app's permission) | each remaining grant of a covered service is reset, found in the device's `TCC.db` and locationd's clients (verified for user and built-in apps, and Maps location); `simctl privacy reset all` without a bundle id, used before, changed nothing on iOS 26.5 |
| apps installed since the baseline | gone, with their data and delivered notifications | restored by the snapshot (verified) | uninstalled, which also removes their grants (verified; delivered notifications not checked) |

The rule for new capabilities: whatever one can change, `reset` restores. Otherwise the change
is listed under "Survives reset" below.

Survives reset:
- iOS: data of built-in apps and added media (unchanged, see [IOS.md](IOS.md)), and a
  notification permission a built-in app got from its system alert.
- iOS: reset does not tell a baseline grant from a later one in a covered service. A new iPhone
  17 Pro on iOS 26.5 has none, so nothing of its baseline is lost; other device types and
  runtimes were not checked.
- Android: nothing found.

## Errors

The daemon checks a call in this order: the device, then the arguments, then the capability,
then the backend.

- **Bad arguments** fail at once, without waiting for the device lock. The message names the
  argument and the accepted values: `unknown permission "camra": use camera, microphone, ...`.
  The CLI exits 1, and MCP returns `isError`.
- **Not implemented**: `<capability> is not implemented on <platform>[: reason]`, for example
  `push is not implemented on android: no faithful path (...)`. A backend can also say it about
  one case: `permission "siri" is not implemented on android`.
  - The CLI exits **3**, so a script can skip the step on that platform.
  - MCP returns `isError` with the message.
  - The daemon response carries `"code":"not_implemented"`.
  - In Go, `errors.Is(err, api.ErrNotImplemented)` holds on both sides of the socket.
- **Backend failures** carry the platform's reason: no app handles the URL, the app is not
  installed, a system app cannot be uninstalled, and so on. The CLI exits 1.
- **Compound calls** (`open`, `push` with `--wait-*`) fail like other actions:
  `open_url done, but: timeout after 10s waiting for ...`.

## Fidelity

What each capability does differently from a person using a real device:

- **open_url**
  - Android sends `ACTION_VIEW` with `CATEGORY_BROWSABLE`, as a browser does for a tapped link.
    An activity whose intent filter lacks `BROWSABLE` does not open, as with a real link;
    `adb shell am start -d` would open it and hide that bug.
  - The caller is still the shell, so `getReferrer()` differs from a browser's.
  - On iOS the source application differs. iOS may first ask "Open in <app>?", as for a link in
    Safari (seen for a single open of a just-installed app, and for two quick opens of one URL);
    agemu does not answer it.
  - App Links and universal links open the app only when their domain is verified, as on a
    device; otherwise the browser opens (not exercised).
  - Android returns when the target activity (or the app chooser) is displayed. iOS returns
    once the system has accepted the URL, so use `--wait-*` for the screen.
- **permission**
  - The system dialog never shows, so its text and the app's handling of it are not tested.
    Test the dialog with `reset` and a tap.
  - Android kills the app's process when a permission is revoked or reset, as Settings does on
    a device. iOS may terminate the app.
  - Precise versus approximate location is not modelled: grants are precise.
- **location**: a fixed point, no route. Android goes through the emulator's GNSS (only
  `LocationManager` GPS was checked, not fused location). On iOS, CoreLocation sees a simulated
  fix; after `location clear` an app that was already updating usually keeps its last fix, as
  with `simctl location clear`. The call returns before the app has the fix: on Android the app
  sees it about 110 ms after the call starts, with `geo fix` too.
- **clipboard**
  - Plain text only.
  - The OS's paste prompt (iOS) and clipboard toast (Android 13+) still show, as on a device.
  - The Mac's clipboard does not reach the device (iOS: the helper's pasteboard is private;
    Android: checked from the Mac to the guest).
  - agemu's own iOS typing pastes through the clipboard and puts the old content back once the
    focused field shows the paste. When that cannot be confirmed (a secure field, a web view, a
    second paste), the typed text stays on the clipboard, so the app never pastes the old content.
- **appearance**: a real configuration change (Android `uimode`; iOS interface style). Apps
  redraw as on a device. Android recreates activities that do not handle the change, and waits
  for the new window; an activity that handles `uiMode` itself redraws in place, and agemu does
  not wait for that redraw. iOS waits for its ~0.5 s crossfade, so a screenshot right after the
  call shows the new appearance, while the app's UI tree has it before the call returns.
- **clear_app_data**
  - Android runs `pm clear`, the Settings "Clear storage" action. It also resets the app's
    permissions and stops the app.
  - iOS has no such action. agemu gives the state a delete and reinstall would leave: data,
    preferences, app-group data and permissions gone, keychain items kept (they survive a
    reinstall on a device too).
- **push**
  - iOS delivers through the Simulator's push path (`simctl push`): the app receives it as from
    APNs, and an alert shows only with notification permission. There is no APNs server,
    device token, priority or expiry. `content-available` pushes were not delivered (simctl
    the same).
  - Android has no faithful path: real FCM delivery needs the app's server credentials, and the
    shell may not send the `c2dm` broadcast. agemu answers not implemented rather than post a
    notification as the shell.

## Status

Android serves 8 of the 9 capabilities (push answers not implemented by design); iOS serves all
9, with four permission cases that answer not implemented. Latencies below are medians of the
daemon (a persistent socket, as `agemu mcp` uses it), the CLI (one `agemu` process per call)
and the standard tool, n=12 per arm, interleaved, on a host shared with other heavy sessions.

### Android

Implemented: open_url, permission, location, clipboard, appearance, apps, uninstall,
clear_app_data. Push stays not implemented by design. Mechanisms, measurements and caveats:
[ANDROID.md, Test capabilities](ANDROID.md#test-capabilities-2026-11-07).

| Capability | How | Latency, agemu daemon / CLI / standard tool (medians, ms) |
|---|---|---|
| open_url | agent: `ACTION_VIEW` + `CATEGORY_BROWSABLE` + `NEW_TASK`, `startActivityAndWait` | app in front: 3.7 / 9.3 / 34.7 (`am start -W -d`); app stopped: 261 / 216 / 222 (process start); app in the background: not measured |
| permission | agent: UiAutomation grant / revoke (the shell's, as `pm grant`), then the user-set / user-fixed flags; app-op permissions: `IAppOpsService.setMode` | grant 4.2 / 10.6 / 35.5 (`pm grant`); revoke 4.1 / 13.8 / 30.4; reset 2.6 / 8.6 / 31.8 |
| location | emulator gRPC `SetGps` (what `geo fix` does) | the call 0.6 / 8.3 / 16.3 (`adb emu geo fix`); the app's GPS listener sees the fix at the same time with either tool (113.9 against 111.7 ms after the call starts), so 1.0x as the app sees it |
| clipboard | agent: `IClipboard` set / get / clear as the shell | set 2.4 / 8.6, get 1.8 / 9.9; no standard tool (`cmd clipboard` has no shell command here; `adb shell true` takes 24-27) |
| appearance | agent: `IUiModeManager.setNightMode` (what `cmd uimode night` does), then waits until the focused app is recreated | home screen 27.2 / 49.2 / 67.4 (`cmd uimode night`); read 0.8 / 6.0 / 22.8. How much sooner an app in front shows it is not established (one build 1.7x, an earlier one 0.6x); right after agemu returns the app showed the new mode 10 of 10 times, after `cmd uimode night` 1 of 10 |
| apps | agent: installed non-system packages, label from the APK in the device's locale (cached per APK), `versionName` | 8.5 / 14.0 / 28.0 (`pm list packages -3`, ids only), with one user app installed |
| uninstall | agent: `deletePackageAsUser` for all users (`pm uninstall`); system apps refused | 61.7 / 53.0 / 119.1 (`adb uninstall`) |
| clear_app_data | agent: `clearApplicationUserData` (`pm clear`, Settings "Clear storage") | 35.6 / 44.0 / 79.3 (`pm clear`), app in front |

Numbers: n=12 per arm, interleaved, on a host at load average 23-110 (another session's
Gradle, Metro and simulators); ranges are in ANDROID.md. For uninstall (guest-side `cmd package
uninstall` about 78 ms) and a cold open the system's own work dominates. For clear-data and
setting the appearance that is not shown: the guest's `cmd package clear` (about 13 ms) was
timed with the app idle, not in front, and the night-mode change itself takes 11-16 ms of the
27.2 ms. Through the CLI, location (2.0x), apps (2.0x), revoke (2.2x) and a clipboard read
(2.5x the adb floor) are also below 3x: the CLI process start is most of their time.

Verified on the device:
- Reset contract: after `reset` the appearance is light, the emulator's GPS fix is back to its
  default, the clipboard is empty (also 5 s later: the emulator does not push its copy back), a
  system app's permission is back to its baseline flags, and user apps are gone (9 checks, 2
  rounds). The GPS fix is emulator state that a snapshot does not hold: reset and standby send
  the default fix (read from the emulator when agemu starts it) before the load and check it
  after.
- The Mac clipboard did not reach the guest. The other direction is reported checked, without
  a kept log. The emulator keeps a copy of the guest's clip (gRPC `GetClipboard` sees it),
  which agemu does not read.
- Permission names map per the table above, acting only on the permissions the app requests.
  Raw names may also be app-op permissions (special access such as `SYSTEM_ALERT_WINDOW`): grant
  sets the app-op to allowed, revoke to errored (the Settings switch), reset to default. A raw
  normal or signature permission is refused. A grant of a name the app does not request is
  refused; a revoke or reset of one is a no-op.
- Deviation from the table: `location` and `location-always` reset also reset background
  location, like revoke.

### iOS

All nine commands are implemented, as the CoreSimulator calls `simctl` makes, issued by the
long-lived helper; details, latencies and the probe-app checks are in
[IOS.md](IOS.md#test-capabilities). Daemon (MCP) medians against `xcrun simctl` (iPhone 17 Pro,
iOS 26.5, lean; load average about 18-21): open 6.7 against 120 ms (18x), permission 1.5-4.7
against 116-130 ms (28-78x), location 0.5-0.8 against 107-109 ms (>100x), clipboard set/get
0.8/11.7 against 133/161 ms (159x/14x), appearance read 1.0 against 175 ms (174x), push 15.8
against 133 ms (8x). `apps` is 0.7 against 173 ms, but agemu reads user apps' Info.plists on the
host while `simctl listapps` lists every app through CoreSimulator, so that is a different
mechanism, not only a saved process start. Uninstall (83 against 194 ms, 2.3x) and clear-data
(258 against 538 ms, 2.1x) are bound by installd. Setting the appearance waits for iOS's ~0.5 s
crossfade: a screenshot shows it as soon as with simctl (694 against 670 ms, 1.0x), but the call
takes 684 against 130 ms, and the app's trait reaches the UI tree 2.9x later than with simctl
(736 against 257 ms), whose screenshot at that time still shows the old appearance.

Deviations from the tables above:
- **permission**: `photos` grant, `contacts-limited`, `notifications` and raw names answer not
  implemented. A Simulator photos grant is not seen by Photos on iOS 26.5 (the app reads not
  determined); the Simulator's contacts-limited is basic contact info, which the app reads as
  full access; notifications have no privacy service (answer the system alert, which `ui`
  shows). `photos` revoke and reset work. `siri` is mapped but was not read back (that needs
  the Siri entitlement). `location-always` needs both location usage descriptions. `camera`
  works (`kTCCServiceCamera`), though it is not in `simctl privacy`'s list.
- **open_url**: an "Open in <app>?" prompt can show first (seen for a single open of a
  just-installed app on a new simulator, and for two quick opens of the same URL; not in 36
  benchmark opens). `app` is not reported.
- **location clear**: an app that starts updating gets `kCLErrorLocationUnknown`; an app that
  was already updating usually keeps its last fix (simctl the same).
- **push**: in the background the banner showed ~10.5 s after the push on a lean simulator
  (SpringBoard waits for a notification inference that times out), and it is not in the UI
  tree: tap it by position. `content-available` pushes were not delivered (simctl the same).
- **clear_app_data** is delete and reinstall from an APFS clone of the installed bundle: the
  end state of a device's delete + reinstall, including the notification permission (delivered
  notifications were not checked); the keychain is kept.
- **reset**: `simctl privacy reset all` (no bundle id), which reset ran before, changed nothing
  on iOS 26.5 (reported by the experimenter; no log of that check was kept). Reset now resets
  each remaining grant of a covered service (from the device's `TCC.db`, bundle clients only,
  and locationd's clients), sets light, clears the simulated location and empties the
  pasteboard. User apps' grants go with their uninstall. Checked: dark, a location, the
  clipboard, camera for the probe and location for Maps, reset, then light, empty clipboard (as
  agemu reads it), no user apps, camera not determined after reinstall, no location fix, no
  grant rows left; and grants to built-in apps (contacts for Maps, calendar, microphone and a
  camera revoke for Health) were reset while the baseline's other rows stayed.
- **type** depends on a private CoreSimulator class for the pasteboard
  (`SimPasteboardInterface`); without it, `type` falls back to `simctl pbcopy` and leaves the
  typed text on the pasteboard.
