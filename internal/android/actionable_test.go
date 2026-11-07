package android

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/api"
)

// fakeClock is a frame stream on a fake time line. While moving reports true, the guest posts a
// frame every 16 ms (nextPost returns at the next one); otherwise nextPost waits out max.
type fakeClock struct {
	t      time.Time
	n      uint64
	last   time.Time
	ok     bool
	moving func() bool
}

func (c *fakeClock) posts() (uint64, time.Time, bool) { return c.n, c.last, c.ok }

func (c *fakeClock) nextPost(ctx context.Context, n uint64, max time.Duration) {
	if c.moving != nil && c.moving() && max >= 16*time.Millisecond {
		c.t = c.t.Add(16 * time.Millisecond)
		c.n++
		c.last = c.t
		return
	}
	c.t = c.t.Add(max)
}

func (c *fakeClock) now() time.Time { return c.t }

func newClock(sinceLastFrame time.Duration) *fakeClock {
	t := time.Unix(1000, 0)
	return &fakeClock{t: t, n: 7, last: t.Add(-sinceLastFrame), ok: true}
}

// reader returns a find that gives the element's frame at the fake time (frameAt), and counts
// the reads; each read takes 5 ms.
func reader(c *fakeClock, frameAt func(time.Time) api.Rect) (func() (*api.Element, error), *int) {
	reads := 0
	return func() (*api.Element, error) {
		reads++
		f := frameAt(c.t)
		c.t = c.t.Add(5 * time.Millisecond)
		return &api.Element{Text: "Row 42", Frame: f}, nil
	}, &reads
}

func fixed(r api.Rect) func(time.Time) api.Rect { return func(time.Time) api.Rect { return r } }

func TestQuietReadTrustsAScreenThatHasNotMovedLately(t *testing.T) {
	c := newClock(2 * time.Second)
	find, reads := reader(c, fixed(api.Rect{X: 24, Y: 900, W: 1032, H: 150}))
	el, ok, err := quietRead(c, find, c.now)
	if err != nil || !ok || el.Frame.Y != 900 || *reads != 1 {
		t.Fatalf("got %v, %v, %v after %d reads; want one read, at rest", el, ok, err, *reads)
	}
	// Moved 100 ms ago: not trusted (the UI tree can still show an old position).
	c = newClock(100 * time.Millisecond)
	find, reads = reader(c, fixed(api.Rect{X: 24, Y: 900, W: 1032, H: 150}))
	if _, ok, _ := quietRead(c, find, c.now); ok || *reads != 0 {
		t.Fatalf("trusted a screen that moved 100 ms ago (%d reads)", *reads)
	}
	// No stream: cannot tell.
	if _, ok, _ := quietRead(nil, find, c.now); ok {
		t.Fatal("trusted a read without a frame stream")
	}
}

// A list still scrolls when the element first shows: a touch during the scroll stops the scroll
// instead of clicking the row. The read that counts is the one once the screen is still.
func TestAwaitRestWaitsForTheScreenToGoStill(t *testing.T) {
	c := newClock(0)
	stop := c.t.Add(300 * time.Millisecond) // the scroll ends here
	c.moving = func() bool { return c.t.Before(stop) }
	find, _ := reader(c, func(at time.Time) api.Rect {
		if at.Before(stop) {
			return api.Rect{X: 24, Y: 2166 - int(at.Sub(stop.Add(-300*time.Millisecond))/time.Millisecond), W: 1032, H: 150}
		}
		return api.Rect{X: 24, Y: 1866, W: 1032, H: 150}
	})
	el, ok, err := awaitRest(context.Background(), c, find, 0, time.Second, c.now)
	if err != nil || !ok {
		t.Fatalf("got %v, %v; want at rest", ok, err)
	}
	if el.Frame.Y != 1866 {
		t.Fatalf("tapped at y=%d, want the resting frame y=1866", el.Frame.Y)
	}
}

// After a window transition, the UI tree can report the permission dialog's buttons where the
// dialog came from and keep that position while the dialog animates (frames keep coming). An
// unchanged frame across a few posted frames must not count as at rest then; the still screen
// does.
func TestAwaitRestIsNotFooledByAStaleFrameAfterATransition(t *testing.T) {
	c := newClock(0)
	moveEnd := c.t.Add(500 * time.Millisecond)
	c.moving = func() bool { return c.t.Before(moveEnd) }
	treeCatchesUp := c.t.Add(400 * time.Millisecond)
	find, _ := reader(c, func(at time.Time) api.Rect {
		if at.Before(treeCatchesUp) {
			return api.Rect{X: 133, Y: 1959, W: 814, H: 126} // stale
		}
		return api.Rect{X: 133, Y: 1475, W: 814, H: 126}
	})
	el, ok, err := awaitRest(context.Background(), c, find, restSpan, 1500*time.Millisecond, c.now)
	if err != nil || !ok || el.Frame.Y != 1475 {
		t.Fatalf("got y=%d ok=%v err=%v; want y=1475 at rest", el.Frame.Y, ok, err)
	}
}

// Something else on screen keeps changing (the previous tap's ripple, a spinner): the screen
// does not go still, but an element whose frame stays the same across posted frames is at rest:
// at once without a transition, after restSpan after one.
func TestAwaitRestBesideAnAnimation(t *testing.T) {
	for _, span := range []time.Duration{0, restSpan} {
		c := newClock(0)
		c.moving = func() bool { return true }
		find, _ := reader(c, fixed(api.Rect{X: 24, Y: 900, W: 1032, H: 150}))
		start := c.t
		el, ok, err := awaitRest(context.Background(), c, find, span, 1500*time.Millisecond, c.now)
		if err != nil || !ok || el.Frame.Y != 900 {
			t.Fatalf("span %v: got %v, %v, %v; want the element at rest", span, el, ok, err)
		}
		took := c.t.Sub(start)
		if span == 0 && took > 50*time.Millisecond {
			t.Fatalf("span 0: took %v, want about one frame", took)
		}
		if span > 0 && (took < span || took > span+readEvery+50*time.Millisecond) {
			t.Fatalf("span %v: took %v", span, took)
		}
	}
}

// The order of the checks: a screen that has not moved lately needs one cached read and no
// settle; otherwise the transitions are waited for, the first read without a transition may be
// cached and later ones are fresh, and a transition that starts during the reads (a dismissed
// dialog's exit animation) is caught by a second settle, after which the stricter rest applies.
func TestAwaitActionableOrder(t *testing.T) {
	type call struct {
		settles int
		reads   []bool // fresh per read
	}
	run := func(c *fakeClock, transitions []bool) (call, time.Duration, bool) {
		var got call
		settle := func() (bool, error) {
			seen := got.settles < len(transitions) && transitions[got.settles]
			got.settles++
			return seen, nil
		}
		read := func(fresh bool) (*api.Element, error) {
			got.reads = append(got.reads, fresh)
			c.t = c.t.Add(5 * time.Millisecond)
			return &api.Element{Text: "Open dialog", Frame: api.Rect{X: 24, Y: 183, W: 516, H: 126}}, nil
		}
		start := c.t
		_, rest, seen, err := awaitActionable(context.Background(), c, settle, read, c.now)
		if err != nil || !rest {
			t.Fatalf("rest=%v err=%v", rest, err)
		}
		return got, c.t.Sub(start), seen
	}

	got, _, _ := run(newClock(2*time.Second), nil)
	if got.settles != 0 || fmt.Sprint(got.reads) != "[false]" {
		t.Fatalf("still screen: %+v, want one cached read and no settle", got)
	}

	moving := newClock(0)
	moving.moving = func() bool { return true } // the previous tap's ripple
	got, _, seen := run(moving, []bool{false, false})
	if got.settles != 2 || got.reads[0] || !got.reads[len(got.reads)-1] || seen {
		t.Fatalf("no transition: %+v seen=%v, want 2 settles, a cached then fresh reads", got, seen)
	}

	moving = newClock(0)
	moving.moving = func() bool { return true }
	got, took, seen := run(moving, []bool{true})
	if got.settles != 1 || !seen || took < restSpan {
		t.Fatalf("transition first: %+v seen=%v took %v, want 1 settle and at least restSpan", got, seen, took)
	}
	for _, f := range got.reads {
		if !f {
			t.Fatalf("transition first: a cached read %v", got.reads)
		}
	}

	moving = newClock(0)
	moving.moving = func() bool { return true }
	got, took, seen = run(moving, []bool{false, true})
	if got.settles != 2 || !seen || took < restSpan {
		t.Fatalf("transition during the reads: %+v seen=%v took %v, want the second settle to catch it", got, seen, took)
	}
}

func TestTapWaitEnv(t *testing.T) {
	for v, want := range map[string]bool{"": true, "1": true, "0": false, "x": true} {
		t.Setenv("AGEMU_ANDROID_TAP_WAIT", v)
		if got := tapWait(); got != want {
			t.Errorf("AGEMU_ANDROID_TAP_WAIT=%q: %v, want %v", v, got, want)
		}
	}
}

// A row that pauses for one frame and then moves one step more (the end of a scroll) is not at
// rest at the paused position: the frame must hold across two posted frames.
func TestAwaitRestNeedsTwoFrames(t *testing.T) {
	c := newClock(0)
	c.moving = func() bool { return true } // something else keeps animating
	pauseEnd := c.t.Add(25 * time.Millisecond)
	find, _ := reader(c, func(at time.Time) api.Rect {
		if at.Before(pauseEnd) {
			return api.Rect{X: 24, Y: 700, W: 1032, H: 150}
		}
		return api.Rect{X: 24, Y: 690, W: 1032, H: 150}
	})
	el, ok, err := awaitRest(context.Background(), c, find, 0, time.Second, c.now)
	if err != nil || !ok || el.Frame.Y != 690 {
		t.Fatalf("got y=%d ok=%v err=%v; want y=690 at rest", el.Frame.Y, ok, err)
	}
}

// Without a delivering stream (reopening after idle), two equal reads restGap apart count.
func TestAwaitRestWithoutTheStream(t *testing.T) {
	reads := 0
	find := func() (*api.Element, error) {
		reads++
		return &api.Element{Frame: api.Rect{X: 1, Y: 2, W: 3, H: 4}}, nil
	}
	start := time.Now()
	el, ok, err := awaitRest(context.Background(), nil, find, 0, time.Second, time.Now)
	if err != nil || !ok || el.Frame.Y != 2 {
		t.Fatalf("got %v, %v, %v", el, ok, err)
	}
	if reads != 2 || time.Since(start) < restGap {
		t.Fatalf("%d reads in %v, want 2 reads at least %v apart", reads, time.Since(start), restGap)
	}
}

func TestAwaitRestGivesUpAfterMax(t *testing.T) {
	c := newClock(0)
	c.moving = func() bool { return true }
	y := 0
	find := func() (*api.Element, error) {
		y += 10 // never stops
		c.t = c.t.Add(5 * time.Millisecond)
		return &api.Element{Frame: api.Rect{X: 0, Y: y, W: 10, H: 10}}, nil
	}
	start := c.t
	el, ok, err := awaitRest(context.Background(), c, find, 0, 300*time.Millisecond, c.now)
	if err != nil || ok {
		t.Fatalf("ok=%v err=%v, want a bounded give-up (ok false)", ok, err)
	}
	if el.Frame.Y != y {
		t.Fatalf("returned y=%d, want the latest read y=%d", el.Frame.Y, y)
	}
	if took := c.t.Sub(start); took > 300*time.Millisecond+readEvery {
		t.Fatalf("took %v, past the bound", took)
	}
}

func TestAwaitRestDoesNotReturnAnElementThatWentAway(t *testing.T) {
	c := newClock(0)
	c.moving = func() bool { return true }
	reads := 0
	find := func() (*api.Element, error) {
		reads++
		if reads > 1 {
			return nil, errors.New("no element matches")
		}
		return &api.Element{Frame: api.Rect{X: 0, Y: 0, W: 10, H: 10}}, nil
	}
	_, _, err := awaitRest(context.Background(), c, find, 0, time.Second, c.now)
	if err == nil || !strings.Contains(err.Error(), "disappeared while it was moving") {
		t.Fatalf("err = %v", err)
	}
	// Not found at all: the plain error.
	_, _, err = awaitRest(context.Background(), c, func() (*api.Element, error) { return nil, errors.New("no element matches") }, 0, time.Second, c.now)
	if err == nil || strings.Contains(err.Error(), "disappeared") {
		t.Fatalf("err = %v", err)
	}
}
