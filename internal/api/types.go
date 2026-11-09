// Package api defines the platform-neutral contract shared by the daemon,
// the CLI, the MCP server and the platform backends.
//
// Coordinate convention: every coordinate and frame is in SCREENSHOT PIXELS
// (the pixel grid of a scale=1 screenshot of the current profile), origin at
// the top-left. An agent can take a screenshot, pick a point on it, and tap
// that exact point on either platform.
package api

import (
	"errors"
	"fmt"
	"math"
	"strings"
)

type Platform string

const (
	Android Platform = "android"
	IOS     Platform = "ios"
)

// ErrNotImplemented is returned by backend operations that are not available.
// Test with errors.Is: NotImplementedError matches it, also after a round trip through the
// daemon socket (the response carries code "not_implemented").
var ErrNotImplemented = errors.New("not implemented")

// NotImplementedError says that a capability has no implementation on a platform, either not
// yet or because the platform has no path to it that behaves like a real device. Reason says
// which, for the agent reading the error.
type NotImplementedError struct {
	Platform Platform
	What     string // a daemon method ("push") or a narrower thing ("permission \"siri\"")
	Reason   string // optional
}

func (e *NotImplementedError) Error() string {
	s := e.What + " is not implemented on " + string(e.Platform)
	if e.Reason != "" {
		s += ": " + e.Reason
	}
	return s
}

func (e *NotImplementedError) Unwrap() error { return ErrNotImplemented }

// NotImplemented returns a *NotImplementedError. Backends return it for a part of a capability
// they cannot do faithfully (one permission name, say); the daemon returns it for a capability
// whose optional backend interface a backend does not implement.
func NotImplemented(p Platform, what, reason string) error {
	return &NotImplementedError{Platform: p, What: what, Reason: reason}
}

// Permission is a platform-neutral privacy permission name. Each backend maps it to its own
// permissions (Android runtime permissions, iOS privacy services); docs/CAPABILITIES.md has
// the table. A name containing a '.' is a raw platform permission, passed through as is
// (android.permission.READ_SMS); backends without raw names answer NotImplemented.
type Permission string

const (
	PermCamera          Permission = "camera"
	PermMicrophone      Permission = "microphone"
	PermPhotos          Permission = "photos"     // read the photo library (full access)
	PermPhotosAdd       Permission = "photos-add" // add to the photo library only
	PermLocation        Permission = "location"   // while the app is in use
	PermLocationAlways  Permission = "location-always"
	PermContacts        Permission = "contacts"
	PermContactsLimited Permission = "contacts-limited"
	PermCalendar        Permission = "calendar"
	PermReminders       Permission = "reminders"
	PermNotifications   Permission = "notifications"
	PermMotion          Permission = "motion" // motion and fitness / activity recognition
	PermMediaLibrary    Permission = "media-library"
	PermSiri            Permission = "siri"
)

// Permissions lists every platform-neutral permission name.
var Permissions = []Permission{
	PermCamera, PermMicrophone, PermPhotos, PermPhotosAdd, PermLocation, PermLocationAlways,
	PermContacts, PermContactsLimited, PermCalendar, PermReminders, PermNotifications,
	PermMotion, PermMediaLibrary, PermSiri,
}

// Raw reports whether p is a raw platform permission name rather than a neutral one.
func (p Permission) Raw() bool { return strings.Contains(string(p), ".") }

// Known reports whether p is a neutral name or a raw platform name.
func (p Permission) Known() bool {
	if p.Raw() {
		return !strings.ContainsAny(string(p), " \t\n,")
	}
	for _, k := range Permissions {
		if p == k {
			return true
		}
	}
	return false
}

// PermissionAction is what to do with a permission:
//   - grant: the app has access, as if the user allowed it (the system dialog never shows);
//   - revoke: the app is denied, as if the user chose "Don't allow" and the app may not ask
//     again (iOS: denied; Android: denied and user-fixed);
//   - reset: back to "not determined", so the app asks again on next use.
type PermissionAction string

const (
	PermissionGrant  PermissionAction = "grant"
	PermissionRevoke PermissionAction = "revoke"
	PermissionReset  PermissionAction = "reset"
)

// Appearance is the system light/dark mode.
type Appearance string

const (
	AppearanceLight Appearance = "light" // the baseline; reset restores it
	AppearanceDark  Appearance = "dark"
)

// App is one installed app.
type App struct {
	ID      string `json:"id"`                // package name / bundle id
	Name    string `json:"name,omitempty"`    // display name, when the backend knows it
	Version string `json:"version,omitempty"` // versionName / CFBundleShortVersionString, when known
}

// MaxPushPayload is the largest push payload in bytes (compact JSON), APNs' limit for a
// regular (not VoIP) notification.
const MaxPushPayload = 4096

// Device is one virtual device managed by agemu.
type Device struct {
	// ID is globally unique and prefixed with the platform: "android:<avd>" or "ios:<udid>".
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Platform  Platform `json:"platform"`
	State     string   `json:"state"` // "shutdown" | "booting" | "booted" | "parked" | "preparing"
	Profile   string   `json:"profile,omitempty"`
	Screen    *Screen  `json:"screen,omitempty"`
	OSVersion string   `json:"osVersion,omitempty"`
	// Preparing is set while the device is in StatePreparing.
	Preparing *Preparing `json:"preparing,omitempty"`
	// PrepareError is why the last preparation failed (or was cancelled), until the next boot.
	PrepareError string `json:"prepareError,omitempty"`
	// NoBaseline: the device runs but has no baseline snapshot (a daemon stopped or was
	// restarted while preparing it). Calls work, but reset cannot; `boot` prepares it again.
	NoBaseline bool `json:"noBaseline,omitempty"`
}

// StatePreparing is the state of a device whose boot takes a baseline snapshot (a new Android
// device, or a retake): the boot goes on in the daemon, other calls on the device fail at once
// with a PreparingError, and `devices` shows the phase and the time so far.
const StatePreparing = "preparing"

// Preparing describes a preparation in progress.
type Preparing struct {
	Phase    string  `json:"phase"`    // e.g. "cold boot", "settling", "saving"
	ElapsedS float64 `json:"elapsedS"` // seconds since the boot started
	// MaxS is the longest the preparation can take, in seconds (its bound).
	MaxS float64 `json:"maxS,omitempty"`
	Hint string  `json:"hint,omitempty"`
}

// ErrPreparing matches (errors.Is) the error of a call on a device that is preparing its baseline,
// also after a round trip through the daemon socket (code "preparing").
var ErrPreparing = errors.New("the device is preparing its baseline")

// PreparingError is returned at once for a call on a preparing device.
type PreparingError struct {
	Device  string
	Phase   string
	Elapsed float64 // seconds
	MaxS    float64 // seconds; 0 if unknown
}

func (e *PreparingError) Error() string {
	s := fmt.Sprintf("%s is preparing its baseline snapshot (%s, %.0fs so far", e.Device, e.Phase, e.Elapsed)
	if e.MaxS > 0 {
		s += fmt.Sprintf(", at most about %.0f min in all", math.Ceil(e.MaxS/60))
	}
	return s + "; this happens once per device): poll `devices` until it is booted, or call `boot` again"
}

func (e *PreparingError) Unwrap() error { return ErrPreparing }

// StateParked is the state of a booted device that `park` stopped in place
// (backend.Parker); the next call on it resumes it.
const StateParked = "parked"

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
	// NoWait returns as soon as the boot turns out to need a baseline snapshot (state
	// StatePreparing; the boot goes on in the daemon), or after a short bound. A boot from an
	// existing baseline still returns booted.
	NoWait bool `json:"noWait,omitempty"`
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
