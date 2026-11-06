package ios

import (
	"fmt"
	"sort"
	"testing"
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

func TestWallpaperExtensions(t *testing.T) {
	const udid = "ED6C55BD-83FB-49B9-93A2-54BA0AD521DD"
	const other = "18962024-0BC7-40CD-9598-F9C4C8DD49E9"
	// Runtime paths contain spaces ("iOS 26.5.simruntime"), so the command is not one field.
	rt := "/Library/Developer/CoreSimulator/Volumes/iOS_23F77/Library/Developer/CoreSimulator/Profiles/Runtimes/iOS 26.5.simruntime/Contents/Resources/RuntimeRoot"
	ps := fmt.Sprintf(`  100     1 launchd_sim /Users/me/Library/Developer/CoreSimulator/Devices/%[1]s/data/var/run/launchd_bootstrap.plist
  200     1 launchd_sim /Users/me/Library/Developer/CoreSimulator/Devices/%[2]s/data/var/run/launchd_bootstrap.plist
  101   100 %[3]s/Applications/PosterBoard.app/PosterBoard
  102   100 %[3]s/System/Library/ExtensionKit/Extensions/MercuryPosterExtension.appex/MercuryPosterExtension -LaunchArguments x
  103   100 %[3]s/System/Library/ExtensionKit/Extensions/WeatherPoster.appex/WeatherPoster
  104   100 %[3]s/System/Library/ExtensionKit/Extensions/SafariLinkExtension.appex/SafariLinkExtension
  201   200 %[3]s/System/Library/ExtensionKit/Extensions/WeatherPoster.appex/WeatherPoster
  300   102 %[3]s/System/Library/ExtensionKit/Extensions/ClockPosterExtension.appex/ClockPosterExtension
  400     1 /usr/bin/grep MercuryPosterExtension.appex/ %[1]s
`, udid, other, rt)
	got := wallpaperExtensions(ps, udid)
	sort.Ints(got)
	// Only this device's launchd_sim children: not PosterBoard itself, not other extensions,
	// not the other simulator's, not grandchildren, not a host process naming the path.
	if fmt.Sprint(got) != "[102 103]" {
		t.Fatalf("wallpaperExtensions = %v, want [102 103]", got)
	}
	if got := wallpaperExtensions(ps, "NO-SUCH-UDID"); got != nil {
		t.Fatalf("unknown device: %v, want none", got)
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
	b.leanDevice(simDevice{UDID: "U", Name: NamePrefix + "x"}, true)
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.leaning) != 0 || len(b.wallGen) != 0 {
		t.Fatalf("background work started after Close: leaning %v, wallGen %v", b.leaning, b.wallGen)
	}
}
