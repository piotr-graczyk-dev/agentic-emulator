package android

import (
	"testing"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/api"
)

func TestAVDConfigRAMAndCores(t *testing.T) {
	prof := api.Profile{Name: "small-phone", Width: 720, Height: 1600, Density: 320}

	t.Setenv("AGEMU_ANDROID_RAM_MB", "")
	t.Setenv("AGEMU_ANDROID_CORES", "")
	c := avdConfig("agemu-x", prof)
	if c["hw.ramSize"] != "2048" {
		t.Errorf("default hw.ramSize = %q, want 2048", c["hw.ramSize"])
	}
	if _, ok := c["hw.cpu.ncore"]; ok {
		t.Errorf("hw.cpu.ncore set without AGEMU_ANDROID_CORES: %q (avdmanager's default must stay)", c["hw.cpu.ncore"])
	}
	if c["hw.lcd.width"] != "720" || c["hw.lcd.height"] != "1600" || c["hw.lcd.density"] != "320" || c["agemu.profile"] != "small-phone" {
		t.Errorf("display not taken from the profile: %v", c)
	}

	t.Setenv("AGEMU_ANDROID_RAM_MB", "1792")
	t.Setenv("AGEMU_ANDROID_CORES", "2")
	c = avdConfig("agemu-x", prof)
	if c["hw.ramSize"] != "1792" || c["hw.cpu.ncore"] != "2" {
		t.Errorf("hw.ramSize %q hw.cpu.ncore %q, want 1792 and 2", c["hw.ramSize"], c["hw.cpu.ncore"])
	}

	for _, bad := range []string{"0", "-1", "two", "9", "16"} {
		t.Setenv("AGEMU_ANDROID_CORES", bad)
		if v, ok := avdConfig("agemu-x", prof)["hw.cpu.ncore"]; ok {
			t.Errorf("AGEMU_ANDROID_CORES=%q set hw.cpu.ncore=%q", bad, v)
		}
	}
}

func TestCoresSetting(t *testing.T) {
	for v, want := range map[string]int{"": 0, "1": 1, "2": 2, " 3 ": 3, "8": 8} {
		t.Setenv("AGEMU_ANDROID_CORES", v)
		if n, err := coresSetting(); err != nil || n != want {
			t.Errorf("AGEMU_ANDROID_CORES=%q: got %d, %v; want %d", v, n, err, want)
		}
	}
	// Values the emulator cannot boot must make create fail, not produce an AVD that hangs.
	for _, bad := range []string{"0", "-1", "two", "9", "16"} {
		t.Setenv("AGEMU_ANDROID_CORES", bad)
		if n, err := coresSetting(); err == nil {
			t.Errorf("AGEMU_ANDROID_CORES=%q accepted as %d", bad, n)
		}
	}
}
