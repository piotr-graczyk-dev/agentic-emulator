package android

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A boot holds the device's life lock for its whole run (a baseline preparation and a first-boot
// repair included): Park waits for it only until its context ends, so the daemon's auto-park (a
// 30 s context) never hangs behind a preparation of several minutes and never pauses a booting
// device.
func TestParkGivesUpWhileABootHoldsTheDevice(t *testing.T) {
	t.Setenv("AGEMU_HOME", t.TempDir())
	b := &Backend{devs: map[string]*device{}}
	d := b.dev("agemu-x")
	d.life.Lock() // bootSync runs
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := b.Park(ctx, "android:agemu-x")
	if err == nil || !strings.Contains(err.Error(), "waiting for its boot") {
		t.Fatalf("Park during a boot = %v, want it to give up waiting", err)
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("Park waited %v past its context", el)
	}
	d.life.Unlock()
}

// A preparation that is visible (the device is listed as preparing) is refused even when the park
// gets the lock (defence in depth: the lock is the boot's, so this needs a preparation that
// outlived it).
func TestParkRefusesAPreparingDevice(t *testing.T) {
	t.Setenv("AGEMU_HOME", t.TempDir())
	b := &Backend{devs: map[string]*device{}}
	d := b.dev("agemu-x")
	p := newPrep(func() {})
	p.visible.Store(true)
	d.mu.Lock()
	d.prep = p
	d.mu.Unlock()
	err := b.Park(context.Background(), "android:agemu-x")
	if err == nil || !strings.Contains(err.Error(), "preparing") {
		t.Fatalf("Park of a preparing device = %v", err)
	}
	if b.Parked("android:agemu-x") {
		t.Fatal("a preparing device was marked parked")
	}
}
