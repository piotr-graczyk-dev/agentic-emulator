package android

import (
	"context"
	"fmt"
	"time"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/api"
)

// Actionability of a selector tap (backend.Actionability). A tap on an element that has just
// appeared or is still moving often does not do what the agent asked (measured on agemu-lab):
//   - a dialog that is closing takes every touch until its exit animation ends: a tap on the
//     screen below, at once after the dialog left the UI tree, was lost 50 of 50 times;
//   - a touch on a list that still scrolls stops the scroll instead of clicking the row (48 of
//     50 failed);
//   - the runtime permission dialog's buttons are first reported where its window comes from:
//     the UI tree kept that position for up to 435 ms (also with the accessibility cache
//     cleared) while the screen still changed, and taps sent there were lost (16 of 50).
//
// The frame stream tells when the screen changes: every frame the guest posts is notified. So:
//   - when the guest has posted no frame for trustQuiet, nothing moved lately and the usual
//     (cached) tree read is used as it is: a tap on a still screen costs nothing more;
//   - otherwise the agent first waits until WindowManager runs no window transition (bounded).
//     Then the element is read fresh (accessibility cache cleared, so moving content is read
//     where it is) until it is at rest: the screen posted no frame for stillFor around a read,
//     or its frame stayed the same across restFrames posted frames (something else animates,
//     such as the ripple of the previous tap). When the agent had to wait for a window
//     transition, the UI tree may still report that window where it came from, so the frame
//     must stay the same for restSpan (the screen going still is enough as before). Without a
//     transition at first, a second check catches one that started during the reads.
//
// Both waits are bounded and logged when they give up; the tap then goes to the latest read, as
// before. The tap itself is the same touch. AGEMU_ANDROID_TAP_WAIT=0 turns the waits off.
const (
	transitionWaitFor = 2 * time.Second         // window transitions (the agent's bound)
	restWaitFor       = 1500 * time.Millisecond // the screen or the element to come to rest
	// trustQuiet: after this long without a posted frame, the cached tree read is trusted.
	trustQuiet = 500 * time.Millisecond
	// transitionSeen: a settle that took this long waited for a window transition. In the traces
	// of this work (agemu-lab, load 12-45) settles without a transition took 3-62 ms, settles
	// after "Ask camera" or a closed dialog 134-213 ms. A settle slowed past it by host load only
	// makes the rest check stricter.
	transitionSeen = 80 * time.Millisecond
	// stillFor is how long the guest must post no frame for the screen to count as still: about
	// three frames at 60 Hz.
	stillFor = 50 * time.Millisecond
	// restSpan: after a window transition, an element whose frame is the same in every read over
	// this span, while frames keep coming, is at rest. It exceeds the longest time the UI tree
	// was seen to keep a moved dialog's old position (435 ms).
	restSpan = 600 * time.Millisecond
	// restFrames: an element is at rest when its frame is the same across this many posted
	// frames (the "two frames" of Playwright's stable check).
	restFrames = 2
	// readEvery bounds the time between two reads while the screen keeps changing.
	readEvery = 100 * time.Millisecond
	// restGap spaces two reads when the frame stream cannot tell when a frame was posted (it is
	// reopening after idle): two frames at 60 Hz.
	restGap = 34 * time.Millisecond
)

// frameClock is what the rest check needs from the frame stream (a fake in tests).
type frameClock interface {
	// posts returns the number of frames posted, when the latest arrived, and whether the stream
	// is delivering (otherwise the count says nothing).
	posts() (n uint64, last time.Time, ok bool)
	// nextPost waits until a frame after the n-th is posted, at most max.
	nextPost(ctx context.Context, n uint64, max time.Duration)
}

// tapWait reports whether selector taps wait until the element is actionable
// (AGEMU_ANDROID_TAP_WAIT, default 1; 0 taps the first read at once, as agemu did before).
func tapWait() bool { return env("AGEMU_ANDROID_TAP_WAIT", "1") != "0" }

// AwaitActionable implements backend.Actionability (see above).
func (b *Backend) AwaitActionable(ctx context.Context, id string, find func() (*api.Element, error)) (*api.Element, bool, error) {
	if !tapWait() {
		el, err := find()
		return el, err == nil, err
	}
	d, err := b.devFor(id)
	if err != nil {
		return nil, false, err
	}
	e, _, err := d.conn(ctx)
	if err != nil {
		return nil, false, err
	}
	var clk frameClock
	if e.stream != nil {
		clk = e.stream
	}
	t0 := time.Now()
	settledAll := true
	settle := func() (seen bool, err error) {
		var res struct {
			Settled bool  `json:"settled"`
			Ms      int64 `json:"ms"`
		}
		if err := d.call(ctx, map[string]any{"cmd": "settle", "maxMs": transitionWaitFor.Milliseconds()}, &res); err != nil {
			return false, err
		}
		if !res.Settled {
			settledAll = false
			logf("%s: tap: a window transition still ran after %d ms; going on", d.name, res.Ms)
		}
		return time.Duration(res.Ms)*time.Millisecond >= transitionSeen, nil
	}
	reads := 0
	read := func(fresh bool) (*api.Element, error) {
		reads++
		if fresh {
			if err := d.call(ctx, map[string]any{"cmd": "clearcache"}, nil); err != nil {
				return nil, err
			}
		}
		return find()
	}
	el, rest, seen, err := awaitActionable(ctx, clk, settle, read, time.Now)
	if err != nil {
		return nil, false, err
	}
	if !rest {
		logf("%s: tap: %s was not at rest after %v (the screen kept changing); tapping its last frame %v", d.name, describeEl(el), restWaitFor, el.Frame)
	}
	if trace {
		logf("%s: tap: waited %v (transition seen %v, %d reads, at rest %v)", d.name, time.Since(t0).Round(time.Millisecond), seen, reads, rest)
	}
	return el, settledAll && rest, nil
}

// awaitActionable is the order of the checks (see the comment at the top). settle waits for
// window transitions and reports whether it had to (see transitionSeen); read reads the
// element, fresh from the app when fresh is set. seen reports that a transition was waited for.
func awaitActionable(ctx context.Context, clk frameClock, settle func() (bool, error), read func(fresh bool) (*api.Element, error), now func() time.Time) (el *api.Element, rest, seen bool, err error) {
	if el, ok, err := quietRead(clk, func() (*api.Element, error) { return read(false) }, now); err != nil || ok {
		return el, ok, false, err
	}
	if seen, err = settle(); err != nil {
		return nil, false, seen, err
	}
	// Reads after the first ask the app for every node again (13-56 ms each in this work's
	// traces, against a few ms for a cached read): the accessibility cache keeps a node's
	// bounds until an event about it arrives, which a scrolling list sends at most every 100 ms,
	// so cached reads of a moving row can agree with each other. Without a transition the first
	// read may come from the cache: a later fresh read must still agree with it. After one, every
	// read is fresh.
	reads := 0
	find := func() (*api.Element, error) {
		reads++
		return read(seen || reads > 1)
	}
	span := func() time.Duration {
		if seen {
			return restSpan
		}
		return 0
	}
	el, rest, err = awaitRest(ctx, clk, find, span(), restWaitFor, now)
	if err == nil && rest && !seen {
		// A transition can start while the element is read: a dismissed dialog leaves the UI tree
		// before its exit animation begins, and that animation takes the touch. Check once more.
		if seen, err = settle(); err == nil && seen {
			el, rest, err = awaitRest(ctx, clk, find, span(), restWaitFor, now)
		}
	}
	if err != nil {
		return nil, false, seen, err
	}
	return el, rest, seen, nil
}

// quietRead is the path for a screen that has not moved lately: when the guest has posted no
// frame for trustQuiet, one read (find) is the element at rest, unless a frame was posted
// during it. ok false means the caller must wait (or the stream cannot tell).
func quietRead(clk frameClock, find func() (*api.Element, error), now func() time.Time) (*api.Element, bool, error) {
	if clk == nil {
		return nil, false, nil
	}
	n, last, ok := clk.posts()
	if !ok || now().Sub(last) < trustQuiet {
		return nil, false, nil
	}
	el, err := find()
	if err != nil {
		return nil, false, err
	}
	if n2, _, ok2 := clk.posts(); ok2 && n2 == n {
		return el, true, nil
	}
	return nil, false, nil
}

// awaitRest reads the element (find) until it is at rest, at most max:
//   - the frame stream saw no frame posted from stillFor before a read until after it;
//   - or, while frames keep coming, its frame has been the same in every read for span, with at
//     least restFrames posted frames between the first and the last of them (a list that ends a
//     scroll can show its last position a frame before it stops taking a touch as a fling stop);
//   - or, without a delivering stream, two reads restGap apart give the same frame.
//
// Between reads it waits for the next posted frame (span 0) or for the screen to go still (at
// most readEvery). It returns the latest read and whether it is at rest. An element that
// disappears after the first read is an error: its last frame may belong to another control by
// now.
func awaitRest(ctx context.Context, clk frameClock, find func() (*api.Element, error), span, max time.Duration, now func() time.Time) (*api.Element, bool, error) {
	deadline := now().Add(max)
	var same *api.Element // the frame every read has given since sameAt
	var sameAt time.Time
	var sameN uint64 // frames posted when the first of those reads ended
	for {
		var n0 uint64
		var last0 time.Time
		ok0 := false
		if clk != nil {
			n0, last0, ok0 = clk.posts()
		}
		start := now()
		el, err := find()
		if err != nil {
			if same != nil {
				return nil, false, fmt.Errorf("%w (it disappeared while it was moving)", err)
			}
			return nil, false, err
		}
		var n1 uint64
		ok1 := false
		if clk != nil {
			n1, _, ok1 = clk.posts()
		}
		streamOK := ok0 && ok1
		if streamOK && n1 == n0 && start.Sub(last0) >= stillFor {
			return el, true, nil // still screen
		}
		if same == nil || el.Frame != same.Frame {
			same, sameAt, sameN = el, start, n1
		} else {
			held := start.Sub(sameAt)
			if streamOK && n0 >= sameN+restFrames && held >= span {
				return el, true, nil // the same frame across posted frames
			}
			if !streamOK && held >= restGap {
				return el, true, nil
			}
		}
		if !now().Before(deadline) {
			return el, false, nil
		}
		switch {
		case streamOK && span == 0:
			// restFrames frames, or stillFor without one (the screen is then still).
			for i := uint64(0); i < restFrames && ctx.Err() == nil; i++ {
				n, _, ok := clk.posts()
				if !ok {
					break
				}
				before := now()
				clk.nextPost(ctx, n, stillFor)
				if m, _, _ := clk.posts(); m == n || now().Sub(before) >= stillFor {
					break
				}
			}
		case streamOK:
			waitQuiet(ctx, clk, min(readEvery, deadline.Sub(now())), now)
		default:
			select {
			case <-ctx.Done():
			case <-time.After(restGap):
			}
		}
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
	}
}

// waitQuiet waits until the guest has posted no frame for stillFor, at most max, or until the
// stream stops delivering.
func waitQuiet(ctx context.Context, clk frameClock, max time.Duration, now func() time.Time) {
	end := now().Add(max)
	for ctx.Err() == nil {
		n, last, ok := clk.posts()
		t := now()
		if !ok || !t.Before(end) {
			return
		}
		gap := stillFor - t.Sub(last)
		if gap <= 0 {
			return
		}
		clk.nextPost(ctx, n, min(gap, end.Sub(t)))
	}
}

func describeEl(e *api.Element) string {
	switch {
	case e.Text != "":
		return fmt.Sprintf("%q", e.Text)
	case e.Label != "":
		return fmt.Sprintf("%q", e.Label)
	case e.ID != "":
		return e.ID
	}
	return e.Role
}
