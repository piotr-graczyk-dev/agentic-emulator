package android

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/android/emupb"
)

// fakeEmu answers streamScreenshot like emulator 37.1 (see TestDeviceStreamReopenGap): a new
// subscription waits up to firstWait for a posted frame, then sends its first message (seq 0
// when none was posted, the number of posted frames otherwise); after that, one message per
// posted frame. Every message writes the screen into the stream's mapping first.
type fakeEmu struct {
	emupb.EmulatorControllerClient
	w, h      int
	firstWait time.Duration
	shm       *shm

	mu      sync.Mutex
	screen  byte          // the framebuffer: every byte of every pixel
	posted  int           // frames posted so far
	changed chan struct{} // closed on every post
	opens   int
	active  int
}

func newFakeEmu(t *testing.T, firstWait time.Duration) (*fakeEmu, *shm) {
	t.Helper()
	emu := &fakeEmu{w: 4, h: 4, firstWait: firstWait, changed: make(chan struct{})}
	s, err := openShm(filepath.Join(t.TempDir(), "fb.stream"), emu.w, emu.h)
	if err != nil {
		t.Fatal(err)
	}
	emu.shm = s
	return emu, s
}

// post makes the guest post a frame showing v.
func (f *fakeEmu) post(v byte) {
	f.mu.Lock()
	f.screen = v
	f.posted++
	close(f.changed)
	f.changed = make(chan struct{})
	f.mu.Unlock()
}

func (f *fakeEmu) StreamScreenshot(ctx context.Context, in *emupb.ImageFormat, _ ...grpc.CallOption) (grpc.ServerStreamingClient[emupb.Image], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opens++
	f.active++
	return &fakeSub{ctx: ctx, emu: f, base: f.posted, seen: f.posted, first: true}, nil
}

func (f *fakeEmu) state() (opens, active int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.opens, f.active
}

type fakeSub struct {
	grpc.ClientStream
	ctx        context.Context
	emu        *fakeEmu
	base, seen int // posted count when the subscription opened / at its last message
	first      bool
	done       bool
}

func (s *fakeSub) Recv() (*emupb.Image, error) {
	f := s.emu
	var timeout <-chan time.Time
	if s.first {
		timeout = time.After(f.firstWait)
	}
	f.mu.Lock()
	for f.posted == s.seen {
		ch := f.changed
		f.mu.Unlock()
		select {
		case <-s.ctx.Done():
			f.mu.Lock()
			if !s.done {
				s.done = true
				f.active--
			}
			f.mu.Unlock()
			return nil, s.ctx.Err()
		case <-ch:
			f.mu.Lock()
			continue
		case <-timeout:
			f.mu.Lock()
		}
		break
	}
	defer f.mu.Unlock()
	s.first = false
	s.seen = f.posted
	f.shm.mu.Lock()
	for i := range f.shm.data {
		f.shm.data[i] = f.screen
	}
	f.shm.mu.Unlock()
	return &emupb.Image{
		Format:      &emupb.ImageFormat{Width: uint32(f.w), Height: uint32(f.h)},
		Seq:         uint32(f.posted - s.base),
		TimestampUs: uint64(time.Now().UnixMicro()),
	}, nil
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// startTestStream starts a frame stream on the fake emulator with the given idle time; with
// alwaysStop the idle stop does not spare a static screen.
func startTestStream(t *testing.T, emu *fakeEmu, s *shm, idle time.Duration, alwaysStop bool) *frameStream {
	t.Helper()
	fs := startFrameStream(emu, s, "test", emu.w, emu.h, idle)
	if alwaysStop {
		fs.mu.Lock()
		fs.quiet = 0
		fs.mu.Unlock()
	}
	t.Cleanup(fs.Close)
	return fs
}

func waitStopped(t *testing.T, emu *fakeEmu) {
	t.Helper()
	waitFor(t, "the idle stop", func() bool { _, active := emu.state(); return active == 0 })
}

// shows reports what the frame shows (every byte is the same in the fake).
func shows(f frame) byte {
	if len(f.Pix) == 0 {
		return 0
	}
	return f.Pix[0]
}

// The first message of a subscription reopened by an input shows the screen from before the
// input (seq 0: no frame was posted while the emulator waited). It must not pass for the input's
// effect: a screenshot right after the input returns the input's frame, posted after that first
// message.
func TestFrameStreamReopenFirstFrameIsNotTheInputsEffect(t *testing.T) {
	emu, s := newFakeEmu(t, 100*time.Millisecond)
	emu.post(1)
	fs := startTestStream(t, emu, s, 600*time.Millisecond, true)
	waitFor(t, "a frame", func() bool { f, ok := fs.get(context.Background(), time.Time{}); return ok && shows(f) == 1 })
	waitStopped(t, emu)
	fs.mu.Lock()
	cached := fs.cache.Pix != nil
	fs.mu.Unlock()
	if cached {
		t.Fatal("frame copy kept after the subscription closed")
	}

	fs.touch() // device.conn, before the input is sent
	input := time.Now()
	go func() {
		time.Sleep(emu.firstWait + 60*time.Millisecond)
		emu.post(2) // the input's frame, after the first message
	}()
	f, ok := fs.get(context.Background(), input)
	if !ok {
		t.Fatal("no frame after the input")
	}
	if shows(f) != 2 {
		t.Fatalf("screenshot after the input shows %d (the reopened subscription's first frame), want the input's frame 2", shows(f))
	}
	if opens, active := emu.state(); opens != 2 || active != 1 {
		t.Fatalf("after reopening: %d opens, %d active; want 2, 1", opens, active)
	}
}

// When the input's frame is posted before the reopened subscription's first message, that
// message has seq 1 and shows it: it is served at once, without waiting reactWindow for a later
// frame.
func TestFrameStreamReopenPostedFirstFrame(t *testing.T) {
	emu, s := newFakeEmu(t, 100*time.Millisecond)
	emu.post(1)
	fs := startTestStream(t, emu, s, 600*time.Millisecond, true)
	waitFor(t, "a frame", func() bool { _, ok := fs.get(context.Background(), time.Time{}); return ok })
	waitStopped(t, emu)

	fs.touch()
	input := time.Now()
	go func() {
		time.Sleep(20 * time.Millisecond)
		emu.post(2)
	}()
	f, ok := fs.get(context.Background(), input)
	took := time.Since(input)
	if !ok || shows(f) != 2 {
		t.Fatalf("got %d (ok %v), want the input's frame 2", shows(f), ok)
	}
	if took >= reactWindow {
		t.Fatalf("took %v: a posted first frame was not taken as the input's effect", took)
	}
}

// An input that changes nothing: the screenshot returns the reopened subscription's first frame
// once reactWindow has passed, like with an open subscription.
func TestFrameStreamReopenInputWithoutEffect(t *testing.T) {
	emu, s := newFakeEmu(t, 100*time.Millisecond)
	emu.post(1)
	fs := startTestStream(t, emu, s, 600*time.Millisecond, true)
	waitFor(t, "a frame", func() bool { _, ok := fs.get(context.Background(), time.Time{}); return ok })
	waitStopped(t, emu)

	fs.touch()
	input := time.Now()
	f, ok := fs.get(context.Background(), input)
	took := time.Since(input)
	if !ok || shows(f) != 1 {
		t.Fatalf("got %d (ok %v), want the unchanged screen 1", shows(f), ok)
	}
	if took < reactWindow || took > reactWindow+200*time.Millisecond {
		t.Fatalf("took %v, want about reactWindow (%v)", took, reactWindow)
	}
}

// Without an input, a screenshot right after the idle stop reads the framebuffer (get reports
// false) and reopens the subscription, which serves the next one.
func TestFrameStreamReopenWithoutInput(t *testing.T) {
	emu, s := newFakeEmu(t, 100*time.Millisecond)
	emu.post(1)
	fs := startTestStream(t, emu, s, 600*time.Millisecond, true)
	waitFor(t, "a frame", func() bool { _, ok := fs.get(context.Background(), time.Time{}); return ok })
	waitStopped(t, emu)
	if _, ok := fs.get(context.Background(), time.Time{}); ok {
		t.Fatal("frame served from a closed subscription")
	}
	waitFor(t, "a frame from the reopened subscription", func() bool {
		f, ok := fs.get(context.Background(), time.Time{})
		return ok && shows(f) == 1
	})
	if opens, _ := emu.state(); opens != 2 {
		t.Fatalf("%d opens, want 2", opens)
	}
}

// The idle stop spares a screen that posts fewer than one frame per idleQuiet (here: none), and
// closes the subscription while the guest keeps posting frames.
func TestFrameStreamIdleStopOnlyWhilePosting(t *testing.T) {
	emu, s := newFakeEmu(t, 20*time.Millisecond)
	emu.post(1)
	fs := startTestStream(t, emu, s, 100*time.Millisecond, false)
	fs.mu.Lock()
	fs.quiet = 100 * time.Millisecond
	fs.mu.Unlock()
	waitFor(t, "a frame", func() bool { _, ok := fs.get(context.Background(), time.Time{}); return ok })

	time.Sleep(450 * time.Millisecond) // several idle checks on a static screen
	if _, active := emu.state(); active != 1 {
		t.Fatal("subscription closed on a static screen")
	}
	if f, ok := fs.get(context.Background(), time.Time{}); !ok || shows(f) != 1 {
		t.Fatal("no frame from the copy after idle on a static screen")
	}

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for v := byte(2); ; v++ {
			select {
			case <-stop:
				return
			case <-time.After(10 * time.Millisecond):
				emu.post(v)
			}
		}
	}()
	waitStopped(t, emu)
}

// postBurst makes the guest post n frames showing first, first+1, ..., waiting for each one's
// notification (the fake sends one message for all the posts its subscription has not sent yet).
func postBurst(t *testing.T, emu *fakeEmu, fs *frameStream, first byte, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		fs.mu.Lock()
		before := fs.notes
		fs.mu.Unlock()
		emu.post(first + byte(i))
		waitFor(t, "the frame's notification", func() bool {
			fs.mu.Lock()
			defer fs.mu.Unlock()
			return fs.notes > before
		})
	}
}

// startRateTest starts a stream with a 600 ms idle time and one frame per 60 ms as the rate
// that counts as changing: the idle check looks at the last 300 ms (half the idle time), so 5
// frames in it close the subscription.
func startRateTest(t *testing.T) (*fakeEmu, *frameStream) {
	t.Helper()
	emu, s := newFakeEmu(t, 20*time.Millisecond)
	emu.post(1)
	fs := startTestStream(t, emu, s, 600*time.Millisecond, false)
	fs.mu.Lock()
	fs.quiet = 60 * time.Millisecond
	fs.mu.Unlock()
	waitFor(t, "a frame", func() bool { _, ok := fs.get(context.Background(), time.Time{}); return ok })
	return emu, fs
}

// The agent loop "tap, screenshot, think for longer than the idle time": the tap's transition
// posts about 12 frames right after the last call, then the screen is static for the idle
// time. The idle check judges the recent frame rate, not the frames since the call, so the
// subscription stays open and the next screenshot is served from its copy. (Judged by the frames
// since the last call, 12 frames in 600 ms are one per 50 ms, and it closed.)
func TestFrameStreamIdleStopSparesBurstThenStatic(t *testing.T) {
	emu, fs := startRateTest(t)
	fs.touch() // the last call
	last := time.Now()
	postBurst(t, emu, fs, 2, 12)
	if took := time.Since(last); took > 250*time.Millisecond {
		t.Skipf("the burst took %v; the test needs it inside the first half of the idle time", took)
	}
	time.Sleep(1500 * time.Millisecond) // the idle check at 600 ms, then every 300 ms
	if opens, active := emu.state(); opens != 1 || active != 1 {
		t.Fatalf("%d opens, %d active after a burst and a static screen; want 1, 1 (the stream stays open)", opens, active)
	}
	if f, ok := fs.get(context.Background(), time.Time{}); !ok || shows(f) != 13 {
		t.Fatalf("got %d (ok %v), want the copy of the burst's last frame 13", shows(f), ok)
	}
}

// Frames that keep coming after the last call close the subscription once the idle time has
// passed, and not before.
func TestFrameStreamIdleStopWhileFramesKeepComing(t *testing.T) {
	emu, fs := startRateTest(t)
	stop := make(chan struct{})
	done := make(chan struct{})
	defer func() { close(stop); <-done }()
	fs.touch() // the last call
	last := time.Now()
	go func() {
		defer close(done)
		for v := byte(2); ; v++ {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
				emu.post(v)
			}
		}
	}()
	waitStopped(t, emu)
	if took := time.Since(last); took < 600*time.Millisecond {
		t.Fatalf("closed %v after the last call, before the idle time (600 ms)", took)
	}
}

func TestFrameStreamNoIdleStop(t *testing.T) {
	emu, s := newFakeEmu(t, 20*time.Millisecond)
	emu.post(1)
	fs := startTestStream(t, emu, s, 0, true)
	waitFor(t, "a frame", func() bool {
		_, ok := fs.get(context.Background(), time.Time{})
		return ok
	})
	for i := 0; i < 10; i++ {
		emu.post(byte(i + 2))
		time.Sleep(20 * time.Millisecond)
	}
	if opens, active := emu.state(); opens != 1 || active != 1 {
		t.Fatalf("%d opens, %d active; want 1, 1 (idle 0 never stops)", opens, active)
	}
}

func TestStreamIdleEnv(t *testing.T) {
	for _, c := range []struct {
		v    string
		want time.Duration
	}{
		{"", 30 * time.Second},
		{"0", 0},
		{"5", 5 * time.Second},
		{"-1", 30 * time.Second},
		{"x", 30 * time.Second},
	} {
		t.Setenv("AGEMU_ANDROID_STREAM_IDLE_S", c.v)
		if got := streamIdle(); got != c.want {
			t.Errorf("AGEMU_ANDROID_STREAM_IDLE_S=%q: %v, want %v", c.v, got, c.want)
		}
	}
}
