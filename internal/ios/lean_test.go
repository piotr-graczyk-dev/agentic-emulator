//go:build !windows

package ios

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestDisabledJobs(t *testing.T) {
	out := "\tdisabled services = {\n" +
		"\t\t\"com.apple.chronod\" => disabled\n" +
		"\t\t\"com.apple.NPKCompanionAgent\" => enabled\n" +
		"\t\t\"com.apple.triald\" => disabled\n" +
		"\t}\n"
	got := disabledJobs(out)
	if !got["com.apple.chronod"] || !got["com.apple.triald"] || got["com.apple.NPKCompanionAgent"] || len(got) != 2 {
		t.Fatalf("disabledJobs = %v", got)
	}
}

// launchArgs encodes ExtensionKit's -LaunchArguments for a provider, as iOS 26.5 does.
func launchArgs(service string) string {
	return "-LaunchArguments " + base64.StdEncoding.EncodeToString([]byte(`{"type":7,"serviceName":"`+service+`","enhancedSecurity":false}`))
}

func TestWallpaperExtensions(t *testing.T) {
	const udid = "ED6C55BD-83FB-49B9-93A2-54BA0AD521DD"
	const other = "18962024-0BC7-40CD-9598-F9C4C8DD49E9"
	// Runtime paths contain spaces ("iOS 26.5.simruntime"), so the command is not one field.
	rt := "/Library/Developer/CoreSimulator/Volumes/iOS_23F77/Library/Developer/CoreSimulator/Profiles/Runtimes/iOS 26.5.simruntime/Contents/Resources/RuntimeRoot"
	ps := fmt.Sprintf(`  100     1 launchd_sim /Users/me/Library/Developer/CoreSimulator/Devices/%[1]s/data/var/run/launchd_bootstrap.plist
  200     1 launchd_sim /Users/me/Library/Developer/CoreSimulator/Devices/%[2]s/data/var/run/launchd_bootstrap.plist
  101   100 %[3]s/Applications/PosterBoard.app/PosterBoard
  102   100 %[3]s/System/Library/ExtensionKit/Extensions/MercuryPosterExtension.appex/MercuryPosterExtension %[4]s
  103   100 %[3]s/Applications/EmojiPoster.app/Extensions/EmojiPosterExtension.appex/EmojiPosterExtension %[5]s
  104   100 %[3]s/System/Library/PrivateFrameworks/AmbientUI.framework/PlugIns/InfographPoster.appex/InfographPoster %[6]s
  105   100 %[3]s/System/Library/ExtensionKit/Extensions/WeatherPoster.appex/WeatherPoster
  106   100 %[3]s/Applications/MobileSafari.app/Extensions/SafariLinkExtension.appex/SafariLinkExtension %[4]s
  201   200 %[3]s/Applications/EmojiPoster.app/Extensions/EmojiPosterExtension.appex/EmojiPosterExtension %[5]s
  107   100 %[3]s/System/Library/CoreServices/AegirProxyApp.app/Extensions/AegirPoster.appex/AegirPoster %[7]s
  108   100 %[3]s/System/Library/Frameworks/ContactsUI.framework/PlugIns/MonogramPosterExtension.appex/MonogramPosterExtension %[8]s
  109   100 /Users/me/Library/Developer/CoreSimulator/Devices/%[1]s/data/Containers/Bundle/Application/0A1B/MyApp.app/PlugIns/MyPoster.appex/MyPoster %[9]s
  110   100 /Users/me/Library/Developer/CoreSimulator/Devices/%[1]s/data/Containers/Bundle/Application/0A1B/MyApp.app/Extensions/MyPoster.appex/MyPoster %[9]s
  300   102 %[3]s/System/Library/ExtensionKit/Extensions/ClockPosterExtension.appex/ClockPosterExtension
  400     1 /usr/bin/grep MercuryPosterExtension.appex/ %[1]s
`, udid, other, rt, launchArgs("com.apple.MercuryPoster"), launchArgs("com.apple.EmojiPoster.EmojiPosterExtension"),
		"-LaunchArguments "+base64.RawStdEncoding.EncodeToString([]byte(`{"serviceName":"com.apple.ambient.AmbientUI.InfographPoster"}`)),
		launchArgs("com.apple.NanoUniverse.AegirProxyApp.AegirPoster"), launchArgs("com.apple.ContactsUI.MonogramPosterExtension"),
		launchArgs("com.example.app.MyPoster"))
	exts, data := wallpaperExtensions(ps, udid)
	sort.Slice(exts, func(i, j int) bool { return exts[i].pid < exts[j].pid })
	// Only this device's launchd_sim children in the gallery's locations in the runtime: not
	// PosterBoard itself, not other extensions, not ContactsUI's poster, not an app's own
	// *Poster* extension, not the other simulator's, not grandchildren, not a host process
	// naming the path. Without launch arguments the provider is unknown ("").
	want := "[{102 com.apple.MercuryPoster} {103 com.apple.EmojiPoster.EmojiPosterExtension} {104 com.apple.ambient.AmbientUI.InfographPoster} {105 } {107 com.apple.NanoUniverse.AegirProxyApp.AegirPoster}]"
	if fmt.Sprint(exts) != want {
		t.Fatalf("wallpaperExtensions = %v, want %s", exts, want)
	}
	if want := "/Users/me/Library/Developer/CoreSimulator/Devices/" + udid + "/data"; data != want {
		t.Fatalf("data = %q, want %q", data, want)
	}
	if exts, data := wallpaperExtensions(ps, "NO-SUCH-UDID"); exts != nil || data != "" {
		t.Fatalf("unknown device: %v %q, want none", exts, data)
	}
}

func TestGalleryToStop(t *testing.T) {
	exts := []galleryExtension{
		{1, "com.apple.MercuryPoster"}, {2, "com.apple.EmojiPoster.EmojiPosterExtension"}, {3, ""},
		{4, "com.apple.AvatarUI.AvatarPosterExtension"}, {5, "com.apple.transcriptBackgroundPoster.DynamicExtension"},
		{6, "com.apple.WallpaperKit.CollectionsPoster"}, {7, "com.apple.PhotosUIPrivate.PhotosPosterProvider"},
	}
	// The data store not read yet: nothing.
	if got := galleryToStop(exts, nil); got != nil {
		t.Fatalf("keep unknown: %v, want none", got)
	}
	// Not the lock screen's provider, not an unknown one, not contact posters (Photos renders
	// photo contact posters too) or Messages backgrounds.
	got := galleryToStop(exts, map[string]bool{"com.apple.MercuryPoster": true})
	if want := "[{2 com.apple.EmojiPoster.EmojiPosterExtension} {6 com.apple.WallpaperKit.CollectionsPoster}]"; fmt.Sprint(got) != want {
		t.Fatalf("galleryToStop = %v, want %s", got, want)
	}
}

func TestNextGalleryPass(t *testing.T) {
	s := time.Second
	type step struct {
		known, stopped, settled bool
		want                    time.Duration
		more                    bool
	}
	for name, c := range map[string]struct {
		steps []step
	}{
		// A later boot: the store is known, the keynote mode settled and the 45 s pass stops
		// nothing, so it ends there.
		"known, quiet": {[]step{{true, true, true, 8 * s, true}, {true, false, true, 20 * s, true},
			{true, false, true, 45 * s, true}, {true, false, true, 0, false}}},
		// The 45 s pass still stopped something: one more pass 15 s later, until one stops nothing.
		"known, still starting": {[]step{{true, true, true, 8 * s, true}, {true, true, true, 20 * s, true},
			{true, true, true, 45 * s, true}, {true, true, true, 60 * s, true}, {true, false, true, 0, false}}},
		// A first boot: the lock screen's poster is written late, so the passes go on until it is.
		"first boot": {[]step{{false, false, true, 8 * s, true}, {false, false, true, 20 * s, true},
			{false, false, true, 45 * s, true}, {false, false, true, 60 * s, true}, {false, false, true, 75 * s, true},
			{true, true, true, 90 * s, true}, {true, false, true, 0, false}}},
		// PosterBoard is still building its gallery after 45 s (the keynote mode is not settled):
		// the passes go on until it is, although none stops anything.
		"gallery still building": {[]step{{true, false, false, 8 * s, true}, {true, false, false, 20 * s, true},
			{true, false, false, 45 * s, true}, {true, false, false, 60 * s, true}, {true, false, true, 0, false}}},
	} {
		at, more := nextGalleryPass(0, false, false, false)
		if at != 3*s || !more {
			t.Fatalf("%s: first pass %v %v, want 3s", name, at, more)
		}
		for i, st := range c.steps {
			at, more = nextGalleryPass(at, st.known, st.stopped, st.settled)
			if more != st.more || (more && at != st.want) {
				t.Fatalf("%s step %d: %v %v, want %v %v", name, i, at, more, st.want, st.more)
			}
		}
	}
	// Never past 5 minutes, even when the store is never written or the gallery never built.
	for _, known := range []bool{false, true} {
		at, more := 45*s, true
		n := 0
		for more {
			at, more = nextGalleryPass(at, known, false, !known)
			if more && at > galleryRetryUntil {
				t.Fatalf("pass at %v, after %v", at, galleryRetryUntil)
			}
			if n++; n > 100 {
				t.Fatal("passes never end")
			}
		}
	}
}

func TestStoppedSummary(t *testing.T) {
	got := stoppedSummary(map[string]int{"com.apple.EmojiPoster.EmojiPosterExtension": 1, "com.apple.Posters.WeatherPosterApp.WeatherPoster": 2})
	if want := "3 wallpaper gallery extensions (EmojiPoster.EmojiPosterExtension, Posters.WeatherPosterApp.WeatherPoster x2)"; got != want {
		t.Fatalf("stoppedSummary = %q, want %q", got, want)
	}
}

func TestPostersInUse(t *testing.T) {
	if _, err := os.Stat(sqlite3); err != nil {
		t.Skip("no " + sqlite3)
	}
	data := t.TempDir()
	if _, err := postersInUse(context.Background(), data); err == nil {
		t.Fatal("no data store: want an error")
	}
	store := func(version, sql string) {
		dir := filepath.Join(data, "Library/Application Support/PRBPosterExtensionDataStore", version)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		schema := `CREATE TABLE poster (posterId INTEGER PRIMARY KEY AUTOINCREMENT, UUID TEXT UNIQUE, providerId TEXT NOT NULL);
CREATE TABLE posterRoleMembership (posterUUID TEXT NOT NULL, roleId TEXT NOT NULL, roleSortKey INTEGER NOT NULL);`
		cmd := exec.Command(sqlite3, filepath.Join(dir, "PBFPosterExtensionDataStoreSQLiteDatabase.sqlite3"))
		cmd.Stdin = strings.NewReader(schema + sql)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("sqlite3: %v %s", err, out)
		}
	}
	// The layout iOS 26.5 wrote on a new iPhone 17 Pro (one lock screen poster, StandBy posters),
	// plus a contact poster made with the emoji provider (incoming call role).
	store("61", `INSERT INTO poster (UUID, providerId) VALUES ('a', 'com.apple.MercuryPoster'), ('b', 'com.apple.ambient.AmbientUI.InfographPoster'),
  ('d', 'com.apple.EmojiPoster.EmojiPosterExtension');
INSERT INTO posterRoleMembership VALUES ('a', 'PRPosterRoleLockScreen', 1), ('b', 'PRPosterRoleAmbient', 1), ('d', 'PRPosterRoleIncomingCall', 1);`)
	// An older schema's store is ignored.
	store("9", `INSERT INTO poster (UUID, providerId) VALUES ('c', 'com.apple.EmojiPoster.EmojiPosterExtension');
INSERT INTO posterRoleMembership VALUES ('c', 'PRPosterRoleLockScreen', 1);`)
	// A ~/.sqliterc that changes the output format must not matter.
	t.Setenv("HOME", t.TempDir())
	if err := os.WriteFile(filepath.Join(os.Getenv("HOME"), ".sqliterc"), []byte(".mode json\n.headers on\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	keep, err := postersInUse(context.Background(), data)
	if err != nil || fmt.Sprint(keep) != "map[com.apple.EmojiPoster.EmojiPosterExtension:true com.apple.MercuryPoster:true]" {
		t.Fatalf("postersInUse = %v, %v", keep, err)
	}
	// A new device before PosterBoard set up the lock screen: nothing to keep yet is an error,
	// so that nothing is stopped; a contact poster alone does not count.
	store("62", `INSERT INTO poster (UUID, providerId) VALUES ('d', 'com.apple.EmojiPoster.EmojiPosterExtension');
INSERT INTO posterRoleMembership VALUES ('d', 'PRPosterRoleIncomingCall', 1);`)
	if keep, err := postersInUse(context.Background(), data); err == nil {
		t.Fatalf("no lock screen poster: %v, want an error", keep)
	}
}

func TestLaunchService(t *testing.T) {
	for _, c := range []struct{ cmd, want string }{
		{"/x/MercuryPosterExtension " + launchArgs("com.apple.MercuryPoster"), "com.apple.MercuryPoster"},
		{"/x/MercuryPosterExtension", ""},
		{"/x/MercuryPosterExtension -LaunchArguments !!!", ""},
		{"/x/MercuryPosterExtension -LaunchArguments " + base64.StdEncoding.EncodeToString([]byte("not json")), ""},
	} {
		if got := launchService(c.cmd); got != c.want {
			t.Errorf("launchService(%q) = %q, want %q", c.cmd, got, c.want)
		}
	}
}

func TestLeanMismatches(t *testing.T) {
	all := map[string]bool{}
	for _, j := range leanJobs {
		all[j] = true
	}
	if m := leanMismatches(all, true); len(m) != 0 {
		t.Fatalf("all disabled, lean on: %v, want none", m)
	}
	if m := leanMismatches(all, false); len(m) != len(leanJobs) {
		t.Fatalf("all disabled, lean off: %d mismatches, want %d", len(m), len(leanJobs))
	}
	delete(all, leanJobs[3])
	if m := leanMismatches(all, true); fmt.Sprint(m) != "["+leanJobs[3]+"]" {
		t.Fatalf("one still enabled: %v", m)
	}
}

func TestClosedBackendStartsNothing(t *testing.T) {
	t.Setenv("AGEMU_IOS_PRESTART", "0")
	b := New()
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := b.helper(); err == nil {
		t.Fatal("helper() after Close started a helper")
	}
	b.leanDevice(simDevice{UDID: "U", Name: NamePrefix + "x"}, true, "")
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.leaning) != 0 || len(b.bootGen) != 0 {
		t.Fatalf("background work started after Close: leaning %v, bootGen %v", b.leaning, b.bootGen)
	}
}
