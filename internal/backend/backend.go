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
	//     Android: the snapshot), with their data and their delivered notifications.
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
