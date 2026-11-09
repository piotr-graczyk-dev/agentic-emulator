package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/api"
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/backend"
)

// apPrepBackend is apBackend that is also a backend.Preparer. Its device can be preparing while
// List still says booted (as a backend whose List lags, or one that lists a first-boot repair as
// booted would): the parker must rely on Preparing, not only on the listed state.
type apPrepBackend struct {
	*apBackend
	pmu       sync.Mutex
	preparing map[string]bool
}

var _ backend.Preparer = (*apPrepBackend)(nil)

func (b *apPrepBackend) setPreparing(id string, on bool) {
	b.pmu.Lock()
	b.preparing[id] = on
	b.pmu.Unlock()
}

func (b *apPrepBackend) Preparing(id string) *api.Preparing {
	b.pmu.Lock()
	defer b.pmu.Unlock()
	if b.preparing[id] {
		return &api.Preparing{Phase: "settling", ElapsedS: 60, MaxS: 1200}
	}
	return nil
}

func (b *apPrepBackend) AwaitDecision(ctx context.Context, id string) *api.Preparing {
	return b.Preparing(id)
}

// Park on a preparing device would hang for minutes behind the boot in a real backend: record it.
func (b *apPrepBackend) Park(ctx context.Context, id string) error {
	if b.Preparing(id) != nil {
		b.mu.Lock()
		b.bad = append(b.bad, "park of a preparing device")
		b.mu.Unlock()
	}
	return b.apBackend.Park(ctx, id)
}

func (b *apPrepBackend) Resume(ctx context.Context, id string) (bool, error) {
	if b.Preparing(id) != nil {
		b.mu.Lock()
		b.bad = append(b.bad, "resume of a preparing device")
		b.mu.Unlock()
	}
	return b.apBackend.Resume(ctx, id)
}

// The auto-parker never parks a device that is preparing its baseline (a first boot, a first-boot
// repair or a retake), even when its listed state does not say so, and parks it once the
// preparation is over and it has been idle for the threshold.
func TestAutoParkNeverParksAPreparingDevice(t *testing.T) {
	const id = "android:agemu-p"
	b := &apPrepBackend{apBackend: newAPBackend(api.Android, "agemu-p"), preparing: map[string]bool{}}
	const after, tick = 30 * time.Millisecond, 2 * time.Millisecond
	s := apService(t, after, tick, b)
	if err := apCall(s, "ui", id); err != nil { // served before the preparation started
		t.Fatal(err)
	}
	b.setPreparing(id, true)
	time.Sleep(15 * after)
	if parks, _, bad := b.counts(); parks != 0 || len(bad) > 0 {
		t.Fatalf("parks %d while preparing, bad %v", parks, bad)
	}
	// A call during the preparation fails at once with the preparing error (no wake, no resume).
	start := time.Now()
	err := apCall(s, "ui", id)
	if !errors.Is(err, api.ErrPreparing) || errorCode(err) != CodePreparing {
		t.Fatalf("call on a preparing device: %v", err)
	}
	if el := time.Since(start); el > 100*time.Millisecond {
		t.Fatalf("the call took %v", el)
	}
	b.setPreparing(id, false)
	if err := apCall(s, "ui", id); err != nil { // the preparation is over: a new idle stretch
		t.Fatal(err)
	}
	if !waitFor(time.Second, func() bool { return b.Parked(id) }) {
		t.Fatal("not parked after the preparation ended")
	}
	if _, _, bad := b.counts(); len(bad) > 0 {
		t.Fatal(bad)
	}
}

// A preparation that starts while the parker is deciding (after its first check) is caught by the
// check under the device lock, right before the park.
func TestAutoParkRechecksPreparingUnderTheLock(t *testing.T) {
	const id = "android:agemu-p"
	b := &apPrepBackend{apBackend: newAPBackend(api.Android, "agemu-p"), preparing: map[string]bool{}}
	s := NewService(t.TempDir(), b)
	s.ap = &autoParker{after: time.Millisecond, tick: time.Hour, stop: func() {}, done: make(chan struct{})}
	if err := apCall(s, "ui", id); err != nil {
		t.Fatal(err)
	}
	s.actMu.Lock()
	last := s.activity[id].last
	s.actMu.Unlock()
	// notParkable lists the backend: make the preparation start there.
	b.apBackend.mu.Lock()
	b.devs[id].state = "booted"
	b.apBackend.mu.Unlock()
	listed := &listHook{apPrepBackend: b, before: func() { b.setPreparing(id, true) }}
	s.backends[api.Android] = listed
	s.autoParkOne(context.Background(), id, last)
	if parks, _, bad := b.counts(); parks != 0 || len(bad) > 0 {
		t.Fatalf("parks %d, bad %v", parks, bad)
	}
	s.actMu.Lock()
	gate := s.activity[id].gate
	s.actMu.Unlock()
	if gate != nil {
		t.Fatal("gate left set")
	}
}

// listHook runs before at each List (the parker's notParkable), then lists the wrapped backend.
type listHook struct {
	*apPrepBackend
	before func()
}

func (l *listHook) List(ctx context.Context) ([]api.Device, error) {
	l.before()
	return l.apPrepBackend.List(ctx)
}

// A device the agent parked and whose next call finds the device preparing can not happen in a
// real backend (a parked device's boot is over); still, with auto-park on, the call must fail fast
// with code preparing before the wake: no resume is attempted.
func TestPreparingIsCheckedBeforeTheWake(t *testing.T) {
	const id = "android:agemu-p"
	b := &apPrepBackend{apBackend: newAPBackend(api.Android, "agemu-p"), preparing: map[string]bool{}}
	s := apService(t, time.Hour, time.Hour, b)
	b.apBackend.mu.Lock()
	b.devs[id].parked = true
	b.apBackend.mu.Unlock()
	b.setPreparing(id, true)
	_, err := s.Call(context.Background(), "screenshot", json.RawMessage(`{"device":"`+id+`"}`))
	if errorCode(err) != CodePreparing {
		t.Fatalf("err = %v, want code preparing", err)
	}
	if _, resumes, bad := b.counts(); resumes != 0 || len(bad) > 0 {
		t.Fatalf("resumes %d, bad %v", resumes, bad)
	}
}
