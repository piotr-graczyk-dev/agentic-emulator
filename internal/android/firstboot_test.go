package android

import (
	"os"
	"path/filepath"
	"testing"
)

func newAVDDir(t *testing.T) avdInfo {
	return avdInfo{Name: "agemu-test-fb", Dir: t.TempDir(), Config: map[string]string{}}
}

func touch(t *testing.T, path string) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func addBaseline(t *testing.T, a avdInfo) {
	touch(t, filepath.Join(a.Dir, "snapshots", baselineSnapshot, "snapshot.pb"))
}

// The normal life of a new AVD: marked before its first launch, unmarked once it booted.
func TestFirstBootMarkerLifecycle(t *testing.T) {
	a := newAVDDir(t)
	if needsWipe(a) {
		t.Fatal("a new AVD needs no wipe")
	}
	marked, err := markFirstBoot(a)
	if err != nil || !marked || !a.hasFirstBootMarker() {
		t.Fatalf("a never-started AVD is marked: %v %v", marked, err)
	}
	if !needsWipe(a) {
		t.Fatal("marked, no record, no baseline: the next boot repairs it")
	}
	touch(t, filepath.Join(a.Dir, userdataOverlay)) // the emulator started it
	if m, _ := markFirstBoot(a); !m {
		t.Fatal("an existing marker stays")
	}
	removed, err := recordBooted(a)
	if err != nil || !removed {
		t.Fatalf("recordBooted = %v %v", removed, err)
	}
	if a.hasFirstBootMarker() || !a.hasBootedRecord() || needsWipe(a) {
		t.Fatal("after a completed boot: record, no marker, never a wipe")
	}
	if m, _ := markFirstBoot(a); m || a.hasFirstBootMarker() {
		t.Fatal("an AVD that booted is never marked again")
	}
}

// An AVD that an emulator already started (an older agemu, Android Studio, a booted device whose
// record was lost) is never marked, so never wiped.
func TestAnAVDThatWasEverStartedIsNeverMarked(t *testing.T) {
	a := newAVDDir(t)
	touch(t, filepath.Join(a.Dir, userdataOverlay))
	if m, err := markFirstBoot(a); m || err != nil || a.hasFirstBootMarker() {
		t.Fatalf("marked %v %v", m, err)
	}
	if needsWipe(a) {
		t.Fatal("no marker: no wipe")
	}
}

func TestABaselineOrARecordPreventsTheWipe(t *testing.T) {
	// A marker left behind next to a baseline (a crash between the save and the record): no wipe.
	a := newAVDDir(t)
	touch(t, filepath.Join(a.Dir, firstBootMarker))
	addBaseline(t, a)
	if needsWipe(a) {
		t.Fatal("a baseline means the AVD booted: never wipe")
	}
	if m, _ := markFirstBoot(newWithBaseline(t)); m {
		t.Fatal("an AVD with a baseline is never marked")
	}
	// A marker next to a record (a crash between writing the record and removing the marker).
	b := newAVDDir(t)
	touch(t, filepath.Join(b.Dir, firstBootMarker))
	touch(t, filepath.Join(b.Dir, bootedRecord))
	if needsWipe(b) {
		t.Fatal("a completed-boot record means it booted: never wipe")
	}
	if removed, err := recordBooted(b); err != nil || !removed || b.hasFirstBootMarker() {
		t.Fatalf("the stale marker is removed on the next completed boot: %v %v", removed, err)
	}
}

func newWithBaseline(t *testing.T) avdInfo {
	a := newAVDDir(t)
	addBaseline(t, a)
	return a
}

func TestRecordBootedOnAnUnmarkedAVD(t *testing.T) {
	a := newAVDDir(t)
	touch(t, filepath.Join(a.Dir, userdataOverlay))
	removed, err := recordBooted(a)
	if err != nil || removed || !a.hasBootedRecord() {
		t.Fatalf("recordBooted = %v %v", removed, err)
	}
	if _, err := recordBooted(a); err != nil {
		t.Fatal("recordBooted is idempotent")
	}
	entries, _ := os.ReadDir(a.Dir)
	for _, e := range entries {
		if filepath.Ext(e.Name()) != "" && e.Name() != userdataOverlay && e.Name() != bootedRecord {
			t.Fatalf("a temporary file was left: %s", e.Name())
		}
	}
}

func TestWipeFlagOnlyWhenAsked(t *testing.T) {
	a := newAVDDir(t)
	p := ports{console: 5580, grpc: 8580}
	for _, arg := range emulatorArgs(a.Name, p, a, false, false) {
		if arg == "-wipe-data" {
			t.Fatal("-wipe-data without wipe")
		}
	}
	found := false
	for _, arg := range emulatorArgs(a.Name, p, a, false, true) {
		if arg == "-wipe-data" {
			found = true
		}
	}
	if !found {
		t.Fatal("no -wipe-data with wipe")
	}
}

func TestFirstBootRepairEnv(t *testing.T) {
	t.Setenv("AGEMU_ANDROID_FIRSTBOOT_REPAIR", "")
	if !firstBootRepair() {
		t.Fatal("on by default")
	}
	t.Setenv("AGEMU_ANDROID_FIRSTBOOT_REPAIR", "0")
	if firstBootRepair() {
		t.Fatal("0 turns it off")
	}
}

// Evidence of a booted guest that agemu sees outside its boot (the agent answered, for example on
// a device whose guest finished booting while no daemon watched it) records the completed boot,
// so a later cold boot or power-off never wipes it.
func TestAnAnsweringAgentRecordsTheFirstBoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ANDROID_AVD_HOME", home)
	dir := filepath.Join(home, "agemu-test-fb.avd")
	touch(t, filepath.Join(dir, "config.ini"))
	if err := os.WriteFile(filepath.Join(home, "agemu-test-fb.ini"), []byte("path="+dir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a, err := loadAVD("agemu-test-fb")
	if err != nil {
		t.Fatal(err)
	}
	if m, err := markFirstBoot(a); !m || err != nil {
		t.Fatalf("setup: marked %v %v", m, err)
	}
	recordBootedIfMarked("agemu-test-fb")
	if !a.hasBootedRecord() || a.hasFirstBootMarker() || needsWipe(a) {
		t.Fatal("after the agent answered: record, no marker, never a wipe")
	}
	recordBootedIfMarked("agemu-test-fb") // again, and for an unknown AVD: no effect, no error
	recordBootedIfMarked("agemu-no-such-avd")
	if !a.hasBootedRecord() || needsWipe(a) {
		t.Fatal("record lost")
	}
}

// Before a power-off of a marked AVD, only the emulator saying the paused guest has not booted
// keeps the AVD eligible for the repair. Any doubt records the boot: no automatic wipe of a guest
// that may hold data.
func TestFirstBootUnfinishedOnlyWhenThePausedEmulatorSaysSo(t *testing.T) {
	for _, c := range []struct {
		paused, known, booted, unfinished bool
		why                               string
	}{
		{true, true, false, true, "a bricked first boot: the paused emulator says its guest never booted"},
		{true, true, true, false, "the paused emulator says booted (the adb server may be down)"},
		{true, false, false, false, "paused but the emulator could not be asked: fail closed"},
		{false, true, false, false, "not paused: a not-booted answer could go stale before the power-off"},
		{false, false, false, false, "neither: fail closed"},
	} {
		if got := firstBootUnfinished(c.paused, c.known, c.booted); got != c.unfinished {
			t.Errorf("%s: firstBootUnfinished = %v, want %v", c.why, got, c.unfinished)
		}
	}
}
