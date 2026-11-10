# Test capabilities

Beyond driving the UI, agents testing real apps need to set up the device: open a deep link,
grant a permission, fake a location, read what the app copied, switch to dark mode, start an
app from a clean state, deliver a push. They also need to see what the app logged and why it
stopped ([Logs and crashes](#logs-and-crashes)). This page defines that surface: the commands,
what each does on each platform, how far it matches a real device, and what `reset` restores.

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
| logs and crashes | `logs` and `crashes` return only what happened after the reset; `since launch` and the crash notice forget earlier launches | the daemon starts both at the reset's end; the backend empties its ring buffer, and drops the baseline's lines the snapshot brings back into logd (to implement) | the daemon starts both at the reset's end, so older `.ips` reports on the Mac are not returned; the backend empties its rings and crash records and keeps the stream running (verified: no app line and no crash from before the reset) |

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
- **The app crashed**: any call on a device that fails while the app launched last through agemu
  is down after a crash (or right after one) says so after its own message, see
  [The crash notice](#the-crash-notice). The CLI exits **4**, MCP returns `isError`, the daemon
  response carries `"code":"app_crashed"` and the crash in `appCrash`, and in Go
  `errors.Is(err, api.ErrAppCrashed)` holds on both sides of the socket. Not implemented wins
  over it (exit 3).

## Fidelity

What each capability does differently from a person using a real device:

- **open_url**
  - Android sends `ACTION_VIEW` with `CATEGORY_BROWSABLE`, as a browser does for a tapped link.
    An activity whose intent filter lacks `BROWSABLE` does not open, as with a real link;
    `adb shell am start -d` would open it and hide that bug.
  - The caller is still the shell, so `getReferrer()` differs from a browser's.
  - On iOS the source application differs. iOS may first ask "Open in <app>?", as for a link in
    Safari (seen for a single open of a just-installed app, for two quick opens of one URL, and
    for every open of an Expo development build in front until one was answered); agemu answers
    Open, as the person who followed the link would.
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
    focused field shows the paste. When the field cannot show it (a secure field, a web view, a
    repeated paste), the typed text stays on the clipboard, so the app never pastes the old
    content.
- **type** (iOS) also uses the log capture below: a paste is repeated when the pasteboard daemon
  logged no app reading it, or once more when an app read it but a field that reads back as text
  had not changed 2 s later ([IOS.md](IOS.md#confirming-a-paste)). With no focused field a call
  now sends 3 Cmd-Vs (1.6-2.6 s).
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

## Logs and crashes

An agent whose tap "did nothing" needs to know whether the selector missed, the app logged an
error, or the app is gone. Two calls and one notice cover that:

| What | CLI | MCP tool | Result |
|---|---|---|---|
| Log lines | `logs [<app>] [--since T] [--level L] [--contains T] [--regex RE] [--limit N] [--max-bytes B]` | `logs {app, since, level, contains, regex, limit, maxBytes, json}` | text lines, or with `--json` / `json`: `{"entries":[...],"cursor":"c...","omitted":N,"partial":true,"capturedFrom":"..."}` |
| Crash reports | `crashes [<app>] [--since T] [--limit N]` | `crashes {app, since, limit, json}` | text, or `{"crashes":[...],"omitted":N}` |
| The crash notice | every command: `note: ...` on stderr; exit 4 when it fails while the app is down | every tool: a first `NOTE: ...` content line; `isError` with the reason | daemon response field `appCrash`; error code `app_crashed` |

Both calls take `-d <device>` (MCP: `device`) and an app (package name or bundle id; the CLI
also takes it as the first word: `agemu logs com.example.app`). Without an app they cover the
whole device. Unlike the capabilities above, `logs` and `crashes` do not take the device lock:
they read what agemu captured, so they never wait behind another agent's action.

### A log line

The same shape on both platforms; times are the Mac's wall clock (UTC in JSON):

```json
{"time":"2026-11-07T10:00:00.123Z","level":"error","pid":1234,"tid":1240,
 "process":"com.example.app","tag":"ReactNativeJS","message":"TypeError: undefined is not a function"}
```

```
10:00:00.123 E com.example.app[1234] ReactNativeJS: TypeError: undefined is not a function
```

- `process`: Android's process name (`com.example.app`, `com.example.app:remote`,
  `system_server`); iOS's executable (`AgemuTestApp`, `SpringBoard`).
- `tag`: Android's log tag; iOS `subsystem:category` (or the subsystem alone), else the image
  that logged the line (`Foundation` for `NSLog`).
- `message` may hold several lines (a stack trace logged in one call stays one entry; text
  output indents the continuation lines).

| level | Android | iOS (unified log type) |
|---|---|---|
| `verbose` | V | - |
| `debug` | D | debug: **not captured** (see Fidelity) |
| `info` | I | info, and default (`os_log` default, `Logger.notice`, `NSLog`) |
| `warn` | W | - (`Logger.warning` logs as error) |
| `error` | E | error |
| `fatal` | F, A (`Log.wtf`) | fault (`Logger.critical`) |

`--level` is the minimum and also takes the letters `v d i w e f`, `warning`, `err` and `fault`.

### Choosing lines

- **since**: a time (`2026-11-07T10:00:00Z`, any RFC 3339 offset), a duration back from now
  (`30s`, `5m`, `1h30m`), `launch` (the start of the app's last launch through agemu; without
  an app, of the device's last launch), or the `cursor` of an earlier result (`c` and a number),
  which returns only lines captured after it. Read new lines in a loop with the cursor:
  `agemu logs com.app --since c1730970000123456`. `crashes` takes the same forms except the
  cursor.
- **level**, **contains** (case-insensitive, in the tag or the message) and **regex** (RE2, at
  most 1000 bytes, matched against the tag or the message) all have to match.
- **Bounds**: at most `limit` lines (default 100, at most 10000) and about `maxBytes` bytes
  (default 16384, 1024 to 1048576; a line counts its message, tag and process plus 64). The
  result holds the **newest** lines that fit, oldest first; `omitted` counts the older matching
  lines left out. The newest line is always returned, its message cut to fit. One message is at
  most 16 KiB; a longer one ends in `...[N bytes cut]`.
- **partial** says that the window asked for reaches back before what agemu holds (the capture
  started later, or the ring buffer dropped lines); `capturedFrom` is the oldest time it holds.
- `crashes`: at most `limit` reports (default 10, at most 100), the newest, oldest first.

### A crash report

```
2026-11-07 10:00:00.312 crash com.example.app pid 4242
  java.lang.IllegalStateException: boom
  thread main
    at com.example.app.CartActivity.pay(CartActivity.kt:88)
    at android.view.View.performClick(View.java:8028)
  report dropbox data_app_crash 2026-11-07 10:00:00
```

| kind | means | Android | iOS |
|---|---|---|---|
| `crash` | the process died of its own fault: an uncaught exception, a fatal signal, abort | uncaught Java/Kotlin exception; native crash (tombstone) | an `.ips` crash report: `EXC_BAD_ACCESS`, an uncaught `NSException`, a Swift runtime error, abort |
| `anr` | the app did not respond (main thread blocked); the system shows its dialog or kills it | ANR | - |
| `killed` | the system killed it for a reason of its own | low memory, excessive resource use | watchdog, jetsam |
| `exited` | it ended without a crash report and without agemu asking: it called `exit()`, or the report is not written yet | `System.exit`, a process death with no crash record | an exit before `ReportCrash` wrote the `.ips` (`crashes` shows the crash once it has) |

Fields: `time`, `app`, `process`, `pid`, `kind`, `exception` (the most specific type: the
exception's class or name, else the signal or Mach exception; for a kill, the mechanism),
`reason` (the exception's message, the termination reason, the ANR's subject), `thread`,
`frames` (the top 20 of the crashing thread, innermost first, as the platform's report prints
them; for an uncaught `NSException`, the exception's own backtrace), and `report` (where the
full report is: a path on the Mac, or the device's record). A report the platform is still
writing can come back without frames. Exits agemu asked for are not reports: `terminate`, a
relaunch, `uninstall`, `clear-data`, a permission revoke or reset, `reset`. A crash during such a
call is still reported (Android: the crash records are made regardless; iOS: an end with a crash
signal, the app's uncaught-exception line or an `.ips` is never taken for agemu's). Records are
believed only from their writers (Android: the crashing process itself for its own crash,
system_server's events, system daemons' lines; iOS: the system's runningboardd), so another app
cannot make agemu report that the app under test crashed, was killed or exited.

### The crash notice

agemu watches the app launched last through agemu on each device (`launch`, or `open` when the
platform names the app that took the URL). When that app crashes, stops responding, is killed
by the system or exits on its own:

- **The next call says so, once**, whatever the call: the daemon response carries `appCrash`
  (the report's fields with up to 3 frames, and `running`), the CLI prints
  `note: com.example.app crashed at 10:00:00.312: java.lang.IllegalStateException: boom; it is
  not running; crashes has the report` on stderr, and MCP starts the result with a `NOTE:` line.
  Once per crash and device: the first call that sees it reports it, from any agent. Reading the
  crash with `crashes` counts as told.
- **A call that fails while the app is down says why**, every time, after its own error:
  `no element matches {"text":"Pay"} (com.example.app crashed at 10:00:00.312: ...)`, with exit
  status 4 and code `app_crashed`. So "the selector did not match" and "the app crashed" read
  differently. An ANR leaves the app running; then only the first call after it says so.
- **A wait stops at once** (`wait`, and the `--wait-*` of an action) when the app is down after a
  crash the agent has not been told about yet: one during the call, or since its last call. It
  fails with `stopped waiting for {...} to appear (com.example.app crashed at ...)` instead of
  waiting out its timeout. The wait asks before each UI tree read and while one runs; once the
  crash is known, that read gets 300 ms more (an element still on screen is found). Once told, a
  wait runs its course (the agent may be waiting for something else). A wait for an element to
  disappear is not cut short.
- `terminate`, `uninstall` or `clear-data` of the watched app stop watching it; `reset` and
  `shutdown` forget the device's launches.

### Lifecycle

- **Log capture** is per device and boot. It starts at the latest when the first app launch
  through agemu begins, or at the first `logs` call, whichever comes first; lines from before
  that come from the OS's own buffer where it keeps one (Android's logd ring; on iOS what the
  unified log persisted, so mostly not `info` lines). It runs until the device shuts down (Android:
  also standby) or the daemon exits. A daemon restart loses what was captured, the launch
  records `since launch` uses, and the reset floor below.
- **Ring buffers**: per device, about 2 MiB for user apps' lines and 2 MiB for all others
  (some 10,000 lines each), so system chatter cannot push an app's lines out.
- **Crash reports** come from the platform's own records. Android: made from logd's crash and
  events buffers as the system logs them (AndroidRuntime's report, libc's fatal-signal line and
  the tombstone, ActivityManager's events); only an ANR's main-thread stack is read from dropbox,
  by `crashes`. iOS: runningboardd's exit line and the app's own last line in the log stream,
  then the `.ips` files the Mac writes, read when asked. The notice comes from watching the
  app's processes from its launch through agemu.
- **Reset** clears both: `logs` and `crashes` return only what happened after the reset's end,
  and the launches before it are forgotten (see the reset contract).

### Fidelity

- **Capture observes, it changes nothing.** No `OS_ACTIVITY_DT_MODE` or other environment for
  launched apps, no `log config`, no log level or `log.tag.*` changes, no logd buffer resize
  (`logcat -G`), no crash-dialog settings, no debugger attached. An app logs and crashes as it
  would without agemu.
- **iOS debug lines are not captured**: a log stream that asks for them turns them on in the app
  (on the Simulator the test app's `os_log_type_enabled(.debug)` read true while a `--level
  debug` stream ran, also in an app already running, and false with agemu's info-level capture,
  as without it), so the app would format messages it otherwise skips. `info` lines are captured
  live; the unified log keeps them in memory and writes them to disk only around an error or
  fault, so a backfill mostly lacks them.
- **iOS `print()` and stdout are not in the unified log**, so they are not captured; on a
  device they go nowhere either. `NSLog`, `os_log` and `Logger` are. (React Native's `console.log`
  reaches the unified log through `RCTLog`: to verify.)
- **Crash dialogs stay**: Android's "keeps stopping" and ANR dialogs show as on a device; they
  appear in `ui`, and the agent dismisses them.
- **The Simulator is not a device**: its apps run as Mac processes, so an iPhone's memory limits
  (jetsam) are not expected to apply, and its FrontBoard gives a launching app 599.82 s before
  the scene-create watchdog fires (seen in its log), so a slow launch is not killed as on a
  device. A run without kills on the Simulator does not prove the app stays within a device's
  limits.
- **iOS crash reports are the Mac's**: `ReportCrash` on the Mac writes them to
  `~/Library/Logs/DiagnosticReports` with the Mac's other reports, seconds after the crash, and
  stopped after the test app's 25th report of the day (`osanalyticshelper`: "25 of max 25"; none
  of its next 21 crashes got one); agemu reads only the device's own and does not delete them.
- Times are the Mac's clock: Android's guest clock is corrected when it differs (after a
  snapshot load, say), so `since` and the crash notice compare with the daemon's own times.

## Status

Logs and crashes: implemented on both platforms. Android: see [Logs and crashes
(Android)](#logs-and-crashes-android) below. iOS: see [iOS logs and
crashes](#ios-logs-and-crashes) below.

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

#### Logs and crashes (Android)

One `logcat -B` stream per device into the daemon; crash records are made as the system logs
them (AndroidRuntime's report, libc's fatal-signal line and the tombstone in the crash buffer;
ActivityManager's `am_crash`, `am_anr`, `am_kill`, `am_proc_died` in the events buffer), not
read from dropbox or exit records afterwards, except an ANR's main-thread stack, which comes
from its dropbox entry. Mechanism and every measurement:
[ANDROID.md, Logs and crashes](ANDROID.md#logs-and-crashes-2026-11-07). On agemu-lab (API 37.1),
host load average 9-15:

- **Crash notice**: per trial a terminate, a cold launch, then the crash 0.6 s after the app is
  ready. From sending the link that crashed the app to the first response carrying the notice,
  on the merged build with 5 s between trials (n=5 each, 20 of 20 reported): Java 10.3 ms,
  SIGSEGV 10.3, abort 62.1 (ART dumps its threads before the signal), `System.exit` 19.2
  (medians). With the trials back to back, a crash's own logging and the next launch kept the
  device busy, and the medians were 13.0, 73.7, 238.8 and 147.8 ms (up to 345 ms). An earlier
  build that always streamed reported 50 of 50 (n=10 each, ANR included) at medians 8.8-60.3 ms.
  An ANR comes from the system 15.5 s after the blocked input (5 s × this image's
  `ro.hw_timeout_multiplier` 3) and is reported 2.9-5.9 ms after it (n=3). A tap on a gone
  button after a crash or exit answers `app_crashed` (exit 4) every time.
- **Frames**: a Java crash's at once (12 here, 3 in the notice); a native crash's from the
  tombstone about 120 ms after the signal; an ANR's main thread from dropbox, 621-786 ms after it
  on the merged build (3 of 3; `crashes` looks again 150 ms after a look that came too early;
  an earlier build looked once a second and got them 1.07 s after), but only for the first 6
  ANRs of a process in 10 minutes (Android's dropbox rate limit). The ANR trace files are
  readable only by the system.
- **While the device logs heavily** (after more than 100 records in each of 3 consecutive
  seconds, the capture reads logd in dumps every 250 ms instead of a record-by-record stream): a
  Java crash's notice 121.4 ms (25.2-143.2) after the request with the probe logging 500 lines/s
  (n=5, merged build; 218.6 ms, n=10, on a build that switched after one busy second).
- **Cost**, capture on against off, 4 windows each, on a build that switched to dumps after one
  busy second: idle, the emulator 5.25% of a core against 5.30% (no measurable difference), the
  daemon 0.02%; the probe logging 500 lines/s, the emulator 20.1% against 12.6% (+7.5 points; a
  record-by-record stream had cost +45), the daemon 0.27%, the adb server 0.13%. The merged
  build streams the first 2-3 s of such a rate. Not measured: 40-100 records/s, where the
  stream stays on (estimated up to about 9 points of a core from the +45 at 500/s). The
  daemon's footprint +12 MB idle, +16 MB with full rings.
- **logs**, daemon round trip: the app's newest 100 lines 0.56 ms against 252 ms for
  `adb logcat -d --uid` (n=15, interleaved); warnings and worse 0.48 against 223 ms; a text
  filter 3.83 against 198 ms; the whole device's newest 100 lines 1.38 against 42.7 ms. Through
  the CLI (one process per call) 8.2-12.1 ms.
- **Deviations**: verbose and debug lines are captured (Android's logd does not change what an
  app logs when someone reads); `Log.wtf` logs at E on this image, so it is `error`. A wait
  after a crash stops 30-169 ms after it (medians per crash kind, n=8 each; 326-377 ms on the
  round-4 build, 5.0 s before it). The on-device agent no longer waits for the window of an app
  whose process is gone (crashing, dead, or stopped by `crash_dump`): the first UI tree read once
  the crash is known took 3.4-113 ms (medians per kind, n=12) instead of up to 5.0 s, and the
  read after the wait 2.8-5.3 ms instead of 4.7 s (see ANDROID.md, "The first tree read after a
  crash"). Calls that do not read the tree carry the notice within milliseconds. With a 5,000-line burst that overflows
  logd, logd kept fewer lines with capture on (median 3,153 against 4,618, n=4 each, p≈0.06)
  and the app's own `Log.i` calls were slower (median 7.54 against 6.43 ms per burst, 20 each,
  p≈0.01). The second crash of an app within a minute shows "keeps stopping": its
  process waits in its crash handler until the dialog goes (or the system kills it as cached,
  15 s later here), and a `launch` meanwhile either brings that frozen activity back or fails
  after 20 s without showing the app (both seen): dismiss the dialog or `terminate` the app
  first. The capture starts after the first `launch` returns, so a crash during that launch is
  told by the next call. A capture reads logd's buffer back at most 10 minutes;
  logd keeps its 2 MiB buffers compressed (some 7 MiB of lines each), so it can hold older
  lines than agemu's 2 MiB rings (`partial` says so).

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
  just-installed app on a new simulator, for two quick opens of the same URL, and for every open
  of an Expo development build in front until one was answered; not in 36 benchmark opens).
  agemu answers it with Open. `app` is not reported.
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

### iOS logs and crashes

`logs`, `crashes` and the crash notice are implemented; mechanisms, measurements and caveats:
[IOS.md, Logs and crashes](IOS.md#logs-and-crashes). In short:
- **logs**: one `log stream --style ndjson --level info` per booted simulator (whole device),
  started when an app is installed or launched through agemu or at the first `logs` call; a
  read is served from memory: 1.11 ms over the daemon socket and 8.84 ms through the CLI, against
  961 ms for `xcrun simctl spawn <udid> log show --last 1m --predicate ...` (medians, n=12 each,
  interleaved; agemu returned at most 200 lines, `log show` a median 648). Bursts of 1000 and
  10,000 lines were held whole (8 of 8).
- **Cost**: with the app idle, no measurable CPU and about 15 MB in the daemon's processes; while
  the app logs 200 lines/s, about 4% of a core (3.3 points in the simulator, 0.8 in the daemon)
  and about 22 MB. The app's own logging took 29.2 against 24.3 ms per 1000 lines with the stream
  running (4 ABBA windows per arm of 3 bursts; every "on" window above every "off" one).
- **The crash notice**: the launched pid's exit (kqueue), runningboardd's exit line and the app's
  own last line. A `wait` for a missing element ended with `app_crashed` 58-791 ms after the
  crash (median 199 ms, 60 of 60, six ways of ending, n=10 each; an earlier build of the capture,
  with leftover in-simulator streams running), carrying the NSException's name, reason and 3
  frames (the exception machinery's; the app's frame is the 4th, in `crashes`), the Swift runtime
  error's message and location, or the signal; without the watcher the same waits ran to their
  15 s timeout (10 of 10, run separately). On the merged build (smoke, n=3 per kind, load
  60-67): NSException 145.5 ms, Swift `fatalError` 131.9, bad access 114.3 (medians), 9 of 9
  with exit 4.
- **crashes**: the same records, completed by the Mac's `.ips` reports of this device: 19 of 40
  crashes got one, 1.7-42.9 s after the crash (median 10.4 s), and none after the test app's 25th
  report that day. The frames of a signal crash or a Swift runtime error come only from the
  `.ips`: for 13 of 30 such crashes, 1.7-17.5 s later; never for the other 17.

Deviations from the sections above:
- `exited` also stands for an end seen before runningboardd's line (kqueue is first); the
  watcher waits up to 150 ms for that line, so a notice right after the crash says how it ended.
- A FrontBoard force quit (`simctl terminate`, a quit in the app switcher) and a privacy
  permission change (TCC) end the app without being reported, also when agemu did not ask.
  `install` over a running instance is one of agemu's own exits too. Any end with crash evidence
  (a crash signal, the app's uncaught-exception or Swift-error line, an `.ips`) is reported even
  inside agemu's own call window.
- `crashes` reads only agemu's simulators (`agemu-*`), like every other call.
- The capture slows the app's own logging (above): "observe only" holds for what is logged and
  at which level (debug stays off), not for its cost.
- Lines from before the stream's first line are read once from the unified log's store, for user
  apps only and from 30 s before the capture started: default, error and fault lines, not info.
