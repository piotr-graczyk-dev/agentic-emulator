package android

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/api"
)

// parkEnv points agemu's state and the emulators' discovery files at empty directories, so no
// emulator of the host is seen.
func parkEnv(t *testing.T) *Backend {
	t.Helper()
	t.Setenv("AGEMU_HOME", t.TempDir())
	t.Setenv("ANDROID_EMULATOR_DISCOVERY_DIR", t.TempDir())
	if err := os.MkdirAll(logDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	return New()
}

func TestParkedFollowsTheMarker(t *testing.T) {
	b := parkEnv(t)
	id := "android:agemu-parktest"
	if b.Parked(id) || b.Parked("ios:x") {
		t.Fatal("nothing is parked yet")
	}
	if err := os.WriteFile(parkMarker("agemu-parktest"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if !b.Parked(id) {
		t.Fatal("a device with a marker is parked")
	}
	if d := b.dev("agemu-parktest"); d.inStandby(context.Background()) {
		t.Fatal("a parked device is not in standby")
	}
}

func TestAParkedDeviceIsListedAsParked(t *testing.T) {
	b := parkEnv(t)
	a := avdInfo{Name: "agemu-parktest", Config: map[string]string{}}
	running := map[string]runningEmu{"agemu-parktest": {}}
	if st := b.describe(a, map[string]runningEmu{}).State; st != "shutdown" {
		t.Fatalf("not running: state %q", st)
	}
	if err := os.WriteFile(parkMarker("agemu-parktest"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// Without the marker check this would ask the (missing) emulator and say "booted".
	if st := b.describe(a, running).State; st != api.StateParked {
		t.Fatalf("parked: state %q", st)
	}
}

func TestResumeOfAnEmulatorThatWentAwayClearsTheMarker(t *testing.T) {
	b := parkEnv(t)
	id := "android:agemu-parktest"
	if err := os.WriteFile(parkMarker("agemu-parktest"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	resumed, err := b.Resume(context.Background(), id)
	if err != nil || resumed {
		t.Fatalf("resume = %v, %v; want nothing to resume", resumed, err)
	}
	if b.Parked(id) {
		t.Fatal("the stale marker must be gone")
	}
	// A device that is not parked: no-op.
	if resumed, err := b.Resume(context.Background(), id); err != nil || resumed {
		t.Fatalf("second resume = %v, %v", resumed, err)
	}
}

func TestParkNeedsARunningDevice(t *testing.T) {
	b := parkEnv(t)
	err := b.Park(context.Background(), "android:agemu-parktest")
	if err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("err = %v", err)
	}
	if b.Parked("android:agemu-parktest") {
		t.Fatal("a failed park must not leave a marker")
	}
}
