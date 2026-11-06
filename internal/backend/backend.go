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
