package ios

import (
	"errors"
	"fmt"
	"sort"
	"testing"
	"time"
)

// rtRoot is the iOS 26.5 runtime's root as ps shows it: the path has spaces ("iOS
// 26.5.simruntime"), so a command line is not one field.
const rtRoot = "/Library/Developer/CoreSimulator/Volumes/iOS_23F77/Library/Developer/CoreSimulator/Profiles/Runtimes/iOS 26.5.simruntime/Contents/Resources/RuntimeRoot"

func TestBackgroundToStop(t *testing.T) {
	const udid = "ED6C55BD-83FB-49B9-93A2-54BA0AD521DD"
	const other = "18962024-0BC7-40CD-9598-F9C4C8DD49E9"
	rm := rtRoot + "/System/Library/PrivateFrameworks/RemoteManagement.framework"
	app := "/Users/me/Library/Developer/CoreSimulator/Devices/" + udid + "/data/Containers/Bundle/Application/0A1B/MyApp.app"
	ps := fmt.Sprintf(`  100     1 launchd_sim /Users/me/Library/Developer/CoreSimulator/Devices/%[1]s/data/var/run/launchd_bootstrap.plist
  200     1 launchd_sim /Users/me/Library/Developer/CoreSimulator/Devices/%[2]s/data/var/run/launchd_bootstrap.plist
  101   100 %[3]s/Applications/MobileCal.app/MobileCal
  102   100 %[3]s/Applications/MobileCal.app/Extensions/CalendarIntentsExtension.appex/CalendarIntentsExtension -LaunchArguments eyJ9
  103   100 %[3]s/Applications/MobileSafari.app/Extensions/SafariLinkExtension.appex/SafariLinkExtension -LaunchArguments eyJ9
  104   100 %[3]s/Applications/MobileSMS.app/Extensions/MessagesActionExtension.appex/MessagesActionExtension -LaunchArguments eyJ9
  105   100 %[5]s/remotemanagementd
  106   100 %[5]s/XPCServices/AccountSubscriber.xpc/AccountSubscriber
  107   100 %[5]s/XPCServices/LegacyProfilesSubscriber.xpc/LegacyProfilesSubscriber
  108   100 %[5]s/XPCServices/InteractiveLegacyProfilesSubscriber.xpc/InteractiveLegacyProfilesSubscriber
  109   100 %[5]s/XPCServices/SoftwareUpdateSubscriber.xpc/SoftwareUpdateSubscriber
  110   100 %[3]s/Applications/Spotlight.app/Spotlight
  111   100 %[3]s/Applications/WidgetRenderer_Default.app/WidgetRenderer_Default
  112   100 %[3]s/Applications/MobileCal.app/MobileCalHelper
  113   100 %[5]s/RemoteManagementAgent
  114   100 %[4]s/Extensions/SafariLinkExtension.appex/SafariLinkExtension
  115   100 /usr/bin/grep %[3]s/Applications/MobileCal.app/MobileCal
  116   100 %[5]s/XPCServices/MDMAppSubscriber.xpc/MDMAppSubscriber
  117   100 %[5]s/XPCServices/PowerSubscriber.xpc/PowerSubscriber
  118   100 %[5]s/XPCServices/AccountSubscriberHelper.xpc/AccountSubscriberHelper
  201   200 %[3]s/Applications/MobileCal.app/MobileCal
  300   101 %[3]s/Applications/MobileSafari.app/Extensions/SafariLinkExtension.appex/SafariLinkExtension
`, udid, other, rtRoot, app, rm)
	procs, _ := simChildren(ps, udid)
	stop := backgroundToStop(procs)
	sort.Slice(stop, func(i, j int) bool { return stop[i].pid < stop[j].pid })
	// Only this device's launchd_sim children running the listed runtime executables: not
	// Spotlight or the widget renderer, not remotemanagementd or its agent, not the subscribers
	// never seen after a boot (MDMApp, Power, SoftwareUpdate) or another service of the
	// framework, not a longer name, not an app's own extension of the same name, not a process
	// naming the path, not the other simulator's, not grandchildren.
	var got []string
	for _, s := range stop {
		got = append(got, fmt.Sprintf("%d %s %q", s.pid, s.name, s.app))
	}
	want := `[101 MobileCal "com.apple.mobilecal" 102 CalendarIntentsExtension "" 103 SafariLinkExtension "" ` +
		`104 MessagesActionExtension "" 106 AccountSubscriber "" 107 LegacyProfilesSubscriber "" ` +
		`108 InteractiveLegacyProfilesSubscriber ""]`
	if fmt.Sprint(got) != want {
		t.Fatalf("backgroundToStop = %v,\nwant %s", got, want)
	}
}

func TestBackgroundSubscribersAreTheObservedOnes(t *testing.T) {
	n := 0
	for _, p := range backgroundProcesses {
		if len(p.exe) > len(remoteManagement) && p.exe[:len(remoteManagement)] == remoteManagement {
			n++
		}
	}
	if n != 10 {
		t.Fatalf("%d device management subscribers listed, want the 10 seen after a boot", n)
	}
}

func TestOlderThanAPass(t *testing.T) {
	seen := map[int]bool{}
	pass := func(pids ...int) string {
		var running []runningBackground
		for _, p := range pids {
			running = append(running, runningBackground{pid: p})
		}
		var old []int
		for _, p := range olderThanAPass(running, seen) {
			old = append(old, p.pid)
		}
		return fmt.Sprint(old)
	}
	// A process is stopped at the pass after the one that first found it; one started again
	// (a new pid) waits for the next pass again.
	for i, c := range []struct {
		pids []int
		want string
	}{
		{nil, "[]"},
		{[]int{10, 11}, "[]"},
		{[]int{10, 11, 12}, "[10 11]"},
		{[]int{12, 13}, "[12]"},
		{[]int{13}, "[13]"},
	} {
		if got := pass(c.pids...); got != c.want {
			t.Fatalf("pass %d with %v: stop %s, want %s", i, c.pids, got, c.want)
		}
	}
}

// fakeBackground is a backgroundEnv whose passes see procs[i] at pass i.
type fakeBackground struct {
	procs [][]simProc
	// beforePass runs at the start of pass i (after the wait), e.g. to boot the device again.
	beforePass func(i int)
	isCurrent  func() bool
	// front is the frontmost pid at pass i (missing: 1, SpringBoard); frontErr fails the lookup.
	front    map[int]int
	frontErr error
	closed   bool

	pass                 int
	waits                []time.Duration
	listed               []int
	killed, terminated   []string
	killedAt, terminAt   []int
	frontmostAsked       int
	processesAfterClosed bool
}

func (f *fakeBackground) waitUntil(at time.Duration) bool {
	f.waits = append(f.waits, at)
	f.pass = len(f.waits) - 1
	if f.closed {
		return false
	}
	if f.beforePass != nil {
		f.beforePass(f.pass)
	}
	return true
}

func (f *fakeBackground) current() bool {
	if f.isCurrent == nil {
		return true
	}
	return f.isCurrent()
}

func (f *fakeBackground) processes() ([]simProc, error) {
	f.listed = append(f.listed, f.pass)
	if f.pass < len(f.procs) {
		return f.procs[f.pass], nil
	}
	return nil, nil
}

func (f *fakeBackground) frontmostPid() (int, error) {
	f.frontmostAsked++
	if f.frontErr != nil {
		return 0, f.frontErr
	}
	if p, ok := f.front[f.pass]; ok {
		return p, nil
	}
	return 1, nil
}

func (f *fakeBackground) terminate(bundleID string) error {
	f.terminated = append(f.terminated, bundleID)
	f.terminAt = append(f.terminAt, f.pass)
	return nil
}

func (f *fakeBackground) kill(pid int) error {
	f.killed = append(f.killed, fmt.Sprint(pid))
	f.killedAt = append(f.killedAt, f.pass)
	return nil
}

func runtimeProc(pid int, exe string) simProc { return simProc{pid, rtRoot + "/" + exe} }

var (
	calendar  = runtimeProc(101, "Applications/MobileCal.app/MobileCal")
	safariExt = runtimeProc(103, "Applications/MobileSafari.app/Extensions/SafariLinkExtension.appex/SafariLinkExtension -LaunchArguments eyJ9")
	account   = runtimeProc(106, remoteManagement+"XPCServices/AccountSubscriber.xpc/AccountSubscriber")
)

func TestBackgroundPassesSchedule(t *testing.T) {
	f := &fakeBackground{}
	backgroundPasses(f, "agemu-test")
	want := "[1m0s 1m30s 2m0s 2m30s 3m0s 3m30s 4m0s 4m30s 5m0s]"
	if fmt.Sprint(f.waits) != want {
		t.Fatalf("passes at %v, want %s", f.waits, want)
	}
}

func TestBackgroundPassesEndOnNewerBoot(t *testing.T) {
	t.Setenv("AGEMU_IOS_PRESTART", "0")
	b := New()
	defer b.Close()
	const udid = "U"
	// This boot is generation 1; at pass 2 the device is cold-booted again (leanDevice counts
	// it), so pass 2 and later act on nothing: the newer boot has its own passes.
	b.bootGen[udid] = 1
	every := []simProc{safariExt, account}
	f := &fakeBackground{
		procs: [][]simProc{every, every, every, every},
		beforePass: func(i int) {
			if i == 2 {
				b.mu.Lock()
				b.bootGen[udid]++
				b.mu.Unlock()
			}
		},
		isCurrent: func() bool { return b.currentBoot(udid, 1) },
	}
	stopped := backgroundPasses(f, "agemu-test")
	// Pass 0 sees them, pass 1 stops them (same pids), pass 2 finds a newer boot and ends.
	if fmt.Sprint(f.listed) != "[0 1]" || fmt.Sprint(f.killed) != "[103 106]" || fmt.Sprint(f.killedAt) != "[1 1]" {
		t.Fatalf("listed at passes %v, killed %v at %v; want listing at [0 1] and 103, 106 killed at pass 1", f.listed, f.killed, f.killedAt)
	}
	if len(f.waits) != 3 {
		t.Fatalf("%d waits, want 3: the passes end at the first one after the newer boot", len(f.waits))
	}
	if fmt.Sprint(stopped) != "map[AccountSubscriber:1 SafariLinkExtension:1]" {
		t.Fatalf("stopped %v", stopped)
	}
}

func TestBackgroundPassesEndWhenClosed(t *testing.T) {
	f := &fakeBackground{procs: [][]simProc{{account}, {account}}, closed: true}
	backgroundPasses(f, "agemu-test")
	if len(f.listed) != 0 || len(f.killed) != 0 {
		t.Fatalf("closed backend: listed %v, killed %v", f.listed, f.killed)
	}
}

func TestBackgroundPassesSkipFrontmostCalendar(t *testing.T) {
	// Calendar (pid 101) runs from pass 0 on and is frontmost at pass 1 (opened by the user or a
	// test), then goes to the background: it stays, also at later passes, because a pass found
	// it in front. The extension next to it is stopped at pass 1 whatever is frontmost.
	both := []simProc{calendar, safariExt}
	f := &fakeBackground{
		procs: [][]simProc{both, both, {calendar}, {calendar}, {calendar}},
		front: map[int]int{1: 101},
	}
	backgroundPasses(f, "agemu-test")
	if len(f.terminated) != 0 {
		t.Fatalf("terminated %v at passes %v, want Calendar kept: it was in front at pass 1", f.terminated, f.terminAt)
	}
	if fmt.Sprint(f.killed) != "[103]" || fmt.Sprint(f.killedAt) != "[1]" {
		t.Fatalf("killed %v at %v, want 103 at pass 1", f.killed, f.killedAt)
	}
	// In front only when the first pass found it: kept too.
	f = &fakeBackground{procs: [][]simProc{{calendar}, {calendar}, {calendar}}, front: map[int]int{0: 101}}
	backgroundPasses(f, "agemu-test")
	if len(f.terminated) != 0 {
		t.Fatalf("in front at pass 0: terminated %v, want none", f.terminated)
	}
}

func TestBackgroundPassesStopBackgroundCalendar(t *testing.T) {
	// Calendar opened in the background (by calaccessd) and never in front: terminated through
	// CoreSimulator at the pass after the one that first found it, never with SIGTERM. A new
	// Calendar process (pid 102) opened later and in front at its first pass stays.
	later := runtimeProc(102, "Applications/MobileCal.app/MobileCal")
	f := &fakeBackground{
		procs: [][]simProc{{calendar}, {calendar}, {later}, {later}, {later}},
		front: map[int]int{2: 102},
	}
	backgroundPasses(f, "agemu-test")
	if fmt.Sprint(f.terminated) != "[com.apple.mobilecal]" || fmt.Sprint(f.terminAt) != "[1]" {
		t.Fatalf("terminated %v at passes %v, want com.apple.mobilecal at pass 1 only", f.terminated, f.terminAt)
	}
	if len(f.killed) != 0 {
		t.Fatalf("killed %v: an app is terminated through CoreSimulator, not with SIGTERM", f.killed)
	}
}

func TestBackgroundPassesKeepCalendarWhenFrontmostUnknown(t *testing.T) {
	// When the frontmost app cannot be told (accessibility not answering), Calendar is left
	// running; other processes are stopped as usual.
	both := []simProc{calendar, account}
	f := &fakeBackground{procs: [][]simProc{both, both, {calendar}}, frontErr: errors.New("accessibility did not answer")}
	backgroundPasses(f, "agemu-test")
	if len(f.terminated) != 0 || f.frontmostAsked == 0 {
		t.Fatalf("terminated %v (frontmost asked %d times), want none", f.terminated, f.frontmostAsked)
	}
	if fmt.Sprint(f.killed) != "[106]" {
		t.Fatalf("killed %v, want the subscriber", f.killed)
	}
	// A frontmost pid of 0 (no app) is unknown too.
	f = &fakeBackground{procs: [][]simProc{both, both}, front: map[int]int{1: 0}}
	backgroundPasses(f, "agemu-test")
	if len(f.terminated) != 0 {
		t.Fatalf("frontmost 0: terminated %v, want none", f.terminated)
	}
	// Unknown at one pass, known (SpringBoard in front) at the later ones: Calendar may have
	// been in front at that pass, so it stays for the rest of the boot.
	f = &fakeBackground{procs: [][]simProc{{calendar}, {calendar}, {calendar}, {calendar}}, front: map[int]int{1: 0}}
	backgroundPasses(f, "agemu-test")
	if len(f.terminated) != 0 {
		t.Fatalf("frontmost unknown at pass 1 only: terminated %v at passes %v, want none", f.terminated, f.terminAt)
	}
}
