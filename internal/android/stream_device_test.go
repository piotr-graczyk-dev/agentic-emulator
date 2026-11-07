//go:build devicetest

// Device tests of the frame stream's idle stop against a running agemu-* emulator (not part of
// `go test ./...`):
//
//	AGEMU_TEST_AVD=agemu-pixel go test -tags 'noios devicetest' -run TestDeviceStreamReopenGap -v ./internal/android/
package android

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/android/emupb"
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/api"
)

func hash12(p []byte) string {
	s := sha256.Sum256(p)
	return hex.EncodeToString(s[:6])
}

// note is one streamScreenshot message as a client saw it.
type note struct {
	at  time.Time // arrival
	gen time.Time // the message's generation timestamp
	seq uint32
	pix []byte // MMAP contents right after the message (nil: not copied)
}

// subscribe opens a streamScreenshot subscription with the MMAP transport into its own file
// and delivers every message, with a stable copy of the mapping when copyPix is set.
func subscribe(ctx context.Context, t *testing.T, e *emuClient, w, h int, copyPix bool) (<-chan note, time.Time) {
	t.Helper()
	s, err := openShm(filepath.Join(t.TempDir(), "sub"), w, h)
	if err != nil {
		t.Fatal(err)
	}
	ch := make(chan note, 4096)
	t0 := time.Now()
	st, err := e.ctl.StreamScreenshot(ctx, &emupb.ImageFormat{
		Format: emupb.ImageFormat_RGBA8888, Width: uint32(w), Height: uint32(h),
		Transport: &emupb.ImageTransport{Channel: emupb.ImageTransport_MMAP, Handle: "file://" + s.path},
	})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		defer s.Close()
		defer close(ch)
		for {
			img, err := st.Recv()
			if err != nil {
				return
			}
			n := note{at: time.Now(), gen: time.UnixMicro(int64(img.GetTimestampUs())), seq: img.GetSeq()}
			if copyPix {
				// A copy is good when the mapping did not change while it was taken.
				for try := 0; try < 20; try++ {
					p := append([]byte(nil), s.data[:w*h*4]...)
					if bytes.Equal(p, s.data[:w*h*4]) {
						n.pix = p
						break
					}
					time.Sleep(2 * time.Millisecond)
				}
			}
			ch <- n
		}
	}()
	return ch, t0
}

// TestDeviceStreamReopenGap checks the emulator behaviour the idle stop relies on: a frame the
// guest posts after a streamScreenshot call but before that subscription's first message (the
// emulator takes 100-200 ms to send it) is either already in the first message's pixels or
// notified afterwards. If the emulator captured the first frame before it started listening for
// frames, such a frame would be lost, and a screenshot after an input sent in that gap could
// return the pre-input pixels.
//
// Put a screen up that posts single frames at a low rate (a blinking text cursor: one frame per
// 500 ms; the launcher clock: one per second), so that many subscriptions have one frame in the
// gap and none right after it. A control subscription, open throughout, records when frames are
// posted. For every new subscription, the first message's pixels are compared with a direct
// framebuffer read taken right after it: if they differ, a frame was posted after the first
// frame was captured, and the new subscription must notify it.
func TestDeviceStreamReopenGap(t *testing.T) {
	avd := env("AGEMU_TEST_AVD", "agemu-pixel")
	r, ok := findRunning(avd)
	if !ok {
		t.Skipf("%s not running", avd)
	}
	e, err := dialEmu(r)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	a, _ := loadAVD(avd)
	W, H := a.intCfg("hw.lcd.width"), a.intCfg("hw.lcd.height")
	fb, err := openShm(filepath.Join(t.TempDir(), "fb"), W, H)
	if err != nil {
		t.Fatal(err)
	}
	e.fb = fb
	ctx := context.Background()

	cctx, ccancel := context.WithCancel(ctx)
	defer ccancel()
	control, _ := subscribe(cctx, t, e, W, H, false)
	var cmu sync.Mutex
	var posted []time.Time // arrival of the control subscription's messages
	go func() {
		for n := range control {
			cmu.Lock()
			posted = append(posted, n.at)
			cmu.Unlock()
		}
	}()
	time.Sleep(time.Second)
	postedIn := func(from, to time.Time) int {
		cmu.Lock()
		defer cmu.Unlock()
		k := 0
		for _, p := range posted {
			if p.After(from) && !p.After(to) {
				k++
			}
		}
		return k
	}
	cmu.Lock()
	rate := len(posted)
	cmu.Unlock()
	t.Logf("control: %d messages in the first second", rate)

	var firsts []time.Duration
	inGap, differ, notified, lost := 0, 0, 0, 0
	for i := 0; i < envInt("AGEMU_TEST_N", 60); i++ {
		sctx, scancel := context.WithCancel(ctx)
		ch, t0 := subscribe(sctx, t, e, W, H, true)
		var first note
		select {
		case first = <-ch:
		case <-time.After(3 * time.Second):
			scancel()
			t.Fatalf("%d: no first message", i)
		}
		d, err := e.screenshotMMAP(ctx)
		tD := time.Now()
		if err != nil {
			t.Fatal(err)
		}
		// Collect what the new subscription sends next (a frame posted before tD is notified
		// within a readback, well under 400 ms).
		var later []note
		deadline := time.After(400 * time.Millisecond)
	collect:
		for {
			select {
			case n := <-ch:
				later = append(later, n)
			case <-deadline:
				break collect
			}
		}
		scancel()
		lat := first.at.Sub(t0)
		firsts = append(firsts, lat)
		gap := postedIn(t0, first.at)
		if gap > 0 {
			inGap++
		}
		same := first.pix != nil && bytes.Equal(first.pix, d.Pix)
		laterIn := 0
		for _, n := range later {
			if n.at.Before(tD.Add(400 * time.Millisecond)) {
				laterIn++
			}
		}
		verdict := "first frame current"
		switch {
		case same:
		case laterIn > 0:
			differ++
			notified++
			verdict = "changed after the first frame; notified"
		default:
			differ++
			lost++
			verdict = "LOST: changed after the first frame and never notified"
			var buf bytes.Buffer
			_ = encodePNG(&buf, rgbaImage{W: W, H: H, Pix: first.pix})
			_ = os.WriteFile(filepath.Join(os.TempDir(), fmt.Sprintf("agemu-gap-first-%d.png", i)), buf.Bytes(), 0o644)
		}
		seqs := []uint32{first.seq}
		for _, n := range later {
			seqs = append(seqs, n.seq)
		}
		t.Logf("%2d: first message +%v (gen %+v), control frames in the gap %d, later messages %d, seqs %v: %s",
			i, lat.Round(time.Millisecond), first.gen.Sub(t0).Round(time.Millisecond), gap, len(later), seqs, verdict)
		if lost > 0 && verdict[0] == 'L' {
			t.Errorf("%d: a frame posted after the first frame was captured was not notified", i)
		}
		// Let the cursor/clock phase drift against the next call.
		time.Sleep(time.Duration(37*(i%7)) * time.Millisecond)
	}
	t.Logf("first message latency: median %v, min %v, max %v (n=%d)", median(append([]time.Duration(nil), firsts...)),
		minDur(firsts), maxDur(firsts), len(firsts))
	t.Logf("subscriptions with a control frame in the gap %d; first frame differed from the screen after it %d: notified %d, lost %d",
		inGap, differ, notified, lost)
}

func minDur(ds []time.Duration) time.Duration {
	m := ds[0]
	for _, d := range ds {
		m = min(m, d)
	}
	return m
}

func maxDur(ds []time.Duration) time.Duration {
	m := ds[0]
	for _, d := range ds {
		m = max(m, d)
	}
	return m
}

// procStats returns the emulator's thread count (ps -M) and phys_footprint in MB (footprint).
func procStats(pid int) (threads int, footprintMB float64) {
	if out, err := exec.Command("ps", "-M", "-p", strconv.Itoa(pid)).Output(); err == nil {
		threads = len(strings.Split(strings.TrimSpace(string(out)), "\n")) - 1
	}
	if out, err := exec.Command("footprint", "-p", strconv.Itoa(pid)).Output(); err == nil {
		for _, l := range strings.Split(string(out), "\n") {
			// "qemu-system-aarch64-headless [1234]: 64-bit    Footprint: 4962 MB (16384 bytes per page)"
			if i := strings.Index(l, "Footprint: "); i >= 0 {
				f := strings.Fields(l[i+len("Footprint: "):])
				if len(f) >= 2 {
					v, _ := strconv.ParseFloat(f[0], 64)
					switch f[1] {
					case "KB":
						v /= 1024
					case "GB":
						v *= 1024
					}
					footprintMB = v
				}
				break
			}
		}
	}
	return
}

// TestDeviceStreamSoak closes and reopens agemu's frame stream (the production frameStream,
// with a 200 ms idle time) AGEMU_TEST_N times (default 1000), and logs the emulator's thread
// count and memory footprint every 100 cycles.
func TestDeviceStreamSoak(t *testing.T) {
	avd := env("AGEMU_TEST_AVD", "agemu-pixel")
	r, ok := findRunning(avd)
	if !ok {
		t.Skipf("%s not running", avd)
	}
	e, err := dialEmu(r)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	a, _ := loadAVD(avd)
	W, H := a.intCfg("hw.lcd.width"), a.intCfg("hw.lcd.height")
	s, err := openShm(filepath.Join(t.TempDir(), "fb.stream"), W, H)
	if err != nil {
		t.Fatal(err)
	}
	// The idle time must outlast the first message (130-150 ms), or the subscription closes
	// before it delivers; the loop below keeps touching until it has.
	fs := startFrameStream(e.ctl, s, avd, W, H, 200*time.Millisecond)
	defer fs.Close()
	fs.mu.Lock()
	fs.quiet = 0 // stop on a static screen too
	fs.mu.Unlock()
	state := func() (healthy, open bool, ch chan struct{}) {
		fs.mu.Lock()
		defer fs.mu.Unlock()
		return fs.healthy, fs.sub != nil, fs.changed
	}
	waitState := func(what string, cond func(healthy, open bool) bool) {
		deadline := time.After(5 * time.Second)
		for {
			h, o, ch := state()
			if cond(h, o) {
				return
			}
			select {
			case <-ch:
			case <-time.After(20 * time.Millisecond):
			case <-deadline:
				t.Fatalf("timed out waiting for %s", what)
			}
		}
	}
	th0, fp0 := procStats(r.PID)
	t.Logf("cycle 0: emulator threads %d, footprint %.0f MB", th0, fp0)
	var firsts []time.Duration
	n := envInt("AGEMU_TEST_N", 1000)
	start := time.Now()
	for i := 1; i <= n; i++ {
		waitState("the idle stop", func(_, open bool) bool { return !open })
		fs.touch()
		t0 := time.Now()
		waitState("the first frame", func(healthy, _ bool) bool {
			if !healthy {
				fs.touch()
			}
			return healthy
		})
		firsts = append(firsts, time.Since(t0))
		if i%100 == 0 {
			th, fp := procStats(r.PID)
			t.Logf("cycle %d (%v): emulator threads %d, footprint %.0f MB, first frame median of the last 100 %v",
				i, time.Since(start).Round(time.Second), th, fp, median(append([]time.Duration(nil), firsts[len(firsts)-100:]...)))
		}
	}
	waitState("the last idle stop", func(_, open bool) bool { return !open })
	time.Sleep(2 * time.Second)
	th1, fp1 := procStats(r.PID)
	t.Logf("after %d cycles: emulator threads %d -> %d, footprint %.0f -> %.0f MB; first frame median %v, max %v",
		n, th0, th1, fp0, fp1, median(append([]time.Duration(nil), firsts...)), maxDur(firsts))
}

// TestDeviceStreamReopenFresh drives Settings through the backend with the idle stop at 1 s:
// before every input it waits until the subscription is closed, then sends the input (which
// reopens it) and takes a screenshot at once, the way an agent's next call would. Every other
// round, a screenshot reopens the stream first and the input follows AGEMU_TEST_DELAY_MS
// (default: cycling 0-250 ms) later, while the subscription is still waiting for its first
// frame. The screenshot must not show the screen from before the input: it is compared with
// direct framebuffer reads taken before the input and after the screen settled.
//
// AGEMU_TEST_NO_STOP=1 is the control: the same rounds with AGEMU_ANDROID_STREAM_IDLE_S=0, so
// the subscription stays open throughout (as without the idle stop), however long a round takes.
func TestDeviceStreamReopenFresh(t *testing.T) {
	avd := env("AGEMU_TEST_AVD", "agemu-pixel")
	if _, ok := findRunning(avd); !ok {
		t.Skipf("%s not running", avd)
	}
	noStop := env("AGEMU_TEST_NO_STOP", "0") == "1"
	if noStop {
		t.Setenv("AGEMU_ANDROID_STREAM_IDLE_S", "0")
	} else {
		t.Setenv("AGEMU_ANDROID_STREAM_IDLE_S", env("AGEMU_ANDROID_STREAM_IDLE_S", "1"))
	}
	ctx := context.Background()
	b := New()
	defer b.Close()
	id := "android:" + avd
	d, _ := b.devFor(id)
	e, _, err := d.conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case e.stream == nil:
		t.Fatal("no frame stream")
	case noStop && e.stream.idle != 0:
		t.Fatalf("control: the frame stream has an idle stop (%v)", e.stream.idle)
	case !noStop && e.stream.idle == 0:
		t.Fatal("frame stream without an idle stop")
	}
	e.stream.mu.Lock()
	e.stream.quiet = 0 // stop on a static screen too
	e.stream.mu.Unlock()
	out := env("AGEMU_TEST_OUT", "")
	direct := func() frame {
		f, err := e.screenshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return frame{W: f.W, H: f.H, Pix: append([]byte(nil), f.Pix...)}
	}
	// settled reads the framebuffer until two reads 300 ms apart are equal.
	settled := func() frame {
		prev := direct()
		for k := 0; k < 20; k++ {
			time.Sleep(300 * time.Millisecond)
			cur := direct()
			if bytes.Equal(cur.Pix, prev.Pix) {
				return cur
			}
			prev = cur
		}
		t.Fatal("screen never settled")
		return frame{}
	}
	find := func(text string) *api.Element {
		tr, err := b.UITree(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		var hit *api.Element
		tr.Walk(func(el *api.Element) {
			if hit == nil && el.Text == text && el.Frame.W > 0 {
				hit = el
			}
		})
		return hit
	}
	waitText := func(text string) {
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			if find(text) != nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("%q did not appear", text)
	}
	waitIdleStop := func() {
		if noStop {
			return // the control: the stream never stops
		}
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			e.stream.mu.Lock()
			open := e.stream.sub != nil
			e.stream.mu.Unlock()
			if !open {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("the stream did not stop after idle")
	}
	shot := func() (frame, bool) {
		var last time.Time
		if ns := d.lastInput.Load(); ns != 0 {
			last = time.Unix(0, ns)
		}
		f, ok := e.stream.get(ctx, last)
		if !ok {
			return direct(), false
		}
		return f, true
	}
	save := func(name string, f frame) {
		if out == "" {
			return
		}
		var buf bytes.Buffer
		_ = encodePNG(&buf, rgbaImage{W: f.W, H: f.H, Pix: f.Pix})
		_ = os.WriteFile(filepath.Join(out, name), buf.Bytes(), 0o644)
	}

	_ = b.Terminate(ctx, id, "com.android.settings")
	if err := b.Launch(ctx, id, "com.android.settings"); err != nil {
		t.Fatal(err)
	}
	waitText("Network & internet")
	stale, fresh, looksOld, unchanged, fromStream := 0, 0, 0, 0, 0
	delays := []int{0, 20, 50, 80, 120, 160, 200, 250}
	for i := 0; i < envInt("AGEMU_TEST_N", 16); i++ {
		forward := i%2 == 0
		target := "Network & internet"
		if !forward {
			target = "Internet"
		}
		var hit *api.Element
		if forward {
			hit = find(target)
			if hit == nil {
				t.Fatalf("%d: no %q", i, target)
			}
		}
		before := settled()
		waitIdleStop()
		mode := "input reopens"
		delay := -1
		if i%4 >= 2 {
			// A screenshot reopens the stream; the input comes while it waits for its first frame.
			delay = envInt("AGEMU_TEST_DELAY_MS", delays[(i/4)%len(delays)])
			mode = fmt.Sprintf("screenshot reopens, input +%dms", delay)
			_, _ = shot()
			time.Sleep(time.Duration(delay) * time.Millisecond)
		}
		t0 := time.Now()
		if forward {
			x, y := hit.Frame.Center()
			if err := b.Tap(ctx, id, x, y); err != nil {
				t.Fatal(err)
			}
		} else if err := b.Key(ctx, id, "back"); err != nil {
			t.Fatal(err)
		}
		s, ok := shot()
		took := time.Since(t0)
		// The generation time of the returned frame (when it came from the stream's copy),
		// relative to the input.
		genVsInput := "n/a (framebuffer read)"
		var genAfter bool
		if ok {
			fromStream++
			e.stream.mu.Lock()
			if len(e.stream.cache.Pix) > 0 && len(s.Pix) > 0 && &e.stream.cache.Pix[0] == &s.Pix[0] {
				in := time.Unix(0, d.lastInput.Load())
				genAfter = e.stream.cacheGen.After(in)
				genVsInput = fmt.Sprintf("gen %+v after the input", e.stream.cacheGen.Sub(in).Round(time.Millisecond))
			}
			e.stream.mu.Unlock()
		}
		if forward {
			waitText("Internet")
		} else {
			waitText("Network & internet")
		}
		after := settled()
		hs, hb, ha := hash12(s.Pix), hash12(before.Pix), hash12(after.Pix)
		verdict := ""
		switch {
		case bytes.Equal(before.Pix, after.Pix):
			unchanged++
			verdict = "input changed nothing; inconclusive"
		case bytes.Equal(s.Pix, before.Pix) && genAfter:
			looksOld++
			verdict = "looks unchanged: a frame posted after the input that still shows the old screen"
		case bytes.Equal(s.Pix, before.Pix):
			stale++
			verdict = "STALE: the screen from before the input"
			t.Errorf("%d: %s", i, verdict)
			save(fmt.Sprintf("stale-%02d-shot.png", i), s)
			save(fmt.Sprintf("stale-%02d-after.png", i), after)
		case bytes.Equal(s.Pix, after.Pix):
			fresh++
			verdict = "fresh: the settled screen"
		default:
			fresh++
			verdict = "fresh: a frame after the input, before the screen settled"
		}
		if i == 0 || i == 2 { // one round of each mode
			save(fmt.Sprintf("round-%02d-before.png", i), before)
			save(fmt.Sprintf("round-%02d-shot.png", i), s)
		}
		t.Logf("%2d: %s, %s: screenshot %v (stream=%v, %s) shot=%s before=%s after=%s: %s",
			i, map[bool]string{true: "tap " + target, false: "key back"}[forward], mode, took.Round(time.Millisecond), ok, genVsInput, hs, hb, ha, verdict)
	}
	t.Logf("fresh %d, looks unchanged %d, stale %d, inconclusive %d; from the stream %d", fresh, looksOld, stale, unchanged, fromStream)
}
