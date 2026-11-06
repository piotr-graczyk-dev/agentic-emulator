package android

// Lean devices. The Play system image starts Google apps an app under test does not need; on
// a cold boot they kept the emulator at about 2 cores for a minute and held about 120 MB of the
// guest's 2 GB. agemu disables them (`pm disable-user`, like a user turning an app off in
// Settings) before it takes the baseline snapshot, so every boot and reset starts without them.
// Google Play services, the Play Store (Play Billing, Play Integrity), WebView, the keyboard, the
// launcher and the system UI stay. AGEMU_ANDROID_LEAN=0 keeps them all; it applies when the
// baseline is taken (a new device, or a retake).

import (
	"context"
	"os"
	"strings"
	"time"
)

// leanPackages are the apps a lean device has turned off.
var leanPackages = []string{
	"com.google.android.apps.messaging",       // Messages
	"com.google.android.apps.photos",          // Photos (media scanning, backup)
	"com.google.android.apps.restore",         // device setup restore
	"com.google.android.googlequicksearchbox", // the Google app (Assistant, Discover)
	"com.google.android.as",                   // Android System Intelligence
	"com.google.android.apps.wellbeing",       // Digital Wellbeing
	"com.google.android.youtube",
	"com.google.android.apps.youtube.music",
	"com.google.android.gm",           // Gmail
	"com.google.android.apps.maps",    // the Maps app (the Maps SDK lives in Play services)
	"com.google.android.calendar",     // the Calendar app (the calendar provider stays)
	"com.google.android.apps.docs",    // Drive
	"com.google.android.apps.tachyon", // Meet
	"com.google.android.apps.turbo",   // Device Health Services
}

func leanOn() bool {
	switch strings.ToLower(os.Getenv("AGEMU_ANDROID_LEAN")) {
	case "0", "false", "no":
		return false
	}
	return true
}

// disableLeanPackages turns the lean packages off for user 0. A package the image does not
// have is skipped.
func (d *device) disableLeanPackages(ctx context.Context, serial string) {
	n := 0
	for _, p := range leanPackages {
		out, err := adbShellOK(ctx, serial, "pm disable-user --user 0 "+p)
		if err == nil && strings.Contains(out, "disabled-user") {
			n++
		}
	}
	logf("%s: turned off %d of %d apps an app under test does not need (AGEMU_ANDROID_LEAN=0 keeps them)", d.name, n, len(leanPackages))
}

// rendererMismatch reports the emulator refusing a snapshot taken with another -gpu mode
// ("Snapshot load failure: different renderer configured").
func rendererMismatch(err error) bool {
	return err != nil && strings.Contains(err.Error(), "different renderer")
}

// animationsOff: AGEMU_ANDROID_ANIMATIONS=0 sets the system's window, transition and animator
// duration scales to 0 after every boot and reset, as Espresso setups do. Screen changes then
// finish at once: the agent step (find, tap, wait for the next screen, screenshot) took 357-379
// ms instead of 867-899 ms. Apps behave as on a phone with animations turned off in the
// developer options, so it is off by default.
func animationsOff() bool {
	switch strings.ToLower(os.Getenv("AGEMU_ANDROID_ANIMATIONS")) {
	case "0", "off", "false", "no":
		return true
	}
	return false
}

// applyAnimations sets the animation scales for AGEMU_ANDROID_ANIMATIONS=0. A reset restores the
// baseline's settings, so it runs after every boot, resume and reset. adb reconnects 0.2-0.6 s
// after a snapshot load (the agent answers sooner, over its own connection), so it retries.
func (d *device) applyAnimations(ctx context.Context) {
	if !animationsOff() {
		return
	}
	d.mu.Lock()
	serial := d.run.serial()
	d.mu.Unlock()
	cmd := "settings put global window_animation_scale 0; settings put global transition_animation_scale 0; settings put global animator_duration_scale 0"
	// Bounded as a whole: the caller holds the device's lifecycle lock, and an adb read without
	// a deadline could block.
	actx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for {
		_, err := adbShellOK(actx, serial, cmd)
		if err == nil {
			return
		}
		select {
		case <-actx.Done():
			logf("%s: could not turn animations off: %v", d.name, err)
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
}
