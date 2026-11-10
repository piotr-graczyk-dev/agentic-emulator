//go:build !windows

package ios

import (
	"bytes"
	"context"
	"log"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseAnimations(t *testing.T) {
	for _, c := range []struct {
		in   string
		want float64
		bad  bool
	}{
		// Normal speed: the same words as AGEMU_ANDROID_ANIMATIONS.
		{"", 0, false}, {"1", 0, false}, {"normal", 0, false}, {"ON", 0, false}, {"true", 0, false},
		{"yes", 0, false}, {"1.0", 0, false},
		// Off as on Android: as fast as iOS allows.
		{"0", fastCoefficient, false}, {"off", fastCoefficient, false}, {"false", fastCoefficient, false},
		{"No", fastCoefficient, false}, {"fast", fastCoefficient, false}, {" Fast ", fastCoefficient, false},
		{"0.0", fastCoefficient, false}, {"0.00", fastCoefficient, false},
		{"0.25", 0.25, false}, {"0.05", 0.05, false},
		// Below the smallest measured factor, above 1 (slower), not a number.
		{"0.01", 0, true}, {"0.049", 0, true}, {"-1", 0, true}, {"2", 0, true}, {"slow", 0, true},
		{"NaN", 0, true}, {"nan", 0, true}, {"Inf", 0, true}, {"-Inf", 0, true},
	} {
		got, err := parseAnimations(c.in)
		if (err != nil) != c.bad || got != c.want {
			t.Errorf("parseAnimations(%q) = %g, %v; want %g, error %v", c.in, got, err, c.want, c.bad)
		}
	}
}

func TestParseDefaultsFloat(t *testing.T) {
	if got := parseDefaultsFloat("0.1\n"); got != 0.1 {
		t.Errorf("0.1 -> %g", got)
	}
	missing := "2026-11-06 19:40:43.712 defaults[82742:12384860] \nThe domain/default pair of (com.apple.UIKit, UIAnimationDragCoefficient) does not exist\n"
	if got := parseDefaultsFloat(missing); got != 0 {
		t.Errorf("missing key -> %g, want 0", got)
	}
	for _, s := range []string{"nan", "inf", "-0.5"} {
		if got := parseDefaultsFloat(s); got != 0 {
			t.Errorf("%s -> %g, want 0", s, got)
		}
	}
}

func TestSameCoefficient(t *testing.T) {
	// `defaults write -float` stores a 32-bit float; a read-back may carry its rounding.
	if !sameCoefficient(0.1, float64(float32(0.1))) {
		t.Error("0.1 and float32(0.1) differ")
	}
	if sameCoefficient(0.1, 0.05) || sameCoefficient(0, 0.05) {
		t.Error("different coefficients compare equal")
	}
}

// A helper that does not report the data directory cannot apply the setting: that is logged
// once per device (not on every launch), and unmanaged devices are never touched.
func TestAnimateDeviceWithoutDataPath(t *testing.T) {
	saved := animationsEnv
	defer func() { animationsEnv = saved }()
	animationsEnv = func() float64 { return fastCoefficient }
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	b := &Backend{anim: map[string]float64{}}
	d := simDevice{UDID: "U1", Name: "agemu-test"}
	b.animateDevice(context.Background(), d, false)
	b.animateDevice(context.Background(), d, false)
	if n := strings.Count(buf.String(), "not applied"); n != 1 {
		t.Errorf("logged %d times, want once: %q", n, buf.String())
	}
	buf.Reset()
	b.animateDevice(context.Background(), simDevice{UDID: "U2", Name: "iPhone 17 Pro"}, false)
	if buf.Len() != 0 {
		t.Errorf("unmanaged device: %q", buf.String())
	}
	if _, ok := b.anim["U2"]; ok {
		t.Error("unmanaged device recorded")
	}
	if math.IsNaN(b.anim["U1"]) {
		t.Error("NaN recorded")
	}
}

func TestSetUIPreferences(t *testing.T) {
	if _, err := exec.LookPath("plutil"); err != nil {
		t.Skip("plutil not available")
	}
	path := uikitFlagsPath(t.TempDir())
	if uiPreferencesOn(path) {
		t.Fatal("no file: flag reported on")
	}
	if err := setUIPreferences(path, false); err != nil {
		t.Fatalf("removing from a missing file: %v", err)
	}
	if err := setUIPreferences(path, true); err != nil {
		t.Fatal(err)
	}
	if !uiPreferencesOn(path) {
		t.Fatal("flag not on after setting it")
	}
	if err := setUIPreferences(path, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file left behind after removing the only entry: %v", err)
	}

	// Other overrides in the file are kept.
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	other := strings.Replace(emptyPlist, "<dict/>", "<dict><key>other_flag</key><dict><key>Enabled</key><false/></dict></dict>", 1)
	if err := os.WriteFile(path, []byte(other), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := setUIPreferences(path, true); err != nil {
		t.Fatal(err)
	}
	if err := setUIPreferences(path, false); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("plutil", "-convert", "json", "-o", "-", path).Output()
	if err != nil || string(out) != `{"other_flag":{"Enabled":false}}` {
		t.Fatalf("other overrides: %s, %v", out, err)
	}
}
