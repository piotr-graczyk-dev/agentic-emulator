package android

import (
	"bytes"
	"context"
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
type frameStream struct {
	ctl    emupb.EmulatorControllerClient
	shm    *shm
	name   string
	fw, fh int // framebuffer size; the emulator only writes MMAP stream frames when it is given

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
	cancel    context.CancelFunc
	capturing bool
	closed    bool
}

const (
	settleDebounce = 20 * time.Millisecond   // quiet time before a frame is copied
	reactWindow    = 300 * time.Millisecond  // how long a screenshot waits for an input's first frame
	settleCap      = 1500 * time.Millisecond // how long after an input a screenshot waits for the screen to settle
)

func startFrameStream(ctl emupb.EmulatorControllerClient, s *shm, name string, w, h int) *frameStream {
	ctx, cancel := context.WithCancel(context.Background())
	fs := &frameStream{ctl: ctl, shm: s, name: name, fw: w, fh: h, cancel: cancel, changed: make(chan struct{})}
	go fs.run(ctx)
	return fs
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

func (fs *frameStream) run(ctx context.Context) {
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
			for {
				img, rerr := st.Recv()
				if rerr != nil {
					break
				}
				backoff = 100 * time.Millisecond
				fs.onFrame(img)
			}
		}
		fs.mu.Lock()
		fs.healthy = false
		fs.signalLocked()
		fs.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, 2*time.Second)
	}
}

func (fs *frameStream) onFrame(img *emupb.Image) {
	f := img.GetFormat()
	w, h := int(f.GetWidth()), int(f.GetHeight())
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.closed {
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
	n, w, h, gen := fs.notes, fs.w, fs.h, fs.lastGen
	if fs.closed || fs.capturing || n == fs.cacheN {
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
	if fs.closed || fs.notes != n {
		return // a newer frame arrived meanwhile; its own timer captures it
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
func (fs *frameStream) get(ctx context.Context, lastInput time.Time) (frame, bool) {
	if fs == nil {
		return frame{}, false
	}
	var quietDeadline time.Time
	var quietNotes uint64
	for {
		fs.mu.Lock()
		if !fs.healthy || fs.notes == 0 || fs.w == 0 {
			fs.mu.Unlock()
			return frame{}, false
		}
		now := time.Now()
		current := fs.cache.Pix != nil && fs.cacheN == fs.notes
		sinceInput := time.Duration(1 << 62)
		if !lastInput.IsZero() {
			sinceInput = now.Sub(lastInput)
		}
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
