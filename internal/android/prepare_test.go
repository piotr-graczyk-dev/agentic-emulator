package android

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/api"
)

// fakeBoot is a bootSync stand-in: it marks the boot as preparing when told to, then waits for
// release or cancellation.
type fakeBoot struct {
	needBaseline bool
	release      chan struct{}
	started      chan struct{}
}

func newFakeBoot(need bool) *fakeBoot {
	return &fakeBoot{needBaseline: need, release: make(chan struct{}), started: make(chan struct{}, 4)}
}

func (f *fakeBoot) boot(ctx context.Context, d *device, id string, opts api.BootOptions) (api.Device, error) {
	f.started <- struct{}{}
	if f.needBaseline {
		d.markNeeded()
		d.setPhase(phaseSettling)
	}
	select {
	case <-f.release:
		return api.Device{ID: id, State: "booted"}, nil
	case <-ctx.Done():
		return api.Device{}, ctx.Err()
	}
}

func testBackend(f *fakeBoot) *Backend {
	b := New()
	b.bootHook = f.boot
	return b
}

const testID = "android:agemu-test-prep"

func TestBootFromBaselineReturnsBooted(t *testing.T) {
	f := newFakeBoot(false)
	b := testBackend(f)
	close(f.release) // a normal boot from the baseline finishes at once
	dev, err := b.Boot(context.Background(), testID, api.BootOptions{NoWait: true})
	if err != nil || dev.State != "booted" {
		t.Fatalf("got %+v %v, want booted", dev, err)
	}
	if b.Preparing(testID) != nil {
		t.Fatal("a finished boot must not show as preparing")
	}
}

func TestNoWaitReturnsPreparingAndJoins(t *testing.T) {
	f := newFakeBoot(true)
	b := testBackend(f)
	t0 := time.Now()
	dev, err := b.Boot(context.Background(), testID, api.BootOptions{NoWait: true})
	if err != nil {
		t.Fatal(err)
	}
	if dev.State != api.StatePreparing || dev.Preparing == nil || dev.Preparing.Phase == "" {
		t.Fatalf("got %+v, want preparing with a phase", dev)
	}
	if time.Since(t0) > 2*time.Second {
		t.Fatalf("NoWait took %v; it must return once the baseline is known to be needed", time.Since(t0))
	}
	p := b.Preparing(testID)
	if p == nil || p.Phase != phaseSettling || p.MaxS <= 0 {
		t.Fatalf("Preparing = %+v", p)
	}
	// A second NoWait boot answers with the state at once and starts nothing.
	dev2, err := b.Boot(context.Background(), testID, api.BootOptions{NoWait: true})
	if err != nil || dev2.State != api.StatePreparing {
		t.Fatalf("second boot: %+v %v", dev2, err)
	}
	if len(f.started) != 1 {
		t.Fatalf("boots started: %d, want 1", len(f.started))
	}
	// A waiting boot joins and returns the result once the boot is done.
	res := make(chan api.Device, 1)
	go func() {
		d, _ := b.Boot(context.Background(), testID, api.BootOptions{})
		res <- d
	}()
	select {
	case <-res:
		t.Fatal("a waiting boot returned before the boot finished")
	case <-time.After(100 * time.Millisecond):
	}
	close(f.release)
	select {
	case d := <-res:
		if d.State != "booted" {
			t.Fatalf("joined boot got %+v", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("joined boot did not return")
	}
	if b.Preparing(testID) != nil {
		t.Fatal("still preparing after the boot finished")
	}
}

func TestNoWaitBound(t *testing.T) {
	old := noWaitBound
	noWaitBound = 50 * time.Millisecond
	defer func() { noWaitBound = old }()
	f := newFakeBoot(false) // a slow boot that never says it needs a baseline
	b := testBackend(f)
	dev, err := b.Boot(context.Background(), testID, api.BootOptions{NoWait: true})
	if err != nil || dev.State != api.StatePreparing {
		t.Fatalf("after the bound: %+v %v, want preparing", dev, err)
	}
	if b.Preparing(testID) == nil {
		t.Fatal("a boot a NoWait caller left must show as preparing (other calls fail at once)")
	}
	close(f.release)
}

func TestCancelPrep(t *testing.T) {
	f := newFakeBoot(true)
	b := testBackend(f)
	if _, err := b.Boot(context.Background(), testID, api.BootOptions{NoWait: true}); err != nil {
		t.Fatal(err)
	}
	d, _ := b.devFor(testID)
	if err := d.cancelPrep("shutdown"); err != nil {
		t.Fatal(err)
	}
	if b.Preparing(testID) != nil {
		t.Fatal("still preparing after cancel")
	}
	d.mu.Lock()
	perr := d.prepErr
	d.mu.Unlock()
	if !strings.Contains(perr, "cancelled by shutdown") {
		t.Fatalf("prepErr %q", perr)
	}
	// The next boot starts a new preparation.
	f2 := newFakeBoot(false)
	b.bootHook = f2.boot
	close(f2.release)
	if dev, err := b.Boot(context.Background(), testID, api.BootOptions{NoWait: true}); err != nil || dev.State != "booted" {
		t.Fatalf("boot after cancel: %+v %v", dev, err)
	}
}

func TestCloseCancelsPreparation(t *testing.T) {
	f := newFakeBoot(true)
	b := testBackend(f)
	res := make(chan error, 1)
	go func() {
		_, err := b.Boot(context.Background(), testID, api.BootOptions{})
		res <- err
	}()
	<-f.started
	_ = b.Close()
	select {
	case err := <-res:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not stop the preparation")
	}
}

func TestPreparingErrorText(t *testing.T) {
	e := &api.PreparingError{Device: testID, Phase: phaseSettling, Elapsed: 192, MaxS: 840}
	msg := e.Error()
	for _, want := range []string{testID, "settling", "192s", "14 min", "poll `devices`", "boot"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q lacks %q", msg, want)
		}
	}
	if !errors.Is(e, api.ErrPreparing) {
		t.Fatal("PreparingError must match ErrPreparing")
	}
}
