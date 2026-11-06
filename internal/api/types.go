// Package api defines the platform-neutral contract shared by the daemon,
// the CLI, the MCP server and the platform backends.
//
// Coordinate convention: every coordinate and frame is in SCREENSHOT PIXELS
// (the pixel grid of a scale=1 screenshot of the current profile), origin at
// the top-left. An agent can take a screenshot, pick a point on it, and tap
// that exact point on either platform.
package api

import "errors"

type Platform string

const (
	Android Platform = "android"
	IOS     Platform = "ios"
)

// ErrNotImplemented is returned by backend operations that are not available.
var ErrNotImplemented = errors.New("not implemented")

// Device is one virtual device managed by agemu.
type Device struct {
	// ID is globally unique and prefixed with the platform: "android:<avd>" or "ios:<udid>".
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Platform  Platform `json:"platform"`
	State     string   `json:"state"` // "shutdown" | "booting" | "booted"
	Profile   string   `json:"profile,omitempty"`
	Screen    *Screen  `json:"screen,omitempty"`
	OSVersion string   `json:"osVersion,omitempty"`
}

// Screen is the current logical screen as seen by apps and screenshots.
type Screen struct {
	Width  int `json:"width"`  // pixels
	Height int `json:"height"` // pixels
	// Density: Android dpi (e.g. 420) or iOS scale factor * 160 (e.g. 3x -> 480).
	Density int `json:"density"`
}

// Profile describes a target real-world device screen.
type Profile struct {
	Name        string   `json:"name"`
	Platform    Platform `json:"platform"`
	Description string   `json:"description"`
	Width       int      `json:"width"`   // pixels
	Height      int      `json:"height"`  // pixels
	Density     int      `json:"density"` // android dpi; ios scale*160
	// DeviceType is the CoreSimulator device type identifier (iOS only).
	DeviceType string `json:"deviceType,omitempty"`
}

type Rect struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

func (r Rect) Center() (int, int) { return r.X + r.W/2, r.Y + r.H/2 }

// Element is one node of the accessibility / view hierarchy.
type Element struct {
	Ref       string     `json:"ref"`            // stable within one snapshot, e.g. "e12"
	Role      string     `json:"role"`           // normalized: button, text, textfield, image, switch, cell, window, other...
	Type      string     `json:"type,omitempty"` // raw platform class (android.widget.Button / XCUIElementTypeButton / AXButton)
	Text      string     `json:"text,omitempty"`
	Label     string     `json:"label,omitempty"` // content-description / accessibilityLabel
	ID        string     `json:"id,omitempty"`    // resource-id / accessibilityIdentifier
	Value     string     `json:"value,omitempty"`
	Frame     Rect       `json:"frame"`
	Enabled   bool       `json:"enabled"`
	Focused   bool       `json:"focused,omitempty"`
	Selected  bool       `json:"selected,omitempty"`
	Checked   *bool      `json:"checked,omitempty"`
	Clickable bool       `json:"clickable,omitempty"`
	Children  []*Element `json:"children,omitempty"`
}

type UITree struct {
	App    string   `json:"app,omitempty"` // foreground package / bundle id when known
	Width  int      `json:"width"`
	Height int      `json:"height"`
	Root   *Element `json:"root"`
}

// Walk visits every element depth-first.
func (t *UITree) Walk(fn func(e *Element)) {
	var rec func(e *Element)
	rec = func(e *Element) {
		if e == nil {
			return
		}
		fn(e)
		for _, c := range e.Children {
			rec(c)
		}
	}
	rec(t.Root)
}

type CreateRequest struct {
	Platform Platform `json:"platform"`
	Name     string   `json:"name"`
	Profile  string   `json:"profile,omitempty"`
}

type BootOptions struct {
	// Cold forces a full boot, bypassing snapshots (used for baselines).
	Cold bool `json:"cold,omitempty"`
}

type ScreenshotOptions struct {
	Path   string  `json:"path"`             // output file; daemon fills a temp path when empty
	Format string  `json:"format,omitempty"` // "png" (default) | "jpeg"
	Scale  float64 `json:"scale,omitempty"`  // 0 or 1 = full resolution
}

type Screenshot struct {
	Path   string `json:"path"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Format string `json:"format"`
	Bytes  int    `json:"bytes"`
}

// Selector finds elements in a UITree. Empty fields are ignored; all set fields must match.
type Selector struct {
	Text     string `json:"text,omitempty"`     // exact match on text or label
	Contains string `json:"contains,omitempty"` // case-insensitive substring of text or label
	ID       string `json:"id,omitempty"`       // exact resource-id / accessibilityIdentifier (suffix match after '/')
	Role     string `json:"role,omitempty"`
	Index    int    `json:"index,omitempty"` // pick the n-th match (0-based)
}

func (s Selector) Empty() bool {
	return s.Text == "" && s.Contains == "" && s.ID == "" && s.Role == ""
}
