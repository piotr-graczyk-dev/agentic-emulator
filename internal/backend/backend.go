// Package backend defines the interface every platform backend implements.
package backend

import (
	"context"

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

// Settler is implemented by backends that can wait until the screen stops changing. The daemon
// settles before it resolves a tap selector, so the element's frame is read where it comes to
// rest and not mid-scroll or mid-transition.
type Settler interface {
	Settle(ctx context.Context, id string) error
}
