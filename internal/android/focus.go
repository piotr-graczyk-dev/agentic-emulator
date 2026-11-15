package android

import (
	"context"
	"time"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/api"
)

// A tap returns when the emulator has taken the touch; the app moves the input focus when it
// handles the touch, a moment later. Text typed right after a tap on another text field then
// went into the field that still had the focus: 6 of 150 `tap` + `type` pairs on the second
// field of a form, the calls 15-20 ms apart (2026-11-15, a loaded host). So typed text waits,
// for a short time, until the text field under the latest tap has the focus.
const (
	focusTapWindow = 2 * time.Second        // an older tap has had its effect
	focusWaitFor   = 600 * time.Millisecond // after that the text goes to what has the focus, as before
	focusPoll      = 15 * time.Millisecond
)

// tapMark is the latest tap: where, in logical pixels, and when.
type tapMark struct {
	x, y int
	at   time.Time
}

func (d *device) noteTap(x, y int) {
	d.mu.Lock()
	d.tap = tapMark{x, y, time.Now()}
	d.mu.Unlock()
}

func (d *device) lastTap() tapMark {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.tap
}

// fieldAt returns the text field of the focused window that contains the point (the last in
// tree order, which is the innermost), or nil.
func fieldAt(t *api.UITree, x, y int) *api.Element {
	var hit *api.Element
	var walk func(e *api.Element, inFocused bool)
	walk = func(e *api.Element, inFocused bool) {
		if e == nil {
			return
		}
		inFocused = inFocused || (e.Role == "window" && e.Focused)
		r := e.Frame
		if inFocused && e.Role == "textfield" && x >= r.X && x < r.X+r.W && y >= r.Y && y < r.Y+r.H {
			hit = e
		}
		for _, c := range e.Children {
			walk(c, inFocused)
		}
	}
	walk(t.Root, false)
	return hit
}

// awaitTapFocus waits until the text field under the tap has the focus and reports whether a
// field was still without it when the wait gave up. It returns at once when the tap is old,
// when it did not land on a text field, or when the field has the focus already. read returns
// a fresh UI tree; a failed read ends the wait (the text is typed as before).
//
// focusWaitFor bounds the polling, not a read in flight: read keeps the caller's ctx. A read
// is slow when the app's UI thread is, and the text call that follows (findFocus,
// ACTION_SET_TEXT) waits for the same thread, so cutting the read short would save no time. It
// would drop the agent connection (agentClient.call) and type before the focus is known.
func awaitTapFocus(ctx context.Context, tap tapMark, read func() (*api.UITree, error)) (gaveUp bool) {
	if tap.at.IsZero() || time.Since(tap.at) > focusTapWindow {
		return false
	}
	deadline := time.Now().Add(focusWaitFor)
	for {
		t, err := read()
		if err != nil {
			return false
		}
		f := fieldAt(t, tap.x, tap.y)
		if f == nil || f.Focused {
			return false
		}
		if !time.Now().Before(deadline) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(focusPoll):
		}
	}
}
