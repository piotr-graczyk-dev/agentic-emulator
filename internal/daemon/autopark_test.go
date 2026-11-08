package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/api"
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/backend"
)

// apBackend is a Parker safe for concurrent use. It fails any device call made while the
// device is parked or being parked, which is what the auto-parker must never let happen.
type apBackend struct {
	fakeBackend
	plat api.Platform

	mu       sync.Mutex
	devs     map[string]*apDev
	parks    int
	resumes  int
	parkErr  error
	parkFor  time.Duration // how long a park takes
	treeHold chan struct{} // when set, UITree blocks until it is closed
	bad      []string      // calls that reached a stopped device
	// canon, when set, makes the fake a backend.IDNormalizer (as the real iOS and Android
	// backends are) with this spelling rule.
	canon func(string) string
}

type apDev struct {
	name, state      string // state as listed: booted, booting, shutdown
	parked, stopping bool
}

var _ backend.Parker = (*apBackend)(nil)

func newAPBackend(plat api.Platform, names ...string) *apBackend {
	b := &apBackend{plat: plat, devs: map[string]*apDev{}}
	for _, n := range names {
		b.devs[string(plat)+":"+n] = &apDev{name: n, state: "booted"}
	}
	return b
}

func (b *apBackend) Platform() api.Platform { return b.plat }

func (b *apBackend) List(ctx context.Context) ([]api.Device, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []api.Device
	for id, d := range b.devs {
		st := d.state
		if d.parked {
			st = api.StateParked
		}
		out = append(out, api.Device{ID: id, Name: d.name, Platform: b.plat, State: st})
	}
	return out, nil
}

func (b *apBackend) Park(ctx context.Context, id string) error {
	b.mu.Lock()
	d := b.devs[id]
	if b.parkErr != nil {
		b.mu.Unlock()
		return b.parkErr
	}
	d.stopping = true
	wait := b.parkFor
	b.mu.Unlock()
	time.Sleep(wait)
	b.mu.Lock()
	d.stopping, d.parked = false, true
	b.parks++
	b.mu.Unlock()
	return nil
}

func (b *apBackend) Parked(id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	d := b.devs[id]
	return d != nil && d.parked
}

func (b *apBackend) Resume(ctx context.Context, id string) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	d := b.devs[id]
	if d.stopping {
		b.bad = append(b.bad, "resume during a park")
	}
	if !d.parked {
		return false, nil
	}
	d.parked = false
	b.resumes++
	return true, nil
}

// use records a call on the device; it must find the device running.
func (b *apBackend) use(id, what string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if d := b.devs[id]; d.parked || d.stopping {
		b.bad = append(b.bad, fmt.Sprintf("%s on %s (parked %v, stopping %v)", what, id, d.parked, d.stopping))
	}
}

func (b *apBackend) UITree(ctx context.Context, id string) (*api.UITree, error) {
	b.use(id, "ui")
	b.mu.Lock()
	hold := b.treeHold
	b.mu.Unlock()
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	b.use(id, "ui (end)")
	return tree(), nil
}

func (b *apBackend) Tap(ctx context.Context, id string, x, y int) error {
	b.use(id, "tap")
	return nil
}

// Logs reads what was captured: it may run on a parked device.
func (b *apBackend) Logs(ctx context.Context, id string, q api.LogQuery) (api.LogBatch, error) {
	return api.LogBatch{}, nil
}

func (b *apBackend) counts() (parks, resumes int, bad []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.parks, b.resumes, append([]string(nil), b.bad...)
}

func (b *apBackend) setState(id, st string) {
	b.mu.Lock()
	b.devs[id].state = st
	b.mu.Unlock()
}

// apService is a service with the auto-parker on at a test threshold (the setting's minimum
// does not apply here).
func apService(t *testing.T, after, tick time.Duration, bs ...backend.Backend) *Service {
	t.Helper()
	t.Setenv("AGEMU_AUTO_PARK", "")
	s := NewService(t.TempDir(), bs...)
	s.startAutoPark(after, tick)
	t.Cleanup(s.stopAutoPark)
	return s
}

func apCall(s *Service, method, dev string) error {
	_, err := s.Call(context.Background(), method, json.RawMessage(`{"device":"`+dev+`"}`))
	return err
}

// waitFor polls cond every millisecond for at most d.
func waitFor(d time.Duration, cond func() bool) bool {
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if cond() {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return cond()
}

func TestParseAutoPark(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"": 0, "0": 0, "off": 0, "OFF": 0, "no": 0, "false": 0,
		"10m": 10 * time.Minute, "90s": 90 * time.Second, "600": 600 * time.Second, " 1h ": time.Hour, "10s": 10 * time.Second,
	} {
		got, err := parseAutoPark(in)
		if err != nil || got != want {
			t.Errorf("parseAutoPark(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"5s", "9.9", "-1m", "soon", "1"} {
		if got, err := parseAutoPark(in); err == nil {
			t.Errorf("parseAutoPark(%q) = %v, want an error", in, got)
		}
	}
}

func TestAutoParkTick(t *testing.T) {
	for after, want := range map[time.Duration]time.Duration{
		10 * time.Second: time.Second, 60 * time.Second: 3 * time.Second, 10 * time.Minute: 10 * time.Second,
	} {
		if got := autoParkTick(after); got != want {
			t.Errorf("autoParkTick(%v) = %v, want %v", after, got, want)
		}
	}
}

func TestAutoParkIsOffByDefault(t *testing.T) {
	t.Setenv("AGEMU_AUTO_PARK", "")
	if s := NewService(t.TempDir()); s.ap != nil {
		t.Fatal("auto-park on without AGEMU_AUTO_PARK")
	}
	t.Setenv("AGEMU_AUTO_PARK", "5s") // below the minimum: refused, stays off
	if s := NewService(t.TempDir()); s.ap != nil {
		t.Fatal("auto-park on with a refused value")
	}
	t.Setenv("AGEMU_AUTO_PARK", "45s")
	s := NewService(t.TempDir())
	defer s.Close()
	if s.ap == nil || s.ap.after != 45*time.Second {
		t.Fatalf("ap = %+v", s.ap)
	}
}

func TestAnIdleDeviceIsParkedAfterTheThresholdAndTheNextCallResumesIt(t *testing.T) {
	b := newAPBackend(api.Android, "agemu-a")
	const after, tick = 80 * time.Millisecond, 5 * time.Millisecond
	s := apService(t, after, tick, b)
	if err := apCall(s, "ui", "android:agemu-a"); err != nil {
		t.Fatal(err)
	}
	called := time.Now()
	if !waitFor(time.Second, func() bool { return b.Parked("android:agemu-a") }) {
		t.Fatal("not parked")
	}
	if idle := time.Since(called); idle < after || idle > after+tick+50*time.Millisecond {
		t.Fatalf("parked after %v idle, want %v (+ a tick)", idle, after)
	}
	if err := apCall(s, "ui", "android:agemu-a"); err != nil {
		t.Fatal(err)
	}
	parks, resumes, bad := b.counts()
	if parks != 1 || resumes != 1 || len(bad) > 0 || b.Parked("android:agemu-a") {
		t.Fatalf("parks %d resumes %d bad %v", parks, resumes, bad)
	}
	// One park per idle stretch: idle again, parked again once.
	if !waitFor(time.Second, func() bool { return b.Parked("android:agemu-a") }) {
		t.Fatal("not parked again")
	}
	time.Sleep(3 * after)
	if parks, _, _ := b.counts(); parks != 2 {
		t.Fatalf("parks = %d, want 2", parks)
	}
}

func TestNoAutoParkWhileACallIsInFlight(t *testing.T) {
	b := newAPBackend(api.Android, "agemu-a")
	hold := make(chan struct{})
	b.treeHold = hold
	s := apService(t, 30*time.Millisecond, 2*time.Millisecond, b)
	done := make(chan error, 1)
	go func() { done <- apCall(s, "ui", "android:agemu-a") }() // held like a long wait
	time.Sleep(300 * time.Millisecond)                         // 10x the threshold
	if parks, _, _ := b.counts(); parks != 0 {
		t.Fatalf("parked %d times while a call was in flight", parks)
	}
	b.mu.Lock()
	b.treeHold = nil
	b.mu.Unlock()
	close(hold)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !waitFor(time.Second, func() bool { return b.Parked("android:agemu-a") }) {
		t.Fatal("not parked once the call ended")
	}
	if _, _, bad := b.counts(); len(bad) > 0 {
		t.Fatal(bad)
	}
}

func TestALongWaitKeepsTheDeviceRunning(t *testing.T) {
	b := newAPBackend(api.Android, "agemu-a")
	s := apService(t, 30*time.Millisecond, 2*time.Millisecond, b)
	// wait polls the tree until its timeout, 10x the threshold.
	_, err := s.Call(context.Background(), "wait", json.RawMessage(`{"device":"android:agemu-a","selector":{"text":"never"},"timeoutMs":300}`))
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("err = %v", err)
	}
	parks, _, bad := b.counts()
	if parks != 0 || len(bad) > 0 {
		t.Fatalf("parks %d during the wait, bad %v", parks, bad)
	}
}

func TestOnlyDevicesTheDaemonServedAreParked(t *testing.T) {
	b := newAPBackend(api.Android, "agemu-a", "agemu-b")
	s := apService(t, 20*time.Millisecond, 2*time.Millisecond, b)
	if err := apCall(s, "ui", "android:agemu-a"); err != nil {
		t.Fatal(err)
	}
	if !waitFor(time.Second, func() bool { return b.Parked("android:agemu-a") }) {
		t.Fatal("served device not parked")
	}
	time.Sleep(100 * time.Millisecond)
	if b.Parked("android:agemu-b") {
		t.Fatal("parked a device this daemon never served")
	}
	// devices lists without counting as a call on any device.
	if _, err := s.Call(context.Background(), "devices", nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if b.Parked("android:agemu-b") {
		t.Fatal("devices made a device parkable")
	}
}

func TestDevicesThatAreNotBootedOrNotAgemusAreNotParked(t *testing.T) {
	for _, st := range []string{"booting", "shutdown"} {
		b := newAPBackend(api.Android, "agemu-a")
		s := apService(t, 20*time.Millisecond, 2*time.Millisecond, b)
		if err := apCall(s, "ui", "android:agemu-a"); err != nil {
			t.Fatal(err)
		}
		b.setState("android:agemu-a", st) // e.g. shut down by the call, or booting
		time.Sleep(150 * time.Millisecond)
		if parks, _, _ := b.counts(); parks != 0 {
			t.Fatalf("parked a %s device", st)
		}
		// Booted again: the next idle stretch parks it.
		b.setState("android:agemu-a", "booted")
		if err := apCall(s, "ui", "android:agemu-a"); err != nil {
			t.Fatal(err)
		}
		if !waitFor(time.Second, func() bool { return b.Parked("android:agemu-a") }) {
			t.Fatalf("%s then booted: not parked", st)
		}
	}
	// A simulator that is not agemu's (driven with AGEMU_IOS_ALLOW_FOREIGN) is never parked.
	ib := newAPBackend(api.IOS, "Zuapp E2E", "agemu-iphone")
	s := apService(t, 20*time.Millisecond, 2*time.Millisecond, ib)
	for _, d := range []string{"ios:Zuapp E2E", "ios:agemu-iphone"} {
		if err := apCall(s, "ui", d); err != nil {
			t.Fatal(err)
		}
	}
	if !waitFor(time.Second, func() bool { return ib.Parked("ios:agemu-iphone") }) {
		t.Fatal("agemu simulator not parked")
	}
	time.Sleep(100 * time.Millisecond)
	if ib.Parked("ios:Zuapp E2E") {
		t.Fatal("parked a foreign simulator")
	}
}

func TestAManuallyParkedDeviceIsLeftAloneAndStaysParkedAtClose(t *testing.T) {
	b := newAPBackend(api.Android, "agemu-a", "agemu-b")
	s := apService(t, 30*time.Millisecond, 2*time.Millisecond, b)
	if err := apCall(s, "park", "android:agemu-a"); err != nil {
		t.Fatal(err)
	}
	if err := apCall(s, "ui", "android:agemu-b"); err != nil {
		t.Fatal(err)
	}
	if !waitFor(time.Second, func() bool { return b.Parked("android:agemu-b") }) {
		t.Fatal("b not auto-parked")
	}
	time.Sleep(100 * time.Millisecond)
	if parks, _, _ := b.counts(); parks != 2 {
		t.Fatalf("parks = %d, want 2 (a once by hand, b once by the parker)", parks)
	}
	s.stopAutoPark() // what Close does before closing the backends
	if !b.Parked("android:agemu-a") {
		t.Fatal("the agent's park was undone at close")
	}
	if b.Parked("android:agemu-b") {
		t.Fatal("the parker's park was left behind at close")
	}
}

func TestAnAutoParkedDeviceParkedAgainByHandStaysParkedAtClose(t *testing.T) {
	b := newAPBackend(api.Android, "agemu-a")
	s := apService(t, 30*time.Millisecond, 2*time.Millisecond, b)
	if err := apCall(s, "ui", "android:agemu-a"); err != nil {
		t.Fatal(err)
	}
	if !waitFor(time.Second, func() bool { return b.Parked("android:agemu-a") }) {
		t.Fatal("not auto-parked")
	}
	// park on a parked device: wake does not run for park, the backend's park is a no-op; the
	// agent now owns the park.
	if err := apCall(s, "park", "android:agemu-a"); err != nil {
		t.Fatal(err)
	}
	s.stopAutoPark()
	if !b.Parked("android:agemu-a") {
		t.Fatal("an agent's park was undone at close")
	}
}

func TestLogReadsKeepTheDeviceRunningButDoNotResumeIt(t *testing.T) {
	b := newAPBackend(api.Android, "agemu-a")
	const after = 60 * time.Millisecond
	s := apService(t, after, 2*time.Millisecond, b)
	if err := apCall(s, "ui", "android:agemu-a"); err != nil {
		t.Fatal(err)
	}
	// An agent polling logs (and crashes) more often than the threshold, for 5 thresholds.
	for i := 0; i < 15; i++ {
		time.Sleep(after / 3)
		m := "logs"
		if i%2 == 1 {
			m = "crashes"
		}
		if err := apCall(s, m, "android:agemu-a"); err != nil && !errors.Is(err, api.ErrNotImplemented) {
			t.Fatal(err)
		}
	}
	if parks, _, _ := b.counts(); parks != 0 {
		t.Fatalf("parked %d times while the agent polled logs", parks)
	}
	// Once polling stops it is parked; a log read then does not resume it.
	if !waitFor(time.Second, func() bool { return b.Parked("android:agemu-a") }) {
		t.Fatal("not parked")
	}
	if err := apCall(s, "logs", "android:agemu-a"); err != nil {
		t.Fatal(err)
	}
	if !b.Parked("android:agemu-a") {
		t.Fatal("logs resumed the device")
	}
	// It still counts as the parker's park at close.
	s.stopAutoPark()
	if b.Parked("android:agemu-a") {
		t.Fatal("the parker's park was left behind at close")
	}
}

func TestAFailedAutoParkIsNotRetriedUntilTheNextCall(t *testing.T) {
	b := newAPBackend(api.Android, "agemu-a")
	b.parkErr = errors.New("emulator gone")
	var tries atomic.Int32
	s := apService(t, 20*time.Millisecond, 2*time.Millisecond, &countingParker{b, &tries})
	if err := apCall(s, "ui", "android:agemu-a"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if n := tries.Load(); n != 1 {
		t.Fatalf("park tried %d times in one idle stretch, want 1", n)
	}
	if err := apCall(s, "ui", "android:agemu-a"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if n := tries.Load(); n != 2 {
		t.Fatalf("park tried %d times after a second stretch, want 2", n)
	}
}

type countingParker struct {
	*apBackend
	n *atomic.Int32
}

func (c *countingParker) Park(ctx context.Context, id string) error {
	c.n.Add(1)
	return c.apBackend.Park(ctx, id)
}

// The race the gate exists for: calls that start while the parker parks the device. Many
// callers with random gaps around the threshold; no call may reach a parked or stopping
// device, and every call must succeed. Run with -race.
func TestConcurrentCallsAroundTheParkNeverReachAStoppedDevice(t *testing.T) {
	b := newAPBackend(api.Android, "agemu-a", "agemu-b")
	b.parkFor = 3 * time.Millisecond // a park takes a moment, as a real one (8-90 ms)
	const after = 15 * time.Millisecond
	s := apService(t, after, time.Millisecond, b)
	var wg sync.WaitGroup
	var failed atomic.Int32
	deadline := time.Now().Add(1500 * time.Millisecond)
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			r := rand.New(rand.NewSource(int64(w)))
			dev := []string{"android:agemu-a", "android:agemu-b"}[w%2]
			for time.Now().Before(deadline) {
				time.Sleep(time.Duration(r.Int63n(int64(2 * after))))
				m := []string{"ui", "tap", "logs", "find"}[r.Intn(4)]
				var raw string
				switch m {
				case "tap":
					raw = `{"device":"` + dev + `","x":1,"y":1}`
				case "find":
					raw = `{"device":"` + dev + `","selector":{"text":"Log in"}}`
				default:
					raw = `{"device":"` + dev + `"}`
				}
				if _, err := s.Call(context.Background(), m, json.RawMessage(raw)); err != nil {
					t.Errorf("%s: %v", m, err)
					failed.Add(1)
				}
			}
		}(w)
	}
	wg.Wait()
	parks, resumes, bad := b.counts()
	if len(bad) > 0 {
		t.Fatalf("%d calls reached a stopped device, e.g. %v", len(bad), bad[0])
	}
	if parks == 0 || resumes == 0 {
		t.Fatalf("parks %d resumes %d: the test did not exercise the transition", parks, resumes)
	}
	t.Logf("parks %d, resumes %d, failed calls %d", parks, resumes, failed.Load())
}

// A call that starts while a park is in progress waits for it, then resumes the device.
func TestACallDuringTheParkWaitsAndResumes(t *testing.T) {
	b := newAPBackend(api.Android, "agemu-a")
	b.parkFor = 100 * time.Millisecond
	s := apService(t, 10*time.Millisecond, time.Millisecond, b)
	if err := apCall(s, "ui", "android:agemu-a"); err != nil {
		t.Fatal(err)
	}
	if !waitFor(time.Second, func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return b.devs["android:agemu-a"].stopping
	}) {
		t.Fatal("park did not start")
	}
	start := time.Now()
	if err := apCall(s, "ui", "android:agemu-a"); err != nil {
		t.Fatal(err)
	}
	parks, resumes, bad := b.counts()
	if len(bad) > 0 || parks != 1 || resumes != 1 {
		t.Fatalf("parks %d resumes %d bad %v", parks, resumes, bad)
	}
	if waited := time.Since(start); waited > time.Second {
		t.Fatalf("the call waited %v", waited)
	}
}

func TestAContextCancelledWhileWaitingForAParkEndsTheCall(t *testing.T) {
	b := newAPBackend(api.Android, "agemu-a")
	b.parkFor = 300 * time.Millisecond
	s := apService(t, 10*time.Millisecond, time.Millisecond, b)
	if err := apCall(s, "ui", "android:agemu-a"); err != nil {
		t.Fatal(err)
	}
	waitFor(time.Second, func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return b.devs["android:agemu-a"].stopping
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := s.Call(ctx, "ui", json.RawMessage(`{"device":"android:agemu-a"}`)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
}

func TestAProfileSwitchToAnotherDeviceMakesItParkable(t *testing.T) {
	b := &switchBackend{apBackend: newAPBackend(api.IOS, "agemu-iphone", "agemu-ipad")}
	s := apService(t, 20*time.Millisecond, 2*time.Millisecond, b)
	if _, err := s.Call(context.Background(), "set_profile", json.RawMessage(`{"device":"ios:agemu-iphone","profile":"ipad-mini"}`)); err != nil {
		t.Fatal(err)
	}
	if !waitFor(time.Second, func() bool { return b.Parked("ios:agemu-ipad") }) {
		t.Fatal("the device the profile switch moved to was not parked")
	}
}

// switchBackend answers set_profile with another simulator, as iOS does.
type switchBackend struct{ *apBackend }

func (b *switchBackend) SetProfile(ctx context.Context, id string, p api.Profile) (api.Device, error) {
	return api.Device{ID: "ios:agemu-ipad", Name: "agemu-ipad", Platform: api.IOS, State: "booted"}, nil
}

// apNormBackend is apBackend with a canonical id rule (backend.IDNormalizer).
type apNormBackend struct{ *apBackend }

func (b *apNormBackend) CanonicalID(id string) string { return b.canon(id) }

// Two spellings of one simulator's id (the helper matches UDIDs in any case): a call in flight
// under one spelling must hold off the park registered under the other, and a call under either
// spelling must resume it. (The skeptic's repro, with the fake made an IDNormalizer as the real
// backends are.)
func TestAnotherSpellingOfTheIdCountsAsTheSameDevice(t *testing.T) {
	ab := newAPBackend(api.IOS, "agemu-x")
	ab.devs["ios:AGEMU-X"] = ab.devs["ios:agemu-x"] // the backend accepts both spellings
	ab.canon = strings.ToLower                      // and lists ios:agemu-x
	b := &apNormBackend{ab}
	s := apService(t, 30*time.Millisecond, 2*time.Millisecond, b)
	if err := apCall(s, "ui", "ios:agemu-x"); err != nil {
		t.Fatal(err)
	}
	hold := make(chan struct{})
	ab.mu.Lock()
	ab.treeHold = hold
	ab.mu.Unlock()
	done := make(chan error, 1)
	go func() { done <- apCall(s, "ui", "ios:AGEMU-X") }() // a long call under the other spelling
	time.Sleep(200 * time.Millisecond)
	ab.mu.Lock()
	ab.treeHold = nil
	ab.mu.Unlock()
	close(hold)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if parks, _, bad := ab.counts(); len(bad) > 0 || parks != 0 {
		t.Fatalf("parked %d times under a call made with another spelling: %v", parks, bad)
	}
	// Parked under one spelling, a call under the other resumes it first.
	if !waitFor(time.Second, func() bool { return ab.Parked("ios:agemu-x") }) {
		t.Fatal("not parked")
	}
	if err := apCall(s, "ui", "ios:AGEMU-X"); err != nil {
		t.Fatal(err)
	}
	if ab.Parked("ios:agemu-x") {
		t.Fatal("a call under the other spelling did not resume the device")
	}
	if _, _, bad := ab.counts(); len(bad) > 0 {
		t.Fatal(bad)
	}
	s.actMu.Lock()
	n := len(s.activity)
	s.actMu.Unlock()
	if n != 1 {
		t.Fatalf("%d activity entries for one device", n)
	}
}

// switchingBackend models iOS's profile switch: it acts on the target simulator (boot, ready),
// resuming it first if it is parked, as ios.Backend.SetProfile does; hold, when set, keeps the
// switch running.
type switchingBackend struct {
	*apBackend
	target string
	hold   chan struct{}
}

func (b *switchingBackend) SetProfile(ctx context.Context, id string, p api.Profile) (api.Device, error) {
	if b.Parked(b.target) {
		if _, err := b.Resume(ctx, b.target); err != nil {
			return api.Device{}, err
		}
	}
	b.use(b.target, "set_profile boots the target")
	if b.hold != nil {
		<-b.hold
	}
	b.use(b.target, "set_profile readies the target")
	return api.Device{ID: b.target, Name: strings.TrimPrefix(b.target, "ios:"), Platform: api.IOS, State: "booted"}, nil
}

// The skeptic's blocker: a profile switch lands on a simulator the parker parked while the agent
// worked on another one. The switch resumes it, and nothing reaches it stopped.
func TestAProfileSwitchOntoAnAutoParkedDeviceResumesIt(t *testing.T) {
	b := &switchingBackend{apBackend: newAPBackend(api.IOS, "agemu-iphone", "agemu-ipad"), target: "ios:agemu-ipad"}
	s := apService(t, 30*time.Millisecond, 2*time.Millisecond, b)
	if err := apCall(s, "ui", "ios:agemu-ipad"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for !b.Parked("ios:agemu-ipad") && time.Now().Before(deadline) {
		if err := apCall(s, "ui", "ios:agemu-iphone"); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !b.Parked("ios:agemu-ipad") {
		t.Fatal("setup: the iPad was not auto-parked")
	}
	if _, err := s.Call(context.Background(), "set_profile", json.RawMessage(`{"device":"ios:agemu-iphone","profile":"ipad-mini"}`)); err != nil {
		t.Fatal(err)
	}
	if _, _, bad := b.counts(); len(bad) > 0 {
		t.Fatalf("the switch acted on a stopped device: %v", bad)
	}
	if b.Parked("ios:agemu-ipad") {
		t.Fatal("the switch's target is still parked")
	}
	// The resume came from the switch, not from the parker: the close must not resume it again.
	s.stopAutoPark()
}

// While a switch runs, no device of its platform is parked (the switch may be booting any of
// them); after it, the idle ones are.
func TestNoAutoParkOnThePlatformWhileAProfileSwitchRuns(t *testing.T) {
	hold := make(chan struct{})
	b := &switchingBackend{apBackend: newAPBackend(api.IOS, "agemu-iphone", "agemu-ipad", "agemu-se"), target: "ios:agemu-ipad", hold: hold}
	s := apService(t, 30*time.Millisecond, 2*time.Millisecond, b)
	for _, d := range []string{"ios:agemu-ipad", "ios:agemu-se"} {
		if err := apCall(s, "ui", d); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan error, 1)
	go func() {
		_, err := s.Call(context.Background(), "set_profile", json.RawMessage(`{"device":"ios:agemu-iphone","profile":"ipad-mini"}`))
		done <- err
	}()
	time.Sleep(300 * time.Millisecond) // 10x the threshold
	if parks, _, _ := b.counts(); parks != 0 {
		t.Fatalf("parked %d devices while a profile switch ran", parks)
	}
	close(hold)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !waitFor(time.Second, func() bool { return b.Parked("ios:agemu-se") && b.Parked("ios:agemu-ipad") }) {
		t.Fatal("idle devices not parked after the switch")
	}
	if _, _, bad := b.counts(); len(bad) > 0 {
		t.Fatal(bad)
	}
}

// A switch that starts while another device of its platform is being parked waits for that park.
func TestAProfileSwitchWaitsForAParkInProgress(t *testing.T) {
	b := &switchingBackend{apBackend: newAPBackend(api.IOS, "agemu-iphone", "agemu-ipad"), target: "ios:agemu-ipad"}
	b.parkFor = 150 * time.Millisecond
	s := apService(t, 10*time.Millisecond, time.Millisecond, b)
	if err := apCall(s, "ui", "ios:agemu-ipad"); err != nil {
		t.Fatal(err)
	}
	if !waitFor(time.Second, func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return b.devs["ios:agemu-ipad"].stopping
	}) {
		t.Fatal("park did not start")
	}
	if _, err := s.Call(context.Background(), "set_profile", json.RawMessage(`{"device":"ios:agemu-iphone","profile":"ipad-mini"}`)); err != nil {
		t.Fatal(err)
	}
	if _, _, bad := b.counts(); len(bad) > 0 {
		t.Fatalf("the switch ran during a park: %v", bad)
	}
}

func TestCallsWithoutAutoParkDoNotTrackActivity(t *testing.T) {
	t.Setenv("AGEMU_AUTO_PARK", "")
	b := newAPBackend(api.Android, "agemu-a")
	s := NewService(t.TempDir(), b)
	for _, m := range []string{"ui", "logs", "park", "resume"} {
		if err := apCall(s, m, "android:agemu-a"); err != nil {
			t.Fatal(m, err)
		}
	}
	if len(s.activity) != 0 || len(s.switching) != 0 {
		t.Fatalf("activity tracked with auto-park off: %v %v", s.activity, s.switching)
	}
}

// noResumeSwitch acts on the switch's target without resuming it (a backend that does not handle
// a parked target itself): the daemon must have resumed the devices it parked before the switch.
type noResumeSwitch struct{ *apBackend }

func (b *noResumeSwitch) SetProfile(ctx context.Context, id string, p api.Profile) (api.Device, error) {
	b.use("ios:agemu-ipad", "set_profile readies the target")
	return api.Device{ID: "ios:agemu-ipad", Name: "agemu-ipad", Platform: api.IOS, State: "booted"}, nil
}

func TestTheDaemonResumesItsParksOnThePlatformBeforeAProfileSwitch(t *testing.T) {
	b := &noResumeSwitch{newAPBackend(api.IOS, "agemu-iphone", "agemu-ipad")}
	s := apService(t, 30*time.Millisecond, 2*time.Millisecond, b)
	if err := apCall(s, "ui", "ios:agemu-ipad"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for !b.Parked("ios:agemu-ipad") && time.Now().Before(deadline) {
		if err := apCall(s, "ui", "ios:agemu-iphone"); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !b.Parked("ios:agemu-ipad") {
		t.Fatal("setup: the iPad was not auto-parked")
	}
	if _, err := s.Call(context.Background(), "set_profile", json.RawMessage(`{"device":"ios:agemu-iphone","profile":"ipad-mini"}`)); err != nil {
		t.Fatal(err)
	}
	if _, _, bad := b.counts(); len(bad) > 0 {
		t.Fatalf("the switch acted on a parked device: %v", bad)
	}
}
