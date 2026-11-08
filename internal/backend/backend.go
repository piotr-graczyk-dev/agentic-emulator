// Package backend defines the interface every platform backend implements.
package backend

import (
	"context"
	"time"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/api"
)

// Backend drives one platform. Device ids passed in are full ids ("android:x" / "ios:y").
// All coordinates are screenshot pixels of the current profile (see api package doc).
type Backend interface {
	Platform() api.Platform

	List(ctx context.Context) ([]api.Device, error)
	Create(ctx context.Context, req api.CreateRequest) (api.Device, error)
	// Boot brings the device to an interactive, agent-ready state. It should use the
	// fastest path available (warm snapshot) unless opts.Cold is set.
	Boot(ctx context.Context, id string, opts api.BootOptions) (api.Device, error)
	Shutdown(ctx context.Context, id string) error
	// Reset returns a booted device to its clean baseline state as fast as possible.
	//
	// Reset contract: Reset also undoes everything the optional capability interfaces below can
	// change, so a test that used them leaves nothing behind for the next one:
	//   - appearance: light (AppearanceSetter);
	//   - location: no simulated location, i.e. the device's baseline location state
	//     (LocationSetter; iOS: none, Android: the emulator's default fix);
	//   - clipboard: empty (ClipboardAccessor);
	//   - permissions: every app's permissions back to not determined (PermissionSetter);
	//   - apps: the apps installed since the baseline are gone (iOS: user apps uninstalled;
	//     Android: the snapshot), with their data and their delivered notifications;
	//   - logs and crashes: the backend drops the log lines and crash records it captured
	//     before the reset (LogReader, CrashReader, AppWatcher). The daemon also starts logs
	//     and crashes at the reset's end, so reports kept on the host (iOS .ips) do not return.
	// State a capability changes that Reset does not restore must be listed as "survives
	// reset" in docs/CAPABILITIES.md.
	Reset(ctx context.Context, id string) error
	// SetProfile switches the screen to the given profile. It may return a different
	// device id when the platform needs another device for that profile (iOS).
	SetProfile(ctx context.Context, id string, p api.Profile) (api.Device, error)

	Screenshot(ctx context.Context, id string, opts api.ScreenshotOptions) (api.Screenshot, error)
	UITree(ctx context.Context, id string) (*api.UITree, error)

	Tap(ctx context.Context, id string, x, y int) error
	Swipe(ctx context.Context, id string, x1, y1, x2, y2, durationMs int) error
	TypeText(ctx context.Context, id string, text string) error
	// Key presses a named key: home, back, enter, delete, tab, escape, volumeup, volumedown, lock.
	Key(ctx context.Context, id string, key string) error

	Install(ctx context.Context, id string, path string) error
	Launch(ctx context.Context, id string, appID string) error
	Terminate(ctx context.Context, id string, appID string) error

	// Close releases helper processes and connections (devices keep running).
	Close() error
}

// LaunchOptions are extra launch parameters. A development client, for example, needs the
// URL of its dev server as a process argument (Expo: --initialUrl http://localhost:8081).
type LaunchOptions struct {
	Args     []string          // process arguments
	Env      map[string]string // process environment
	Relaunch bool              // terminate a running instance first
}

// Launcher is implemented by backends that support LaunchOptions.
type Launcher interface {
	LaunchWith(ctx context.Context, id string, appID string, o LaunchOptions) error
}

// Settler is implemented by backends that can tell when the screen has stopped changing. The
// daemon uses it to tap a selector where the element comes to rest, not mid-scroll or
// mid-transition, without waiting out animations that never stop (a blinking cursor, a
// spinner, a countdown):
//   - Settle waits briefly for a still screen and reports whether it got one.
//   - When it did, the tap is the backend's Tap, whose own settle catches a transition that
//     started during the tree read.
//   - When it did not, the daemon reads the tree again until the element's frame repeats, and
//     TapNow taps without the backend's own wait for a still screen (it would only time out).
type Settler interface {
	Settle(ctx context.Context, id string) (bool, error)
	TapNow(ctx context.Context, id string, x, y int) error
}

// Actionability is implemented by backends that wait, before a selector tap, until the target can
// take it, like Playwright's actionability checks: no window transition runs and the element has
// stopped moving. AwaitActionable calls find (a fresh UI-tree read of the target) as often as it
// needs and returns the element to tap. Its waits are bounded: when one gives up, it logs that
// and returns the latest read with ok false, and the daemon taps it anyway. An error (the
// element went away while it moved) means no tap. The daemon then taps with Tap, a normal touch.
type Actionability interface {
	AwaitActionable(ctx context.Context, id string, find func() (*api.Element, error)) (el *api.Element, ok bool, err error)
}

// ElementTapper is implemented by backends that tap a selector's element knowing the element,
// not only its centre (iOS: an element that appeared since the last input is tapped once its
// presentation has finished, since UIKit drops touches until then). The daemon uses it instead
// of Tap and TapNow for a tap by selector; atRest is the daemon's verdict that the screen kept
// changing and the element's frame repeated (TapNow's case).
type ElementTapper interface {
	TapElement(ctx context.Context, id string, el *api.Element, atRest bool) error
}

// Parker is implemented by backends that can park a booted device: stop it in place, so it uses
// no CPU and the host can compress or swap out its memory, and resume it later with everything
// as it was (the apps, their processes and state, the screen). iOS stops the simulator's
// processes (SIGSTOP); Android pauses the VM without restoring the baseline (unlike standby).
//
//   - Park returns once the device is stopped.
//   - Parked reports, from memory, whether the device is parked; the daemon calls it before
//     every call on a device, so it must not touch the device.
//   - Resume continues a parked device and returns once it is agent-ready; on a device that is
//     not parked it does nothing and reports false.
//
// The daemon resumes a parked device before any other call on it (logs and crashes excepted:
// they read what was captured), so an agent never acts on a stopped device. Fidelity: the
// device's clocks run on during a park (apps see a jump of its length and their timers fire
// on resume); docs/IOS.md and docs/ANDROID.md ("Parking") list what was measured.
type Parker interface {
	Park(ctx context.Context, id string) error
	Parked(id string) bool
	Resume(ctx context.Context, id string) (bool, error)
}

// IDNormalizer is implemented by backends whose device ids have more than one spelling that
// names the same device (an iOS UDID in lower case; an AVD name in another case on a
// case-insensitive disk). CanonicalID returns the one spelling the backend lists; the daemon
// uses it for every call, so device locks, idle times and park state have one key per device.
// An id it does not recognize is returned unchanged.
type IDNormalizer interface {
	CanonicalID(id string) string
}

// FrameWaiter is implemented by backends that can block until the screen shows a new frame.
// The daemon's wait re-reads the UI tree as soon as the screen has changed instead of polling
// on a timer. WaitFrame with mark 0 returns the current mark at once; otherwise it returns
// once a frame newer than mark has shown and the screen has had no big change for a moment,
// or after max, with the newest mark.
type FrameWaiter interface {
	WaitFrame(ctx context.Context, id string, mark uint64, max time.Duration) (uint64, error)
}

// Test capabilities (docs/CAPABILITIES.md). Each is an optional interface: the daemon checks
// it with a type assertion and answers api.NotImplemented for a backend without it. A backend
// that implements an interface but cannot do one case faithfully (one permission name, say)
// returns api.NotImplemented for that case, with the reason. The daemon validates and
// normalizes the arguments first and holds the device lock during every call, reads included.
// Every method that changes what the app sees should mark input (frame waits count from it).

// URLOpener opens a URL or deep link the way following a link in another app does, not the
// way a shell does:
//   - Android: ACTION_VIEW with CATEGORY_BROWSABLE, FLAG_ACTIVITY_NEW_TASK and no package,
//     started with startActivityAndWait (as launch is). An activity whose intent filter lacks
//     BROWSABLE does not match, as from a real link (`am start -d` would open it). The caller
//     is still the shell, so getReferrer() differs from a browser's. Returns once the target
//     activity, or the disambiguation dialog, is displayed.
//   - iOS: SimDevice openURL (what `simctl openurl` does). Safari's "Open in <app>?"
//     confirmation never shows. Returns once the system has accepted the URL.
//
// App Links / universal links open the app only when their domain is verified, as on a device;
// otherwise the browser opens. app is the package / bundle id that took the URL, "" when the
// backend cannot tell. A URL no app handles is an error.
type URLOpener interface {
	OpenURL(ctx context.Context, id, url string) (app string, err error)
}

// PermissionSetter grants, revokes or resets an app's privacy permissions without the system
// dialog (see api.PermissionAction for the states). perms holds normalized names (api.Permission,
// neutral or raw); an empty perms with PermissionReset means all of the app's permissions.
// Backends check every name before changing anything, and:
//   - refuse a grant the app could not get on a device: Android, a permission its manifest
//     does not request; iOS, a service whose Info.plist usage description is missing (on a
//     device the app would crash when it asks);
//   - answer api.NotImplemented for a name without a faithful mapping on the platform.
//
// Fidelity: the dialog never shows, so its look and text are not tested. Android kills the
// app's process when a permission is revoked or reset, as on a device; iOS may terminate it.
type PermissionSetter interface {
	SetPermissions(ctx context.Context, id, app string, action api.PermissionAction, perms []api.Permission) error
}

// LocationSetter simulates the device's location fix (validated WGS84 degrees). ClearLocation
// returns to the baseline location state: iOS, no simulated location; Android, the emulator's
// default fix (its GNSS always reports one).
type LocationSetter interface {
	SetLocation(ctx context.Context, id string, lat, lon float64) error
	ClearLocation(ctx context.Context, id string) error
}

// ClipboardAccessor reads and writes the device clipboard as plain text ("" when it holds no
// text). The OS's own paste prompts and toasts still show, as on a device. The host's clipboard
// must never reach the device, and agemu's own use of the clipboard (iOS types by pasting)
// must leave the clipboard as it was.
type ClipboardAccessor interface {
	Clipboard(ctx context.Context, id string) (string, error)
	SetClipboard(ctx context.Context, id, text string) error
}

// AppearanceSetter reads and switches the system light/dark mode. Appearance reports what apps
// see. SetAppearance returns once the change is applied and the foreground app has had a moment
// to redraw (bounded, like set_profile).
type AppearanceSetter interface {
	Appearance(ctx context.Context, id string) (api.Appearance, error)
	SetAppearance(ctx context.Context, id string, a api.Appearance) error
}

// AppManager manages user apps (apps not part of the system image):
//   - Apps lists them, sorted by id;
//   - Uninstall removes one with its data (a system app is an error);
//   - ClearAppData leaves the app installed and stopped, in the state of a fresh install:
//     data, caches, preferences and permissions are gone. Android: `pm clear`, the Settings
//     "Clear storage" action. iOS has no such action; the faithful equivalent is delete and
//     reinstall, after which the keychain survives, as on a device.
type AppManager interface {
	Apps(ctx context.Context, id string) ([]api.App, error)
	Uninstall(ctx context.Context, id, app string) error
	ClearAppData(ctx context.Context, id, app string) error
}

// PushSender delivers a push notification to an app. payload is a compact JSON object with an
// "aps" object, at most api.MaxPushPayload bytes. iOS: SimDevice sendPushNotification (what
// `simctl push` does): the app receives it as from APNs, and an alert shows only with
// notification permission. There is no APNs server, device token or delivery priority.
// Android has no faithful path (FCM delivery needs the app's server credentials), so no
// Android backend should implement it.
type PushSender interface {
	Push(ctx context.Context, id, app string, payload []byte) error
}

// Logs and crashes (docs/CAPABILITIES.md, "Logs and crashes"). Three optional interfaces, so a
// backend can deliver them one at a time. The daemon answers api.NotImplemented for logs and
// crashes without LogReader / CrashReader; without AppWatcher, calls carry no crash notice.
//
// Unlike the capabilities above, these calls run WITHOUT the device lock: they read what the
// backend has captured and must not change the screen or wait for the device's other work.
// Backends synchronize their own buffers.
//
// Fidelity: capture observes what the OS and the app already log; it must not change it. No
// OS_ACTIVITY_DT_MODE or other environment for launched apps, no `log config`, no log level or
// `log.tag.*` property changes, no logd buffer resize (`logcat -G`), no crash-dialog settings,
// no debugger. iOS debug-level messages are therefore not captured: a stream that asks for
// them turned them on in the app (os_log_type_enabled(.debug) read true while a debug-level
// stream ran, false with the info-level capture; docs/IOS.md). Every time is the host's wall
// clock: a backend whose device clock can differ (Android after a snapshot load) corrects it,
// so the daemon's own times (the start of a launch, the end of a reset) compare with it.
//
// Lifecycle (per device and boot): log capture starts at the latest when the device's first
// app launch through agemu begins, or at the first Logs call, whichever comes first, and the
// lines from before it are backfilled from the OS's own buffer where there is one. It runs
// until the device shuts down (or, Android, goes to standby) or the daemon exits. Reset
// empties it (see Backend.Reset). The ring buffer keeps user apps' lines apart from the rest
// (about 2 MiB each per device, by LogEntry.Size: some 10,000 lines each), so system chatter
// cannot push an app's lines out; one message is cut at api.MaxLogMessage. Measure what
// capture costs (CPU and memory of the device and the daemon, idle and during an app flow)
// against no capture, and keep it off the agent's action path.

// LogReader returns the log lines a backend captured on a device.
//
// Logs returns the entries of q.App's processes (every process when q.App is ""; Android: the
// app's uid, so all its processes; iOS: processes whose executable is in the app's bundle,
// extensions included) at or after q.Since and captured after the cursor q.After, oldest
// first. It returns them all: the daemon applies the level, text filters and bounds, though a
// backend may apply q.Level, q.Contains and q.Regex first to save work. The batch carries the
// cursor of the newest captured entry, the oldest time the capture holds, and whether the
// asked window reaches back before it. Cursors grow across the backend's captures and daemon
// restarts (start each capture's counter at its start time in microseconds), so a stale one
// returns the newer lines rather than nothing.
type LogReader interface {
	Logs(ctx context.Context, id string, q api.LogQuery) (api.LogBatch, error)
}

// CrashReader returns crash, ANR and system-kill reports (api.CrashKind) of a device's apps:
// those of q.App (every app when ""), at or after q.Since, oldest first, with the exception,
// reason and the crashing thread's top frames when the platform's report has them (a report
// still being written may come back without them). Kills agemu asked for (terminate, a
// relaunch's terminate, uninstall, clear_app_data, permission revoke or reset, reset) are not
// reports, here or in AppWatcher, unless the end carries crash evidence (a crash signal, the
// app's uncaught exception, a crash report): the app crashed during agemu's call. Records are
// trusted only from their writers (Android: the crashing process's own pid, system_server's
// events, a system daemon's lines), so one app cannot report another's crash. Sources that outlive the process
// (Android dropbox and ApplicationExitInfo, iOS .ips files) can be read on demand; reports of
// other devices on the same host (iOS: other simulators' .ips files) must be left out.
type CrashReader interface {
	Crashes(ctx context.Context, id string, q api.CrashQuery) ([]api.Crash, error)
}

// AppWatcher lets the daemon tell an agent, in the result of its next call, that the app it
// launched crashed (docs/CAPABILITIES.md, "The crash notice"). AppState reports whether app has
// a live process now and the newest crash, ANR, system kill or unexpected exit of app at or
// after since (nil when none). An exit agemu did not ask for whose cause is not known yet (iOS:
// before ReportCrash wrote the .ips) is api.CrashKindExited, so the agent learns at once that
// the app is gone; CrashReader returns the crash once the report exists. The daemon calls it after every call on the device and in every wait iteration,
// so it must answer from memory or one cheap local check (no device round trip, well under a
// millisecond); the backend watches the app's processes from its first launch through agemu at
// the latest (Android: the events buffer's am_crash / am_anr / am_proc_died, or the agent's
// process observer; iOS: the app's host pid, which ends visibly to the Mac). The crash it
// returns may lack frames while the platform writes its report; CrashReader has them later.
type AppWatcher interface {
	AppState(ctx context.Context, id, app string, since time.Time) (api.AppState, error)
}
