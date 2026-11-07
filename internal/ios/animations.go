package ios

// Fast animations (opt-in, AGEMU_IOS_ANIMATIONS). UIKit multiplies the duration (and the delay)
// of its animations by UIAnimationDragCoefficient(): UIView animations, navigation push and
// pop, tab switches, alerts, SwiftUI animations. Sheets and the share sheet do not go through
// it on iOS 26.5 (measured). Simulator.app's Debug > Slow Animations makes the function return
// 10 (it sets the com.apple.UIKit.SimulatorSlowMotionAnimationState notification state). The
// function also returns UIKit's internal preference of the same name when that is above 0,
// read from the com.apple.UIKit defaults domain; but UIKit reads its internal preferences
// only on internal builds or with the UIKit/UIPreferences feature flag on
// (allowInternalPreferences: os_variant_has_internal_diagnostics || _os_feature_enabled_impl
// ("UIKit", "UIPreferences"), read from the iOS 26.5 UIKitCore disassembly). So the default
// alone has no effect. agemu turns the feature flag on with an override plist in the device's
// data directory (Library/Preferences/FeatureFlags/Domain/UIKit.plist, which libfeatureflags
// reads in the simulator), writes the coefficient with `defaults` inside the device, and posts
// com.apple.UIKit.InternalPreferences, on which running UIKit processes re-read it.
//
// A process reads the feature flag when it starts, so apps launched after the setting is
// applied use it; SpringBoard (home screen, app launch zoom, permission alerts) uses it from
// the next boot. Turning the setting off deletes the coefficient (running processes go back
// to normal speed at once) and removes the feature flag override.

import (
	"context"
	"fmt"
	"log"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

const (
	// fastCoefficient is AGEMU_IOS_ANIMATIONS=0 (or fast). UIKit treats a coefficient of 0 as
	// unset, so iOS cannot turn animations off the way Android's scales of 0 do. Of 0.2, 0.1
	// and 0.05, 0.05 gave the shortest transitions and was not slower than normal speed in any
	// measured one; 0.01 made Settings' back slower than normal speed (docs/IOS.md).
	fastCoefficient = 0.05
	// minCoefficient is the smallest factor AGEMU_IOS_ANIMATIONS accepts: the smallest one
	// measured not to be slower than normal speed.
	minCoefficient = 0.05
	// uikitInternalPrefsChanged makes running UIKit processes re-read internal preferences.
	uikitInternalPrefsChanged = "com.apple.UIKit.InternalPreferences"
	dragCoefficientKey        = "UIAnimationDragCoefficient"
)

// parseAnimations maps an AGEMU_IOS_ANIMATIONS value to a drag coefficient; 0 means the stock
// speed (nothing set in the device). The words are AGEMU_ANDROID_ANIMATIONS's: unset, 1, on,
// true or yes is normal speed; 0 (also written 0.0), off, false or no makes animations as short
// as iOS allows (fastCoefficient; "fast" is the same). A number between minCoefficient and 1 is
// the factor UIKit multiplies durations by.
func parseAnimations(v string) (float64, error) {
	s := strings.ToLower(strings.TrimSpace(v))
	switch s {
	case "", "1", "on", "true", "yes", "normal":
		return 0, nil
	case "0", "off", "false", "no", "fast":
		return fastCoefficient, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err == nil && f == 0 {
		return fastCoefficient, nil // "0.0" is "0"
	}
	// Written as a negated range so that NaN is rejected too.
	if err != nil || !(f >= minCoefficient && f <= 1) {
		return 0, fmt.Errorf("AGEMU_IOS_ANIMATIONS=%q: want 0 (as fast as iOS allows), 1 (normal) or a factor between %g and 1", v, minCoefficient)
	}
	if f == 1 {
		return 0, nil
	}
	return f, nil
}

var animationsEnv = sync.OnceValue(func() float64 {
	c, err := parseAnimations(os.Getenv("AGEMU_IOS_ANIMATIONS"))
	if err != nil {
		log.Printf("ios: %v; animations stay at normal speed", err)
	}
	return c
})

// sameCoefficient compares coefficients read back from `defaults` (stored as a float).
func sameCoefficient(a, b float64) bool {
	return math.Abs(a-b) < 1e-6
}

// uikitFlagsPath is the device's feature flag override file for the UIKit domain.
func uikitFlagsPath(dataPath string) string {
	return filepath.Join(dataPath, "Library/Preferences/FeatureFlags/Domain/UIKit.plist")
}

// animateDevice brings an agemu simulator's animation speed to AGEMU_IOS_ANIMATIONS. It runs
// after each boot agemu makes, once the device is ready and before boot returns (`defaults`
// needs a booted device, so SpringBoard picks the setting up from the next boot), and before
// the first launch this daemon makes on a device (an app launched after it gets the setting);
// later calls return at once. A failure is logged and leaves the device unchecked, so the next
// boot or launch tries again.
func (b *Backend) animateDevice(ctx context.Context, d simDevice, cold bool) {
	if !managed(d.Name) {
		return
	}
	want := animationsEnv()
	b.mu.Lock()
	got, checked := b.anim[d.UDID]
	b.mu.Unlock()
	if checked && got == want && !cold {
		return
	}
	if d.DataPath == "" {
		// A helper older than the daemon does not report the data directory. Logged once per
		// device and daemon: retrying cannot help until the helper is rebuilt.
		if want > 0 {
			log.Printf("ios animations %s: AGEMU_IOS_ANIMATIONS not applied: agemu-ios-helper did not report the device's data directory (rebuild it with make build)", d.Name)
		}
		b.mu.Lock()
		b.anim[d.UDID] = want
		b.mu.Unlock()
		return
	}
	if err := b.applyAnimations(ctx, d, want); err != nil {
		log.Printf("ios animations %s: AGEMU_IOS_ANIMATIONS not applied: %v", d.Name, err)
		return
	}
	b.mu.Lock()
	b.anim[d.UDID] = want
	b.mu.Unlock()
}

// applyAnimations sets (want > 0) or removes (want == 0) the feature flag and the coefficient.
func (b *Backend) applyAnimations(ctx context.Context, d simDevice, want float64) error {
	flags := uikitFlagsPath(d.DataPath)
	flagOn := uiPreferencesOn(flags)
	if want == 0 && !flagOn {
		// Without the flag UIKit ignores the coefficient: the default costs no call into the
		// device.
		return nil
	}
	cur, err := b.dragCoefficient(ctx, d.UDID)
	if err != nil {
		return err
	}
	if want > 0 && flagOn && sameCoefficient(cur, want) {
		return nil
	}
	if want > 0 && !flagOn {
		if err := setUIPreferences(flags, true); err != nil {
			return err
		}
	}
	if !sameCoefficient(cur, want) {
		args := []string{"delete", "com.apple.UIKit", dragCoefficientKey}
		if want > 0 {
			args = []string{"write", "com.apple.UIKit", dragCoefficientKey, "-float", strconv.FormatFloat(want, 'g', -1, 64)}
		}
		if _, err := b.spawn(ctx, d.UDID, "usr/bin/defaults", args...); err != nil {
			return err
		}
		if err := b.call(ctx, "notify", map[string]any{"udid": d.UDID, "name": uikitInternalPrefsChanged}, nil); err != nil {
			return err
		}
		if now, err := b.dragCoefficient(ctx, d.UDID); err != nil {
			return err
		} else if !sameCoefficient(now, want) {
			return fmt.Errorf("%s is %g after writing %g", dragCoefficientKey, now, want)
		}
	}
	if want == 0 && flagOn {
		if err := setUIPreferences(flags, false); err != nil {
			return err
		}
	}
	if want > 0 {
		log.Printf("ios animations %s: UIKit animation coefficient %g (apps launched from now on; SpringBoard from the next boot)", d.Name, want)
	} else {
		log.Printf("ios animations %s: back to normal speed", d.Name)
	}
	return nil
}

// dragCoefficient reads the coefficient agemu set in the device; 0 when it is not set.
func (b *Backend) dragCoefficient(ctx context.Context, udid string) (float64, error) {
	out, err := b.spawn(ctx, udid, "usr/bin/defaults", "read", "com.apple.UIKit", dragCoefficientKey)
	if err != nil {
		return 0, err
	}
	return parseDefaultsFloat(out), nil
}

// parseDefaultsFloat parses `defaults read` output: a number, or an error text ("The domain/
// default pair ... does not exist") when the key is not set, which reads as 0.
func parseDefaultsFloat(out string) float64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(out), 64)
	if err != nil || !(f >= 0) || math.IsInf(f, 0) {
		return 0
	}
	return f
}

// spawn runs a program of the simulator's runtime in the device and returns its output.
func (b *Backend) spawn(ctx context.Context, udid, path string, args ...string) (string, error) {
	var r struct {
		Stdout string `json:"stdout"`
	}
	err := b.call(ctx, "spawn", map[string]any{"udid": udid, "path": path, "args": args, "timeoutMs": 10000}, &r)
	return r.Stdout, err
}

// uiPreferencesOn reports whether the override file turns UIKit/UIPreferences on.
func uiPreferencesOn(path string) bool {
	if _, err := os.Stat(path); err != nil {
		return false
	}
	out, err := exec.Command("plutil", "-extract", "UIPreferences.Enabled", "raw", "-o", "-", path).Output()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

// setUIPreferences adds or removes the UIPreferences entry of the device's UIKit feature flag
// overrides, keeping any other entry; a file left empty is removed.
func setUIPreferences(path string, on bool) error {
	run := func(args ...string) error {
		if out, err := exec.Command("plutil", args...).CombinedOutput(); err != nil {
			return fmt.Errorf("plutil %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	if on {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if _, err := os.Stat(path); os.IsNotExist(err) {
			if err := os.WriteFile(path, []byte(emptyPlist), 0o644); err != nil {
				return err
			}
		}
		return run("-replace", "UIPreferences", "-json", `{"Enabled":true}`, path)
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	}
	if err := run("-remove", "UIPreferences", path); err != nil {
		return err
	}
	if out, err := exec.Command("plutil", "-convert", "json", "-o", "-", path).Output(); err == nil && strings.TrimSpace(string(out)) == "{}" {
		return os.Remove(path)
	}
	return nil
}

const emptyPlist = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict/>
</plist>
`
