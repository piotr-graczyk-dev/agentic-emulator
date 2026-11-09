package android

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
	cancelled, err := d.cancelPrep("shutdown")
	if err != nil || !cancelled {
		t.Fatalf("cancelPrep = %v %v; a visible preparation before its save was cancelled", cancelled, err)
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

// delayedBoot decides after delay whether it needs a baseline (a retake is known only after a
// failed snapshot load), then either prepares (until release) or finishes.
func delayedBoot(delay time.Duration, need bool, release chan struct{}) func(ctx context.Context, d *device, id string, opts api.BootOptions) (api.Device, error) {
	return func(ctx context.Context, d *device, id string, opts api.BootOptions) (api.Device, error) {
		time.Sleep(delay)
		if !need {
			return api.Device{ID: id, State: "booted"}, nil
		}
		d.markNeeded()
		select {
		case <-release:
			return api.Device{ID: id, State: "booted"}, nil
		case <-ctx.Done():
			return api.Device{}, ctx.Err()
		}
	}
}

func TestACallDuringTheDecisionFailsFastOnceItIsAPreparation(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	b := New()
	b.bootHook = delayedBoot(200*time.Millisecond, true, release)
	go b.Boot(context.Background(), testID, api.BootOptions{NoWait: true})
	time.Sleep(20 * time.Millisecond) // the call arrives before the boot has decided
	t0 := time.Now()
	p := b.AwaitDecision(context.Background(), testID)
	if p == nil {
		t.Fatal("the call must learn that the device prepares a baseline")
	}
	if el := time.Since(t0); el > time.Second {
		t.Fatalf("waited %v; it must return once the boot decided (~0.2 s)", el)
	}
}

func TestACallDuringANormalBootWaitsForIt(t *testing.T) {
	b := New()
	b.bootHook = delayedBoot(150*time.Millisecond, false, nil)
	go b.Boot(context.Background(), testID, api.BootOptions{NoWait: true})
	time.Sleep(20 * time.Millisecond)
	if p := b.AwaitDecision(context.Background(), testID); p != nil {
		t.Fatalf("a boot from the baseline is no preparation: %+v", p)
	}
	if b.Preparing(testID) != nil {
		t.Fatal("still preparing")
	}
}

func TestAwaitDecisionWithoutABoot(t *testing.T) {
	b := New()
	t0 := time.Now()
	if b.AwaitDecision(context.Background(), testID) != nil || time.Since(t0) > 100*time.Millisecond {
		t.Fatal("no boot: no wait, no preparation")
	}
}

func TestDevicesFlagsARunningDeviceWithoutABaseline(t *testing.T) {
	t.Setenv("AGEMU_ANDROID_STANDBY", "0") // describe would ask the emulator about standby
	t.Setenv("AGEMU_HOME", t.TempDir())
	dir := t.TempDir()
	a := avdInfo{Name: "agemu-test-nobase", Dir: dir, Config: map[string]string{}}
	run := map[string]runningEmu{a.Name: {AVD: a.Name}}
	b := New()
	dev := b.describe(a, run)
	if dev.State != "booted" || !dev.NoBaseline || !strings.Contains(dev.PrepareError, "call boot") {
		t.Fatalf("running without a baseline: %+v", dev)
	}
	// With a baseline it is an ordinary booted device.
	if err := os.MkdirAll(filepath.Join(dir, "snapshots", baselineSnapshot), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "snapshots", baselineSnapshot, "snapshot.pb"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	dev = b.describe(a, run)
	if dev.NoBaseline || dev.PrepareError != "" {
		t.Fatalf("with a baseline: %+v", dev)
	}
	// Not running: nothing to flag.
	if dev := b.describe(avdInfo{Name: "agemu-test-off", Dir: t.TempDir(), Config: map[string]string{}}, nil); dev.NoBaseline {
		t.Fatalf("powered off: %+v", dev)
	}
}

// savingBoot marks the baseline saved, then waits to be cancelled (the steps after the save).
func savingBoot(saved chan struct{}) func(ctx context.Context, d *device, id string, opts api.BootOptions) (api.Device, error) {
	return func(ctx context.Context, d *device, id string, opts api.BootOptions) (api.Device, error) {
		d.markNeeded()
		d.markSaved()
		close(saved)
		<-ctx.Done()
		return api.Device{}, ctx.Err()
	}
}

func TestAShutdownAfterTheSaveIsNoFailure(t *testing.T) {
	saved := make(chan struct{})
	b := New()
	b.bootHook = savingBoot(saved)
	res := make(chan error, 1)
	go func() {
		_, err := b.Boot(context.Background(), testID, api.BootOptions{})
		res <- err
	}()
	<-saved
	d, _ := b.devFor(testID)
	cancelled, err := d.cancelPrep("shutdown")
	if err != nil || cancelled {
		t.Fatalf("cancelPrep = %v %v; after the save it is no cancelled preparation (standby may use the baseline)", cancelled, err)
	}
	if err := <-res; err != nil {
		t.Fatalf("the waiting boot got %v; the baseline was saved", err)
	}
	d.mu.Lock()
	perr := d.prepErr
	d.mu.Unlock()
	if perr != "" {
		t.Fatalf("prepareError %q after a saved baseline", perr)
	}
}

func TestANoteIsShownWhilePreparing(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	b := New()
	b.bootHook = func(ctx context.Context, d *device, id string, opts api.BootOptions) (api.Device, error) {
		d.markNeeded()
		d.setNote("an interrupted first boot is being repaired")
		<-release
		return api.Device{ID: id, State: "booted"}, nil
	}
	if _, err := b.Boot(context.Background(), testID, api.BootOptions{NoWait: true}); err != nil {
		t.Fatal(err)
	}
	if p := b.Preparing(testID); p == nil || !strings.Contains(p.Note, "repaired") {
		t.Fatalf("Preparing = %+v", p)
	}
}

// A blocking boot that prepared a baseline returns the device as it is after the preparation,
// not the `preparing` state bootSync read at its end while the preparation was still registered.
func TestABlockingBootReturnsTheStateAfterThePreparation(t *testing.T) {
	t.Setenv("AGEMU_ANDROID_STANDBY", "0")
	t.Setenv("AGEMU_HOME", t.TempDir())
	home := t.TempDir()
	t.Setenv("ANDROID_AVD_HOME", home)
	dir := filepath.Join(home, "agemu-test-prep.avd")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.ini"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "agemu-test-prep.ini"), []byte("path="+dir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	b := New()
	b.bootHook = func(ctx context.Context, d *device, id string, opts api.BootOptions) (api.Device, error) {
		d.markNeeded()
		return b.device(id) // as bootSync ends
	}
	dev, err := b.Boot(context.Background(), testID, api.BootOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if dev.State == api.StatePreparing || dev.Preparing != nil {
		t.Fatalf("a finished blocking boot returned %+v", dev)
	}
}
