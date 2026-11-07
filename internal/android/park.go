package android

// Parking (agemu park / resume) pauses the VM in place. Unlike standby (shutdown), which
// restores the baseline before it pauses, the guest keeps everything: the apps, their
// processes and state, and the screen. A paused emulator uses no CPU; its guest RAM stays
// allocated, and the host can compress or swap it out when it needs the memory (measured in
// docs/ANDROID.md, "Parking"). Resume continues the VM and waits for the agent, as a boot from
// standby does.
//
// To the emulator a parked VM and one in standby are both paused. A marker file,
// <AGEMU_HOME>/android/<avd>.parked, tells them apart, also for a daemon that restarted.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/android/emupb"
)

func parkMarker(name string) string { return filepath.Join(logDir(), name+".parked") }

// parked reports whether the device's marker says it is parked.
func (d *device) parked() bool {
	_, err := os.Stat(parkMarker(d.name))
	return err == nil
}

func (d *device) clearPark() { _ = os.Remove(parkMarker(d.name)) }

// Park pauses a running device in place (see the comment at the top of this file).
func (b *Backend) Park(ctx context.Context, id string) error {
	d, err := b.devFor(id)
	if err != nil {
		return err
	}
	d.life.Lock()
	defer d.life.Unlock()
	if _, ok := findRunning(d.name); !ok {
		return fmt.Errorf("device android:%s is not running (agemu boot android:%s)", d.name, d.name)
	}
	if d.parked() {
		return nil
	}
	// conn refuses a paused VM: one in standby is shut down, not parkable.
	e, _, err := d.conn(ctx)
	if err != nil {
		return err
	}
	start := time.Now()
	// The marker goes first: a daemon that dies before the pause leaves a running device marked
	// parked, which the next call only resumes (a no-op pause), never one in standby unmarked.
	if err := os.MkdirAll(logDir(), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(parkMarker(d.name), nil, 0o644); err != nil {
		return err
	}
	if err := e.setVM(ctx, emupb.VmRunState_PAUSED); err != nil {
		d.clearPark()
		return fmt.Errorf("pausing the VM: %w", err)
	}
	d.mu.Lock()
	d.dispAt = time.Time{}
	d.mu.Unlock()
	logf("%s: parked (VM paused in place in %v)", d.name, time.Since(start).Round(time.Millisecond))
	return nil
}

// Parked reports whether the device is parked (one stat of its marker file).
func (b *Backend) Parked(id string) bool {
	name, err := avdName(id)
	if err != nil {
		return false
	}
	_, err = os.Stat(parkMarker(name))
	return err == nil
}

// Resume continues a parked device and returns once its agent answers. It reports whether the
// device was parked; on one that is not, it does nothing.
func (b *Backend) Resume(ctx context.Context, id string) (bool, error) {
	d, err := b.devFor(id)
	if err != nil {
		return false, err
	}
	if !d.parked() {
		return false, nil
	}
	d.life.Lock()
	defer d.life.Unlock()
	if !d.parked() {
		return false, nil
	}
	if _, ok := findRunning(d.name); !ok {
		// The emulator went away while parked: nothing to resume.
		d.clearPark()
		d.drop()
		return false, nil
	}
	start := time.Now()
	// The marker goes as soon as the VM runs: if the agent then does not answer, the device is
	// a running one with a problem, which every later call (shutdown and reset included) handles
	// as such, not a parked one that each call would try to resume again.
	if err := d.resume(ctx, d.clearPark); err != nil {
		return true, fmt.Errorf("resuming the parked VM: %w", err)
	}
	d.mu.Lock()
	d.agentOK = true
	d.mu.Unlock()
	logf("%s: resumed from park, agent answering after %v", d.name, time.Since(start).Round(time.Millisecond))
	return true, nil
}
