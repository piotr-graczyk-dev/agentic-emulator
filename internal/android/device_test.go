//go:build devicetest

// Device microbenchmarks against a running agemu-* emulator (not part of `go test ./...`):
//
//	AGEMU_TEST_AVD=agemu-pixel go test -tags 'noios devicetest' -run TestDeviceScreenshotPaths -v ./internal/android/
package android

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/android/emupb"
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/api"
)

func median(ds []time.Duration) time.Duration {
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	return ds[len(ds)/2]
}

func TestDeviceScreenshotPaths(t *testing.T) {
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
	path := filepath.Join(t.TempDir(), "fb")
	s, err := openShm(path, a.intCfg("hw.lcd.width"), a.intCfg("hw.lcd.height"))
	if err != nil {
		t.Fatal(err)
	}
	e.fb = s
	ctx := context.Background()
	var grpcT, mmapT, pngT []time.Duration
	for i := 0; i < 15; i++ {
		t0 := time.Now()
		f1, err := e.screenshotBytes(ctx)
		if err != nil {
			t.Fatal(err)
		}
		grpcT = append(grpcT, time.Since(t0))
		t0 = time.Now()
		f2, err := e.screenshotMMAP(ctx)
		if err != nil {
			t.Fatal(err)
		}
		mmapT = append(mmapT, time.Since(t0))
		if f1.W != f2.W || f1.H != f2.H {
			t.Fatalf("size mismatch %dx%d vs %dx%d", f1.W, f1.H, f2.W, f2.H)
		}
		t0 = time.Now()
		var buf bytes.Buffer
		_ = encodePNG(&buf, rgbaImage{W: f2.W, H: f2.H, Pix: f2.Pix})
		pngT = append(pngT, time.Since(t0))
		if i == 0 {
			_ = os.WriteFile(filepath.Join(os.TempDir(), "agemu-mmap-check.png"), buf.Bytes(), 0o644)
		}
	}
	t.Logf("getScreenshot bytes: median %v | mmap: median %v | png encode: median %v", median(grpcT), median(mmapT), median(pngT))
}

// TestDeviceStream measures streamScreenshot: frames delivered on an idle screen, and the
// latency from a touch to the first frame that follows it.
func TestDeviceStream(t *testing.T) {
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
	for _, mode := range []string{"mmap", "bytes"} {
		ctx, cancel := context.WithCancel(context.Background())
		f := &emupb.ImageFormat{Format: emupb.ImageFormat_RGBA8888}
		if mode == "mmap" {
			path := filepath.Join(t.TempDir(), "fb")
			s, err := openShm(path, W, H)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			f.Transport = &emupb.ImageTransport{Channel: emupb.ImageTransport_MMAP, Handle: "file://" + path}
		}
		st, err := e.ctl.StreamScreenshot(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		type ev struct {
			at  time.Time
			seq uint32
			ts  uint64
			n   int
		}
		ch := make(chan ev, 1000)
		go func() {
			for {
				img, err := st.Recv()
				if err != nil {
					close(ch)
					return
				}
				ch <- ev{time.Now(), img.GetSeq(), img.GetTimestampUs(), len(img.GetImage())}
			}
		}()
		// idle
		t0 := time.Now()
		n := 0
		idle := time.After(3 * time.Second)
	loop:
		for {
			select {
			case x := <-ch:
				if n < 3 {
					t.Logf("%s: frame seq %d at +%v, %d bytes, gen lag %v", mode, x.seq, x.at.Sub(t0), x.n, x.at.Sub(time.UnixMicro(int64(x.ts))))
				}
				n++
			case <-idle:
				break loop
			}
		}
		t.Logf("%s: %d frames in 3s idle", mode, n)
		// tap an empty spot on the home screen? we tap the center (may change things)
		for i := 0; i < 5; i++ {
			tap := time.Now()
			_ = e.touch(ctx, 800, 1100, 1)
			_ = e.touch(ctx, 800, 1100, 0)
			timeout := time.After(1500 * time.Millisecond)
			cnt := 0
			var first, last time.Time
		wait:
			for {
				select {
				case x := <-ch:
					if first.IsZero() {
						first = x.at
						t.Logf("%s: tap %d: first frame +%v (gen +%v)", mode, i, x.at.Sub(tap), time.UnixMicro(int64(x.ts)).Sub(tap))
					}
					last = x.at
					cnt++
				case <-timeout:
					break wait
				}
			}
			if cnt > 0 {
				t.Logf("%s: tap %d: %d frames, last +%v", mode, i, cnt, last.Sub(tap))
			} else {
				t.Logf("%s: tap %d: no frame", mode, i)
			}
		}
		cancel()
	}
}

func TestDeviceResize(t *testing.T) {
	src := rgbaImage{W: 1080, H: 2400, Pix: make([]byte, 1080*2400*4)}
	for i := range src.Pix {
		src.Pix[i] = byte(i * 7)
	}
	var ds, ps []time.Duration
	for i := 0; i < 10; i++ {
		t0 := time.Now()
		out := resize(src, 720, 1600)
		ds = append(ds, time.Since(t0))
		t0 = time.Now()
		var buf bytes.Buffer
		_ = encodePNG(&buf, out)
		ps = append(ps, time.Since(t0))
	}
	t.Logf("resize 1080x2400->720x1600 median %v, png 720x1600 median %v", median(ds), median(ps))
}

// TestDeviceResetBreakdown times the phases of an in-place baseline restore.
func TestDeviceResetBreakdown(t *testing.T) {
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
	ctx := context.Background()
	serial := r.serial()
	a := &agentClient{serial: serial}
	for i := 0; i < envInt("AGEMU_TEST_N", 3); i++ {
		a.Close()
		t0 := time.Now()
		if err := e.loadSnapshot(ctx, baselineSnapshot); err != nil {
			t.Fatal(err)
		}
		t1 := time.Now()
		var t2, t3 time.Time
		var st string
		for time.Since(t1) < 60*time.Second {
			c, cancel := context.WithTimeout(ctx, 2*time.Second)
			s, err := adbState(c, serial)
			cancel()
			if err == nil && s == "device" {
				if t2.IsZero() {
					t2 = time.Now()
				}
				c, cancel := context.WithTimeout(ctx, time.Second)
				_, err := a.ping(c)
				cancel()
				if err == nil {
					t3 = time.Now()
					break
				}
			} else if err == nil {
				st = s
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Logf("reset %d: loadSnapshot %v | adb 'device' +%v (last other state %q) | agent ping +%v | total %v",
			i, t1.Sub(t0).Round(time.Millisecond), t2.Sub(t1).Round(time.Millisecond), st, t3.Sub(t2).Round(time.Millisecond), t3.Sub(t0).Round(time.Millisecond))
		time.Sleep(2 * time.Second)
	}
}

// TestDeviceTinyStream: a downscaled screenshot stream as a cheap frame-posted signal.
func TestDeviceTinyStream(t *testing.T) {
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
	w := uint32(envInt("AGEMU_TEST_W", 54))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st, err := e.ctl.StreamScreenshot(ctx, &emupb.ImageFormat{Format: emupb.ImageFormat_RGBA8888, Width: w})
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	var last uint64
	go func() {
		time.Sleep(5 * time.Second)
		t.Logf("--- touch at +%v", time.Since(t0))
		_ = e.touch(ctx, 540, 2200, 1)
		_ = e.touch(ctx, 540, 2200, 0)
		time.Sleep(4 * time.Second)
		cancel()
	}()
	for {
		img, err := st.Recv()
		if err != nil {
			break
		}
		ts := img.GetTimestampUs()
		t.Logf("+%7.1fms seq %d %dx%d %dB gen %+.1fms lag %.1fms", float64(time.Since(t0).Microseconds())/1000, img.GetSeq(), img.GetFormat().GetWidth(), img.GetFormat().GetHeight(), len(img.GetImage()), float64(int64(ts)-int64(last))/1000, float64(time.Now().UnixMicro()-int64(ts))/1000)
		last = ts
	}
}

// TestDeviceStreamContent checks that an MMAP stream frame lands in the mapped file.
func TestDeviceStreamContent(t *testing.T) {
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
	path := filepath.Join(t.TempDir(), "fb")
	s, err := openShm(path, W, H)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	handle := env("AGEMU_TEST_HANDLE", "file://"+path)
	st, err := e.ctl.StreamScreenshot(ctx, &emupb.ImageFormat{Format: emupb.ImageFormat_RGBA8888,
		Width: uint32(envInt("AGEMU_TEST_W", 0)), Height: uint32(envInt("AGEMU_TEST_H", 0)),
		Transport: &emupb.ImageTransport{Channel: emupb.ImageTransport_MMAP, Handle: handle}})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(1500 * time.Millisecond)
		out, _ := exec.Command("lsof", "-p", strconv.Itoa(r.PID)).Output()
		for _, l := range strings.Split(string(out), "\n") {
			if strings.Contains(l, filepath.Base(filepath.Dir(path))) {
				t.Logf("emulator maps: %s", l)
			}
		}
		_ = e.touch(ctx, 540, 2000, 1)
		_ = e.touch(ctx, 540, 2000, 0)
	}()
	for i := 0; i < 6; i++ {
		img, err := st.Recv()
		if err != nil {
			t.Logf("recv: %v", err)
			break
		}
		nz := 0
		for j := 0; j < len(s.data); j += 4099 {
			if s.data[j] != 0 {
				nz++
			}
		}
		t.Logf("frame seq %d %dx%d image bytes %d, shm nonzero samples %d", img.GetSeq(), img.GetFormat().GetWidth(), img.GetFormat().GetHeight(), len(img.GetImage()), nz)
	}
	// On a static screen the streamed frame must equal a direct framebuffer read.
	time.Sleep(time.Second)
	fb, err := openShm(filepath.Join(t.TempDir(), "fb2"), W, H)
	if err != nil {
		t.Fatal(err)
	}
	e.fb = fb
	f, err := e.screenshotMMAP(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	diff := 0
	for i := range f.Pix {
		if f.Pix[i] != s.data[i] {
			diff++
		}
	}
	t.Logf("stream vs getScreenshot: %d of %d bytes differ", diff, len(f.Pix))
	var buf bytes.Buffer
	_ = encodePNG(&buf, rgbaImage{W: W, H: H, Pix: append([]byte(nil), s.data[:W*H*4]...)})
	_ = os.WriteFile(filepath.Join(os.TempDir(), "agemu-stream-check.png"), buf.Bytes(), 0o644)
}

// TestDeviceScreenshotFresh drives Settings back and forth through the backend and checks,
// after every "wait for the new screen", that the screenshot served from the frame stream is
// the frame on screen: it is compared with a direct framebuffer read taken right after it.
// A difference only counts as stale when the screen was not changing (a second direct read
// 300 ms later equals the first).
func TestDeviceScreenshotFresh(t *testing.T) {
	avd := env("AGEMU_TEST_AVD", "agemu-pixel")
	if _, ok := findRunning(avd); !ok {
		t.Skipf("%s not running", avd)
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
	direct := func() frame {
		f, err := e.screenshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return frame{W: f.W, H: f.H, Pix: append([]byte(nil), f.Pix...)}
	}
	waitFor := func(text string) {
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			if tr, err := b.UITree(ctx, id); err == nil {
				found := false
				tr.Walk(func(el *api.Element) { found = found || el.Text == text })
				if found {
					return
				}
			}
			time.Sleep(30 * time.Millisecond)
		}
		t.Fatalf("%q did not appear", text)
	}
	tapText := func(text string) {
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
		if hit == nil {
			t.Fatalf("no %q", text)
		}
		x, y := hit.Frame.Center()
		if err := b.Tap(ctx, id, x, y); err != nil {
			t.Fatal(err)
		}
	}
	_ = b.Terminate(ctx, id, "com.android.settings")
	if err := b.Launch(ctx, id, "com.android.settings"); err != nil {
		t.Fatal(err)
	}
	waitFor("Network & internet")
	stale, checked, inconclusive, fromStream := 0, 0, 0, 0
	for i := 0; i < envInt("AGEMU_TEST_N", 8); i++ {
		if i%2 == 0 {
			tapText("Network & internet")
			waitFor("Internet")
		} else {
			if err := b.Key(ctx, id, "back"); err != nil {
				t.Fatal(err)
			}
			waitFor("Search Settings")
		}
		var last time.Time
		if ns := d.lastInput.Load(); ns != 0 {
			last = time.Unix(0, ns)
		}
		t0 := time.Now()
		s, ok := e.stream.get(ctx, last)
		took := time.Since(t0)
		if !ok {
			t.Logf("%d: stream had no current frame (fallback) after %v", i, took)
			continue
		}
		fromStream++
		e.stream.mu.Lock()
		n0, lastNote := e.stream.notes, e.stream.lastNote
		e.stream.mu.Unlock()
		d1 := direct()
		e.stream.mu.Lock()
		n1 := e.stream.notes
		e.stream.mu.Unlock()
		time.Sleep(300 * time.Millisecond)
		d2 := direct()
		same := func(a, b frame) bool { return bytes.Equal(a.Pix, b.Pix) }
		switch {
		case same(s, d1):
			checked++
			t.Logf("%d: stream frame == direct read (stream wait %v)", i, took.Round(time.Millisecond))
		case n1 != n0:
			inconclusive++
			t.Logf("%d: a frame was posted between the stream read and the direct read; inconclusive", i)
		case same(d1, d2):
			t.Logf("%d: last notification %v before the stream read", i, t0.Sub(lastNote).Round(time.Millisecond))
			stale++
			checked++
			t.Errorf("%d: STALE: stream frame differs from a static screen (stream wait %v)", i, took.Round(time.Millisecond))
			var buf bytes.Buffer
			_ = encodePNG(&buf, rgbaImage{W: s.W, H: s.H, Pix: s.Pix})
			_ = os.WriteFile(filepath.Join(os.TempDir(), fmt.Sprintf("agemu-stale-%d.png", i)), buf.Bytes(), 0o644)
		default:
			inconclusive++
			t.Logf("%d: screen still changing; inconclusive", i)
		}
	}
	t.Logf("from stream %d, compared %d, stale %d, inconclusive %d", fromStream, checked, stale, inconclusive)
}

// TestDeviceLaunchPhases prints the agent's launch timing (startActivityAndWait vs the wait for
// the app's accessibility window) for warm launches, next to `am start -W`.
func TestDeviceLaunchPhases(t *testing.T) {
	avd := env("AGEMU_TEST_AVD", "agemu-pixel")
	if _, ok := findRunning(avd); !ok {
		t.Skipf("%s not running", avd)
	}
	ctx := context.Background()
	b := New()
	defer b.Close()
	id := "android:" + avd
	d, _ := b.devFor(id)
	if _, _, err := d.conn(ctx); err != nil {
		t.Fatal(err)
	}
	pkg := env("AGEMU_TEST_PKG", "com.android.settings")
	_ = d.call(ctx, map[string]any{"cmd": "launch", "pkg": pkg}, nil)
	for i := 0; i < envInt("AGEMU_TEST_N", 5); i++ {
		_ = b.Key(ctx, id, "home")
		time.Sleep(1500 * time.Millisecond)
		var res map[string]any
		t0 := time.Now()
		err := d.call(ctx, map[string]any{"cmd": "launch", "pkg": pkg}, &res)
		t.Logf("warm launch %d: %v total, agent says %v (err %v)", i, time.Since(t0).Round(time.Millisecond), res, err)
	}
}

// TestDeviceTypePhases prints the agent's set-text timing into the focused field.
func TestDeviceTypePhases(t *testing.T) {
	avd := env("AGEMU_TEST_AVD", "agemu-pixel")
	if _, ok := findRunning(avd); !ok {
		t.Skipf("%s not running", avd)
	}
	ctx := context.Background()
	b := New()
	defer b.Close()
	d, _ := b.devFor("android:" + avd)
	if _, _, err := d.conn(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < envInt("AGEMU_TEST_N", 8); i++ {
		var res map[string]any
		t0 := time.Now()
		time.Sleep(time.Duration(envInt("AGEMU_TEST_GAP_MS", 1)) * time.Millisecond)
		keys := i%2 == 1 && env("AGEMU_TEST_KEYS", "1") == "1"
		t0 = time.Now()
		word := env("AGEMU_TEST_WORD", "hello")
		if word == "seq" {
			word = fmt.Sprintf("<%d>", i)
		}
		err := d.call(ctx, map[string]any{"cmd": "text", "text": word, "keys": keys}, &res)
		t.Logf("type %d: %v round trip, agent %v (err %v)", i, time.Since(t0).Round(time.Millisecond), res, err)
	}
}

// TestDevicePauseResume pauses the VM for AGEMU_TEST_PAUSE_S seconds, resumes it, and watches
// the guest's uptime for a reboot.
func TestDevicePauseResume(t *testing.T) {
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
	ctx := context.Background()
	uptime := func() string {
		c, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		out, err := adbShellOK(c, r.serial(), "cat /proc/uptime")
		if err != nil {
			return "err: " + err.Error()
		}
		return strings.Fields(out)[0]
	}
	pause := time.Duration(envInt("AGEMU_TEST_PAUSE_S", 30)) * time.Second
	t.Logf("before pause: uptime %s", uptime())
	if err := e.setVM(ctx, emupb.VmRunState_PAUSED); err != nil {
		t.Fatal(err)
	}
	time.Sleep(pause)
	if err := e.setVM(ctx, emupb.VmRunState_RUNNING); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	for i := 0; i < envInt("AGEMU_TEST_WATCH_S", 60)/5; i++ {
		time.Sleep(5 * time.Second)
		t.Logf("+%v after resume: uptime %s", time.Since(t0).Round(time.Second), uptime())
	}
}

// TestDeviceSaveThenRender saves a throwaway snapshot (deleted afterwards), then drives Settings
// search for a while, watching for a guest reboot.
func TestDeviceSaveThenRender(t *testing.T) {
	avd := env("AGEMU_TEST_AVD", "agemu-pixel")
	if _, ok := findRunning(avd); !ok {
		t.Skipf("%s not running", avd)
	}
	ctx := context.Background()
	b := New()
	defer b.Close()
	id := "android:" + avd
	d, _ := b.devFor(id)
	e, a, err := d.conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	uptime := func() float64 {
		c, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		out, err := adbShellOK(c, d.run.serial(), "cat /proc/uptime")
		if err != nil {
			return -1
		}
		v, _ := strconv.ParseFloat(strings.Fields(out)[0], 64)
		return v
	}
	for round := 0; round < envInt("AGEMU_TEST_N", 2); round++ {
		a.Close()
		if a.rd != nil {
			a.rd.drop(ctx)
		}
		t0 := time.Now()
		if err := e.saveSnapshot(ctx, "agemu-probe"); err != nil {
			t.Fatal(err)
		}
		t.Logf("round %d: saved in %v, uptime %.1f", round, time.Since(t0).Round(time.Millisecond), uptime())
		start := time.Now()
		for time.Since(start) < 60*time.Second {
			_ = b.Terminate(ctx, id, "com.android.settings")
			_ = b.Launch(ctx, id, "com.android.settings")
			_, _ = b.UITree(ctx, id)
			_ = b.Swipe(ctx, id, 540, 1800, 540, 900, 150)
			_ = b.Swipe(ctx, id, 540, 900, 540, 1800, 150)
			u := uptime()
			t.Logf("  +%v uptime %.1f", time.Since(start).Round(time.Second), u)
			if u >= 0 && u < 60 {
				t.Errorf("guest rebooted (uptime %.1f)", u)
				break
			}
		}
	}
	_ = e.deleteSnapshot(ctx, "agemu-probe")
}
