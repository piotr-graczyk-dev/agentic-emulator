package ios

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// posterBoardExport is `defaults export com.apple.PosterBoard.unprotectedUserDefaults -` on a new
// iPhone 17 Pro (iOS 26.5, build 23F77) after its second boot, with KEYNOTE_MODE when keynote is
// true and one more update entry when extra is not "".
func posterBoardExport(keynote bool, extra string) string {
	mode := ""
	if keynote {
		mode = "\t<key>KEYNOTE_MODE</key>\n\t<true/>\n"
	}
	if extra != "" {
		extra = "\t\t<string>" + extra + "</string>\n"
	}
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CompletedPosterBundleIdentifierMigrations</key>
	<array>
		<string>com.apple.Posters.UnityPosterApp.ExtragalacticPoster</string>
		<string>com.apple.Posters.WeatherPosterApp.WeatherPoster</string>
	</array>
` + mode + `	<key>PBFExecuteUpdateHistory</key>
	<array>
		<string>2026-11-07 11:47:46 | prewarm | 15.81s | PBFDescriptorRefreshStrategyRefreshIfEmpty | PBFPosterExtensionDataStoreGalleryConfigurationOptionsPersistAndUse | PBFPosterExtensionDataStoreGalleryConfigurationOptionsOnlyIfStale | SUCCESS</string>
		<string>2026-11-07 11:50:39 | prewarm | 29.86s | PBFDescriptorRefreshStrategyRefreshIfEmpty | PBFPosterExtensionDataStoreGalleryConfigurationOptionsPersistAndUse | PBFPosterExtensionDataStoreGalleryConfigurationOptionsOnlyIfStale | SUCCESS</string>
` + extra + `	</array>
	<key>PBF_LOCALE_DID_CHANGE</key>
	<false/>
	<key>PBF_LOCALE_IDENTIFIER</key>
	<string>en_PL</string>
	<key>SnapshotsBuildVersion</key>
	<string>23F77</string>
	<key>StashedDeviceType</key>
	<string>iPhone18,1</string>
</dict>
</plist>
`
}

const (
	secondBootUpdate = "2026-11-07 11:50:39 | prewarm | 29.86s | PBFDescriptorRefreshStrategyRefreshIfEmpty | PBFPosterExtensionDataStoreGalleryConfigurationOptionsPersistAndUse | PBFPosterExtensionDataStoreGalleryConfigurationOptionsOnlyIfStale | SUCCESS"
	keynoteUpdate    = "2026-11-07 11:57:31 | prewarm | 0.00s | PBFDescriptorRefreshStrategyRefreshIfEmpty | PBFPosterExtensionDataStoreGalleryConfigurationOptionsPersistAndUse | PBFPosterExtensionDataStoreGalleryConfigurationOptionsOnlyIfStale | ERROR: The operation couldn’t be completed. Keynote mode is enabled; we aren't going to prewarm anything.  We are already warm enough, thank you."
)

func TestParsePosterBoardPrefs(t *testing.T) {
	s, err := parsePosterBoardPrefs([]byte(posterBoardExport(false, "")))
	if err != nil {
		t.Fatal(err)
	}
	if s.frozen || s.lastUpdate != secondBootUpdate || s.snapshotsBuild != "23F77" {
		t.Fatalf("parsePosterBoardPrefs = %+v", s)
	}
	s, err = parsePosterBoardPrefs([]byte(posterBoardExport(true, keynoteUpdate)))
	if err != nil || !s.frozen || s.lastUpdate != keynoteUpdate {
		t.Fatalf("keynote mode on: %+v, %v", s, err)
	}
	// What `defaults export` prints for a domain that does not exist yet (a new device).
	s, err = parsePosterBoardPrefs([]byte("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<plist version=\"1.0\">\n<dict/>\n</plist>\n"))
	if err != nil || s != (posterBoardState{}) {
		t.Fatalf("empty domain: %+v, %v", s, err)
	}
	// A <false/> mode, values of other types and nested dictionaries are read or skipped.
	s, err = parsePosterBoardPrefs([]byte(`<plist version="1.0"><dict>
		<key>Nested</key><dict><key>KEYNOTE_MODE</key><true/></dict>
		<key>KEYNOTE_MODE</key><false/>
		<key>Count</key><integer>3</integer>
		<key>PBFExecuteUpdateHistory</key><array><dict/><string>only</string></array>
	</dict></plist>`))
	if err != nil || s.frozen || s.lastUpdate != "only" {
		t.Fatalf("mixed types: %+v, %v", s, err)
	}
	for _, bad := range []string{"", "not xml", "<plist><array/></plist>", "<plist><dict><key>KEYNOTE_MODE</key>"} {
		if _, err := parsePosterBoardPrefs([]byte(bad)); err == nil {
			t.Errorf("parsePosterBoardPrefs(%q): want an error", bad)
		}
	}
	stash, err := plistStrings([]byte(`<plist version="1.0"><dict><key>stashedBuildVersionString</key><string>23F77</string></dict></plist>`))
	if err != nil || stash["stashedBuildVersionString"] != "23F77" {
		t.Fatalf("plistStrings = %v, %v", stash, err)
	}
}

func TestGalleryBuilt(t *testing.T) {
	built := posterBoardState{lastUpdate: secondBootUpdate, snapshotsBuild: "23F77", stashedBuild: "23F77"}
	for _, c := range []struct {
		name   string
		s      posterBoardState
		before string
		want   bool
	}{
		{"first boot, built", built, "", true},
		{"later boot, built in this boot", built, "2026-11-07 11:47:46 | prewarm | 15.81s | x | y | z | SUCCESS", true},
		{"later boot, not yet updated in this boot", built, secondBootUpdate, false},
		{"first boot, nothing yet", posterBoardState{}, "", false},
		{"failed update", posterBoardState{lastUpdate: keynoteUpdate, snapshotsBuild: "23F77", stashedBuild: "23F77"}, "", false},
		// After an iOS update: the store is migrated to the new build, the providers not yet
		// refreshed for it.
		{"providers not refreshed for the build", posterBoardState{lastUpdate: secondBootUpdate, snapshotsBuild: "23G5", stashedBuild: "23F77"}, "", false},
		{"no stashed build", posterBoardState{lastUpdate: secondBootUpdate, snapshotsBuild: "23F77"}, "", false},
	} {
		if got := galleryBuilt(c.s, c.before); got != c.want {
			t.Errorf("%s: galleryBuilt = %v, want %v", c.name, got, c.want)
		}
	}
}

// fakePosterBoard is a posterBoardPrefsEnv whose state follows the writes.
type fakePosterBoard struct {
	s                 posterBoardState
	readErr, writeErr error
	ignoreWrites      bool
	reads             int
	writes            []bool
}

func (f *fakePosterBoard) state() (posterBoardState, error) {
	f.reads++
	return f.s, f.readErr
}

func (f *fakePosterBoard) frozen() (bool, error) {
	f.reads++
	return f.s.frozen, f.readErr
}

func (f *fakePosterBoard) setFrozen(on bool) error {
	f.writes = append(f.writes, on)
	if f.writeErr == nil && !f.ignoreWrites {
		f.s.frozen = on
	}
	return f.writeErr
}

func TestFreezeGalleryStep(t *testing.T) {
	built := posterBoardState{lastUpdate: secondBootUpdate, snapshotsBuild: "23F77", stashedBuild: "23F77"}
	// Still building: nothing written, the passes go on.
	f := &fakePosterBoard{s: posterBoardState{snapshotsBuild: "23F77", stashedBuild: "23F77"}}
	if freezeGalleryStep(f, "", "agemu-test") || len(f.writes) != 0 {
		t.Fatalf("not built: done, writes %v; want not done, no write", f.writes)
	}
	// The update newest before the boot is not this boot's.
	f = &fakePosterBoard{s: built}
	if freezeGalleryStep(f, secondBootUpdate, "agemu-test") || len(f.writes) != 0 {
		t.Fatalf("old update: writes %v, want none", f.writes)
	}
	// Built in this boot: turned on, read back.
	f = &fakePosterBoard{s: built}
	if !freezeGalleryStep(f, "", "agemu-test") || len(f.writes) != 1 || !f.writes[0] || !f.s.frozen || f.reads != 2 {
		t.Fatalf("built: writes %v, frozen %v, reads %d; want one write, on, read back", f.writes, f.s.frozen, f.reads)
	}
	// Already on (an earlier boot): done, nothing written.
	f = &fakePosterBoard{s: posterBoardState{frozen: true}}
	if !freezeGalleryStep(f, "", "agemu-test") || len(f.writes) != 0 {
		t.Fatalf("already on: writes %v", f.writes)
	}
	// A failed read or write, or a write that did not stick, ends this boot's tries.
	for name, f := range map[string]*fakePosterBoard{
		"read fails":  {s: built, readErr: errors.New("spawn failed")},
		"write fails": {s: built, writeErr: errors.New("spawn failed")},
		"write lost":  {s: built, ignoreWrites: true},
	} {
		if !freezeGalleryStep(f, "", "agemu-test") {
			t.Errorf("%s: want done", name)
		}
	}
}

func TestUnfreezeGallery(t *testing.T) {
	f := &fakePosterBoard{s: posterBoardState{frozen: true}}
	if err := unfreezeGallery(f, "agemu-test"); err != nil || f.s.frozen || len(f.writes) != 1 || f.writes[0] {
		t.Fatalf("on: %v, frozen %v, writes %v; want it deleted", err, f.s.frozen, f.writes)
	}
	f = &fakePosterBoard{}
	if err := unfreezeGallery(f, "agemu-test"); err != nil || len(f.writes) != 0 || f.reads != 1 {
		t.Fatalf("off: %v, writes %v, reads %d; want one read, nothing written", err, f.writes, f.reads)
	}
	f = &fakePosterBoard{s: posterBoardState{frozen: true}, ignoreWrites: true}
	if err := unfreezeGallery(f, "agemu-test"); err == nil {
		t.Fatal("a delete that did not stick: want an error")
	}
	f = &fakePosterBoard{readErr: errors.New("spawn failed")}
	if err := unfreezeGallery(f, "agemu-test"); err == nil {
		t.Fatal("read fails: want an error")
	}
}

func TestGalleryUpdateBeforeBoot(t *testing.T) {
	if _, err := exec.LookPath("plutil"); err != nil {
		t.Skip("no plutil")
	}
	data := t.TempDir()
	if got := galleryUpdateBeforeBoot(data); got != "" {
		t.Fatalf("no preferences yet: %q", got)
	}
	if got := galleryUpdateBeforeBoot(""); got != "" {
		t.Fatalf("no data path: %q", got)
	}
	// cfprefsd writes binary property lists.
	prefs := filepath.Join(data, "Library/Preferences")
	if err := os.MkdirAll(prefs, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(prefs, posterBoardPrefs+".plist")
	if err := os.WriteFile(file, []byte(posterBoardExport(false, "")), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("plutil", "-convert", "binary1", file).CombinedOutput(); err != nil {
		t.Fatalf("plutil: %v %s", err, out)
	}
	if got := galleryUpdateBeforeBoot(data); got != secondBootUpdate {
		t.Fatalf("galleryUpdateBeforeBoot = %q, want %q", got, secondBootUpdate)
	}
	if err := os.WriteFile(file, []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := galleryUpdateBeforeBoot(data); got != "" {
		t.Fatalf("unreadable file: %q, want none", got)
	}
}

func TestPosterBoardStateNeedsTheBuild(t *testing.T) {
	// The stash domain's build is what galleryBuilt compares; a prefs export alone leaves it
	// empty, so it never counts as built.
	s, err := parsePosterBoardPrefs([]byte(posterBoardExport(false, "")))
	if err != nil || galleryBuilt(s, "") || !strings.HasSuffix(s.lastUpdate, "SUCCESS") {
		t.Fatalf("prefs alone: %+v %v", s, err)
	}
}
