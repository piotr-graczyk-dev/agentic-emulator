package ios

import (
	"context"
	"fmt"
	"log"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/api"
)

// Taps on just-presented UI.
//
// UIKit drops every touch from the moment a presentation begins until its completion: for alerts
// and sheets the presenting scene ignores interaction events for the transition
// (-[UIWindowScene _beginIgnoringInteractionEventsForReason:], "beginning transition for
// presentation" ... "completing transition"), and a touch in that window never reaches
// -[UIApplication sendEvent:]; an action sheet drops touches until its completion too (a probe
// app on iOS 26.5). The new UI is in the accessibility tree as soon as the presentation begins,
// 32-146 ms before its first frame shows (load average 14-67), so an agent that waits for an
// alert's text and then taps its button at once sends the touch while the screen still looks
// still. The completion comes 20-33 ms before an alert's or a sheet's last frame, but a sheet's
// tail and an action sheet's whole slide change too little of the screen for the screen-wide
// "no big change for 48 ms" wait, which ended before the completion in 19 of 22 sheet traces and
// 21 of 21 action sheet traces. See docs/IOS.md, "Taps on just-presented UI".
//
// So a tap by selector on an element that is new since the last input (it was not in the UI tree
// read before that input) asks the helper to wait until the element is ready
// (waitForRectReady): something has changed on screen since the input, and then the element's
// column has stopped changing. An element the agent could already see is tapped as before, so a
// still screen costs nothing.

// seenElements is what the UI tree held, per device: in the newest read, and in the newest read
// before the last input.
type seenElements struct {
	last   map[string]bool
	before map[string]bool // nil: no read before the last input
}

// elementKey identifies an element across UI tree reads: what it is and where it is.
func elementKey(e *api.Element) string {
	f := e.Frame
	return fmt.Sprintf("%s\x00%s\x00%s\x00%d,%d,%d,%d", e.Role, e.Label, e.ID, f.X, f.Y, f.W, f.H)
}

// noteTree records the elements of a UI tree read.
func (b *Backend) noteTree(udid string, t *api.UITree) {
	keys := map[string]bool{}
	t.Walk(func(e *api.Element) { keys[elementKey(e)] = true })
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.seen[udid]
	if s == nil {
		s = &seenElements{}
		b.seen[udid] = s
	}
	s.last = keys
}

// noteInput marks input sent to the device: the elements read so far are the ones the agent
// could see before it.
func (b *Backend) noteInput(udid string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.seen[udid]
	if s == nil {
		s = &seenElements{}
		b.seen[udid] = s
	}
	s.before = s.last
}

// freshElement reports whether el was not in the UI tree read before the last input (or no such
// read exists): it may belong to a presentation that is still in progress.
func (b *Backend) freshElement(udid string, el *api.Element) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.seen[udid]
	return s == nil || s.before == nil || !s.before[elementKey(el)]
}

// TapElement taps the centre of el, a match of a selector. An element that is new since the last
// input is tapped once it is ready (see above). atRest means the daemon saw the screen keep
// changing and the element's frame repeat: the screen-wide wait for no big change is skipped,
// as in TapNow.
func (b *Backend) TapElement(ctx context.Context, id string, el *api.Element, atRest bool) error {
	d, err := b.target(ctx, id)
	if err != nil {
		return err
	}
	x, y := el.Frame.Center()
	if x < 0 || y < 0 || (d.Width > 0 && (x >= d.Width || y >= d.Height)) {
		return fmt.Errorf("tap (%d,%d) is outside the %dx%d screen", x, y, d.Width, d.Height)
	}
	fresh := b.freshElement(d.UDID, el)
	params := elementTapParams(d.UDID, el, atRest, fresh)
	b.noteInput(d.UDID)
	var r struct {
		ReadyMs      float64 `json:"readyMs"`
		Pending      bool    `json:"pending"`
		ReadySettled bool    `json:"readySettled"`
	}
	if err := b.call(ctx, "tap", params, &r); err != nil {
		return err
	}
	if fresh && (r.Pending || !r.ReadySettled) {
		name := el.Label
		if name == "" {
			name = el.ID
		}
		log.Printf("ios tap on %s: %s %q was new; waited %.0f ms for it (nothing shown yet: %v, still changing: %v)",
			d.UDID, el.Role, name, r.ReadyMs, r.Pending, !r.ReadySettled)
	}
	return nil
}

// elementTapParams is the helper's tap request for el: its centre; "ready" with its frame when
// it is fresh; no screen-wide settle when it is at rest on a screen that keeps changing.
func elementTapParams(udid string, el *api.Element, atRest, fresh bool) map[string]any {
	x, y := el.Frame.Center()
	params := map[string]any{"udid": udid, "x": x, "y": y}
	if atRest {
		params["settleMaxMs"] = 0
	}
	if fresh {
		params["ready"] = []int{el.Frame.X, el.Frame.Y, el.Frame.W, el.Frame.H}
	}
	return params
}
