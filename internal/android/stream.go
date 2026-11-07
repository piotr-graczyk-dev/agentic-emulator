package android

import (
	"bytes"
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/android/emupb"
)

// frameStream keeps the latest frame of the emulator's display in host memory, so that a
// screenshot only crops, resamples and encodes.
//
// It holds one gRPC streamScreenshot subscription with the MMAP transport: for every frame
// the guest posts, the emulator writes the RGBA pixels into a shared file mapping and sends a
// small message (sequence number, generation time). Reading that frame back costs the
// emulator a few milliseconds, against 25-40 ms for a getScreenshot call.
//
// The emulator may overwrite the mapping with the next frame at any time, so a frame is only
// copied once the display has been quiet for settleDebounce, and the copy is checked against
// the mapping a second time: if any byte changed in between, the emulator was writing, and the
// copy is discarded. A copy is current while no newer frame notification has arrived.
//
// Idle stop (AGEMU_ANDROID_STREAM_IDLE_S). While the subscription is open, every frame the
// guest posts costs a readback in the emulator (gfxstream PostWorker::screenshot) and a 10 MB
// copy and compare here, whether or not anyone takes a screenshot: with the Clock app's running
// stopwatch on screen (about 38 fps) that was 12% of a core in the emulator's post worker and 4%
// in agemud. On a static screen no frame is posted and the stream costs nothing. So when the
// stream was not touched (a call to the device) for the idle time and the guest is still
// posting frames (at least one per idleQuiet over the last idleRecent, see postingLocked), the
// subscription is closed; the next call reopens it (touch). Frames posted right after the last
// call (the transition a tap started) do not count: on a screen that is static by the time of
// the check the stream stays open, so the next screenshot is still served from the copy.
//
// A reopened subscription (emulator 37.1, measured): the emulator starts listening for frames
// when the call arrives, then captures the first frame either right after the guest posts one
// (that message has seq >= 1) or after waiting 125 ms for one (seq 0). The first message
// therefore arrives 130-150 ms after the call on a static screen, sooner when frames are posted.
// A frame posted after the call is in the first message's pixels or notified after it; none was
// lost (TestDeviceStreamReopenGap). A seq-0 first frame is not a posted frame, so it counts as
// generated when the subscription was opened (before the input that reopened it): it must not
// pass for that input's effect. Until it arrives, a screenshot right after an input waits for
// it (reopenWait), and any other screenshot reads the framebuffer.
type frameStream struct {
	ctl    emupb.EmulatorControllerClient
	shm    *shm
	name   string
	fw, fh int           // framebuffer size; the emulator only writes MMAP stream frames when it is given
	idle   time.Duration // close the subscription after this long without touch (0: never)
	quiet  time.Duration // ... and only while the guest posts at least one frame per quiet (0: close anyway)

	mu        sync.Mutex
	changed   chan struct{} // closed and replaced on every state change
	healthy   bool          // subscription delivering
	notes     uint64        // frame notifications received
	lastNote  time.Time     // when the latest notification arrived
	lastGen   time.Time     // generation time of the latest posted frame
	w, h      int           // size of the latest posted frame (0: display off)
	cache     frame         // verified copy of a posted frame
	cacheN    uint64        // notes count the cache corresponds to (current iff == notes)
	cacheGen  time.Time     // generation time of the cached frame
	timer     *time.Timer
	ctx       context.Context // parent of every subscription; cancelled by Close
	cancel    context.CancelFunc
	sub       context.CancelFunc // open subscription (nil: closed after idle)
	gen       uint64             // subscription generation; frames of older ones are ignored
	opened    time.Time          // when the current subscription was opened
	used      time.Time          // last touch
	idleTimer *time.Timer
	// Arrival times of the latest posted frames, a ring (a seq-0 first frame is not one); the
	// idle check judges the guest's recent frame rate by them.
	postedAt  [recentPosts]time.Time
	postedI   int // next slot of postedAt
	capturing bool
	closed    bool
}

const (
	settleDebounce = 20 * time.Millisecond   // quiet time before a frame is copied
	reactWindow    = 300 * time.Millisecond  // how long a screenshot waits for an input's first frame
	settleCap      = 1500 * time.Millisecond // how long after an input a screenshot waits for the screen to settle
	reopenWait     = time.Second             // how long a screenshot after an input waits for a reopened subscription's first message (130-150 ms; up to 0.9 s at load 70)
	// A stream idle for longer than its idle time stays open while the guest posts fewer frames
	// than one per idleQuiet: each frame costs the emulator a readback and agemud a copy, so a
	// near-static screen (the status bar clock posts one a minute) costs almost nothing, and
	// keeping its copy current saves the first screenshot after the idle period a framebuffer
	// read.
	idleQuiet = 3 * time.Second
	// idleRecent is how far back the idle check measures the guest's frame rate (at most half
	// the idle time, so that the frames right after the last call never count).
	idleRecent = 10 * time.Second
	// recentPosts is the size of the ring of posted-frame times; it must exceed
	// idleRecent/idleQuiet, the number of frames that make a screen count as changing.
	recentPosts = 8
)

// streamIdle is how long the frame stream stays subscribed without a screenshot or an input
// (AGEMU_ANDROID_STREAM_IDLE_S, default 30 s; 0 keeps it subscribed for good).
func streamIdle() time.Duration {
	v := env("AGEMU_ANDROID_STREAM_IDLE_S", "30")
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 30 * time.Second
	}
	return time.Duration(n) * time.Second
}

func startFrameStream(ctl emupb.EmulatorControllerClient, s *shm, name string, w, h int, idle time.Duration) *frameStream {
	ctx, cancel := context.WithCancel(context.Background())
	fs := &frameStream{ctl: ctl, shm: s, name: name, fw: w, fh: h, idle: idle, quiet: idleQuiet, ctx: ctx, cancel: cancel, changed: make(chan struct{})}
	fs.touch()
	return fs
}

// touch marks the stream as in use, opening the subscription if it was closed after idle.
// Every call to the device does this before it sends input (device.conn).
func (fs *frameStream) touch() {
	if fs == nil {
		return
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.closed {
		return
	}
	fs.used = time.Now()
	if fs.sub == nil {
		fs.gen++
		ctx, cancel := context.WithCancel(fs.ctx)
		fs.sub, fs.opened = cancel, fs.used
		go fs.run(ctx, fs.gen)
	}
	if fs.idle > 0 && fs.idleTimer == nil {
		fs.idleTimer = time.AfterFunc(fs.idle, fs.idleCheck)
	}
}

// idleCheck closes the subscription when the stream was not touched for fs.idle and the guest
// is still posting frames (postingLocked); on a (nearly) static screen it checks again one
// rate window later. Frames posted while the subscription is closed are not seen, so the copy
// is dropped with it.
func (fs *frameStream) idleCheck() {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.closed {
		return
	}
	now := time.Now()
	if left := fs.idle - now.Sub(fs.used); left > 0 {
		fs.idleTimer.Reset(left)
		return
	}
	if window := min(idleRecent, fs.idle/2); fs.quiet > 0 && !fs.postingLocked(now, window) {
		fs.idleTimer.Reset(window)
		return
	}
	fs.idleTimer = nil
	if fs.sub != nil {
		fs.sub()
		fs.sub = nil
		fs.gen++
	}
	if fs.timer != nil {
		fs.timer.Stop()
	}
	fs.healthy = false
	fs.cache, fs.cacheN = frame{}, 0
	fs.signalLocked()
}

// postingLocked reports whether the guest posted frames at a rate of at least one per fs.quiet
// over the window before now. It judges the recent rate only: a burst of frames right after
// the last call (a screen transition) is older than the window by the time of the idle check,
// so a screen that has been static since keeps its subscription.
func (fs *frameStream) postingLocked(now time.Time, window time.Duration) bool {
	need := int((window + fs.quiet - 1) / fs.quiet)
	need = max(1, min(need, recentPosts))
	n := 0
	for _, t := range fs.postedAt {
		if !t.IsZero() && now.Sub(t) <= window {
			n++
		}
	}
	return n >= need
}

// Close stops the subscription and unmaps the frame file (after a running copy finished).
func (fs *frameStream) Close() {
	if fs == nil {
		return
	}
	fs.cancel()
	fs.mu.Lock()
	if fs.timer != nil {
		fs.timer.Stop()
	}
	if fs.idleTimer != nil {
		fs.idleTimer.Stop()
	}
	fs.closed = true
	fs.healthy = false
	fs.signalLocked()
	fs.mu.Unlock()
	fs.shm.Close()
}

// invalidate drops the cached frame. A snapshot load restores the display from the snapshot
// without the guest necessarily posting a new frame, so the cache must not survive it; until
// the next frame is captured, screenshots read the framebuffer directly.
func (fs *frameStream) invalidate() {
	if fs == nil {
		return
	}
	fs.mu.Lock()
	fs.cache, fs.cacheN = frame{}, 0
	fs.signalLocked()
	fs.mu.Unlock()
}

// signalLocked wakes everyone waiting for a state change.
func (fs *frameStream) signalLocked() {
	close(fs.changed)
	fs.changed = make(chan struct{})
}

// run holds subscription number gen until ctx is cancelled (Close, or the idle stop).
func (fs *frameStream) run(ctx context.Context, gen uint64) {
	backoff := 100 * time.Millisecond
	for ctx.Err() == nil {
		// Width/height must be set: with 0 x 0 (unscaled) the emulator acknowledges every frame
		// but never writes the pixels into the mapping (emulator 37.1).
		st, err := fs.ctl.StreamScreenshot(ctx, &emupb.ImageFormat{
			Format:    emupb.ImageFormat_RGBA8888,
			Width:     uint32(fs.fw),
			Height:    uint32(fs.fh),
			Transport: &emupb.ImageTransport{Channel: emupb.ImageTransport_MMAP, Handle: "file://" + fs.shm.path},
		})
		if err == nil {
			first := true
			for {
				img, rerr := st.Recv()
				if rerr != nil {
					break
				}
				backoff = 100 * time.Millisecond
				fs.onFrame(img, gen, first)
				first = false
			}
		}
		fs.mu.Lock()
		if fs.gen == gen {
			fs.healthy = false
			fs.signalLocked()
		}
		fs.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, 2*time.Second)
	}
}

// onFrame records a frame notification of subscription gen. first marks the subscription's
// first message. With seq 0 the emulator captured it without a frame being posted (after
// waiting for one): it shows the framebuffer, but it is no evidence of a frame after an input,
// so it counts as generated when the subscription was opened. (A retried call in the same
// subscription is clamped to that time too; a screenshot then waits at most reactWindow.)
func (fs *frameStream) onFrame(img *emupb.Image, gen uint64, first bool) {
	f := img.GetFormat()
	w, h := int(f.GetWidth()), int(f.GetHeight())
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.closed || gen != fs.gen {
		return
	}
	if len(img.GetImage()) > 0 || w*h*4 > fs.shm.size {
		// The emulator ignored the MMAP transport or the frame does not fit: not usable.
		fs.healthy = false
		fs.signalLocked()
		return
	}
	fs.healthy = true
	fs.notes++
	fs.lastNote = time.Now()
	fs.lastGen = time.UnixMicro(int64(img.GetTimestampUs()))
	if fs.lastGen.After(fs.lastNote) || img.GetTimestampUs() == 0 {
		fs.lastGen = fs.lastNote
	}
	if first && img.GetSeq() == 0 {
		if fs.lastGen.After(fs.opened) {
			fs.lastGen = fs.opened
		}
	} else {
		fs.postedAt[fs.postedI] = fs.lastNote
		fs.postedI = (fs.postedI + 1) % recentPosts
	}
	fs.w, fs.h = w, h
	if fs.timer == nil {
		fs.timer = time.AfterFunc(settleDebounce, fs.capture)
	} else {
		fs.timer.Reset(settleDebounce)
	}
	fs.signalLocked()
}

// capture copies the posted frame once the display has been quiet for settleDebounce.
func (fs *frameStream) capture() {
	fs.mu.Lock()
	n, w, h, gen, sub := fs.notes, fs.w, fs.h, fs.lastGen, fs.gen
	if fs.closed || fs.capturing || fs.sub == nil || n == fs.cacheN {
		fs.mu.Unlock()
		return
	}
	fs.capturing = true
	fs.mu.Unlock()

	var pix []byte
	ok := w > 0 && h > 0
	if ok {
		fs.shm.mu.Lock() // Close waits for the copy before unmapping
		if fs.shm.data == nil {
			ok = false
		} else {
			src := fs.shm.data[:w*h*4]
			pix = make([]byte, len(src))
			copy(pix, src)
			ok = bytes.Equal(pix, src) // unchanged during the copy: not torn
		}
		fs.shm.mu.Unlock()
	}

	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.capturing = false
	if fs.closed || fs.notes != n || fs.gen != sub {
		return // a newer frame arrived meanwhile (its own timer captures it), or the stream stopped
	}
	if !ok {
		if w > 0 && h > 0 {
			fs.timer.Reset(settleDebounce) // emulator was writing: try again
		}
		return
	}
	fs.cache, fs.cacheN, fs.cacheGen = frame{W: w, H: h, Pix: pix}, n, gen
	fs.signalLocked()
}

// get returns the latest posted frame when the stream has it, waiting for the effect of a
// recent input. It reports false when the caller should read the framebuffer directly: the
// stream is down, or the screen keeps changing (an animation), where a getScreenshot gives a
// consistent current frame.
//
//   - After an input (lastInput), get waits until the guest has posted a frame generated after
//     it, or until reactWindow has passed without one (the input changed nothing on screen);
//     then until the display has settled and that frame is copied, at most settleCap after the
//     input.
//   - Otherwise the copy is returned when no newer frame was posted. When frames were posted
//     after it, get waits for their copy, but returns false if they keep coming.
//   - A subscription stopped after idle is reopened. Without a recent input this screenshot
//     reads the framebuffer: a framebuffer read is current, and the subscription's first message
//     takes up to about 150 ms. After an input (which reopened it before it was sent), get waits
//     up to reopenWait for the first message, then as above.
func (fs *frameStream) get(ctx context.Context, lastInput time.Time) (frame, bool) {
	if fs == nil {
		return frame{}, false
	}
	fs.touch()
	var quietDeadline time.Time
	var quietNotes uint64
	for {
		fs.mu.Lock()
		now := time.Now()
		sinceInput := time.Duration(1 << 62)
		if !lastInput.IsZero() {
			sinceInput = now.Sub(lastInput)
		}
		if !fs.healthy && fs.sub != nil && sinceInput < settleCap && now.Sub(fs.opened) < reopenWait {
			// The input reopened the subscription (see device.conn): a framebuffer read now could
			// predate the input's effect, so wait for the stream as an open one would be waited for.
			ch, until := fs.changed, fs.opened.Add(reopenWait)
			fs.mu.Unlock()
			t := time.NewTimer(until.Sub(now))
			select {
			case <-ctx.Done():
				t.Stop()
				return frame{}, false
			case <-ch:
			case <-t.C:
			}
			t.Stop()
			continue
		}
		if !fs.healthy || fs.notes == 0 || fs.w == 0 {
			fs.mu.Unlock()
			return frame{}, false
		}
		current := fs.cache.Pix != nil && fs.cacheN == fs.notes
		framesAfterInput := fs.lastGen.After(lastInput)
		var until time.Time
		waitingFirst := false
		switch {
		case current && (framesAfterInput || sinceInput >= reactWindow):
			f := fs.cache
			fs.mu.Unlock()
			return f, true
		case !framesAfterInput && sinceInput < reactWindow:
			until, waitingFirst = lastInput.Add(reactWindow), true
		case framesAfterInput && sinceInput < settleCap:
			until = lastInput.Add(settleCap)
		default:
			if quietDeadline.IsZero() {
				quietDeadline, quietNotes = fs.lastNote.Add(settleDebounce+40*time.Millisecond), fs.notes
			} else if fs.notes != quietNotes {
				fs.mu.Unlock()
				return frame{}, false // still changing
			}
			until = quietDeadline
		}
		ch := fs.changed
		fs.mu.Unlock()
		if !now.Before(until) {
			if waitingFirst {
				continue // reactWindow over: re-evaluate without waiting for the input
			}
			return frame{}, false
		}
		t := time.NewTimer(until.Sub(now))
		select {
		case <-ctx.Done():
			t.Stop()
			return frame{}, false
		case <-ch:
		case <-t.C:
		}
		t.Stop()
	}
}
