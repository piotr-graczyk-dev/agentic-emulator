//go:build !windows

package ios

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestIdleExitable(t *testing.T) {
	for flags, want := range map[uint32]bool{
		0x0: false, // not tracked (apps, backboardd)
		0x1: false, // tracked, no idle exit (SpringBoard)
		0x3: true,  // tracked, idle exit allowed, clean
		0x7: false, // dirty: a transaction is open
		0x5: false,
		0xb: false, // being launched (PROC_DIRTY_LAUNCH_IN_PROGRESS)
	} {
		if got := idleExitable(flags); got != want {
			t.Errorf("idleExitable(%#x) = %v, want %v", flags, got, want)
		}
	}
}

func TestIdleSnapshotOf(t *testing.T) {
	const udid = "ED6C55BD-83FB-49B9-93A2-54BA0AD521DD"
	app := "/Users/me/Library/Developer/CoreSimulator/Devices/" + udid + "/data/Containers/Bundle/Application/0A1B/MyApp.app"
	ps := fmt.Sprintf(`  100     1 launchd_sim /Users/me/Library/Developer/CoreSimulator/Devices/%[1]s/data/var/run/launchd_bootstrap.plist
  101   100 %[2]s/System/Library/PrivateFrameworks/EmailDaemon.framework/maild
  102   100 %[2]s/usr/libexec/securityd
  103   100 %[3]s/MyApp
  104   100 %[2]s/Applications/MobileSafari.app/Extensions/SafariLinkExtension.appex/SafariLinkExtension -LaunchArguments eyJ9
  105   100 /usr/bin/grep %[2]s/usr/libexec/securityd
  106   100 %[2]s/usr/libexec/gone
  107   100 %[3]s/PlugIns/Widget.appex/Widget -arg x
  108   100 %[2]s/usr/libexec/extensionkitservice %[3]s/PlugIns/Widget.appex
`, udid, rtRoot, app)
	root, procs, _ := simRootChildren(ps, udid)
	if root != 100 {
		t.Fatalf("root = %d", root)
	}
	starts := map[int]int64{101: 10, 102: 20, 103: 300, 104: 30, 105: 40, 107: 250, 108: 260}
	usage := func(pid int) (procUsage, error) {
		s, ok := starts[pid]
		if !ok {
			return procUsage{}, syscall.ESRCH
		}
		return procUsage{cpu: uint64(pid) * 1000, start: s}, nil
	}
	dirty := func(pid int) (uint32, error) {
		if pid == 104 {
			return 0, syscall.ESRCH
		}
		return 0x3, nil
	}
	snap := idleSnapshotOf(root, procs, 0, dirty, usage)
	sort.Slice(snap.daemons, func(i, j int) bool { return snap.daemons[i].pid < snap.daemons[j].pid })
	var got []string
	for _, d := range snap.daemons {
		got = append(got, fmt.Sprintf("%d %s %d", d.pid, d.name, d.usage.start))
	}
	// Runtime executables only (not the app or its extension, not a process naming the path),
	// with usage (106 has none) and readable flags (104 exited); a runtime process whose argument
	// names an app's extension is still a runtime process. firstApp is the earliest app process.
	if want := "[101 maild 10 102 securityd 20 108 extensionkitservice 260]"; fmt.Sprint(got) != want {
		t.Fatalf("daemons = %v, want %s", got, want)
	}
	if snap.firstApp != 250 || snap.root != 100 {
		t.Fatalf("firstApp = %d, root = %d; want 250, 100", snap.firstApp, snap.root)
	}
}

// fakeIdle replays scripted snapshots, one per pass, and records signals.
type fakeIdle struct {
	passes   []idleSnapshot
	pass     int          // passes started so far (waitUntil calls)
	heldAt   map[int]bool // passes (1-based) at which the device is held
	recheckF func(p daemonProc) (gone, killable bool)
	signals  []string
	stopAt   int // current() turns false at this pass (0: never)
	// lockedAt: passes (1-based) at which a park or a shutdown holds the device (exclusive fails);
	// exclusiveHeld is true between exclusive and its release.
	lockedAt      map[int]bool
	exclusiveHeld bool
	snapErrAt     int // the snapshot of this pass (1-based) fails, as deviceIdle's does after a reboot
}

func (f *fakeIdle) waitUntil(time.Duration) bool { f.pass++; return true }
func (f *fakeIdle) current() bool                { return f.stopAt == 0 || f.pass < f.stopAt }
func (f *fakeIdle) held() bool                   { return f.heldAt[f.pass] }
func (f *fakeIdle) snapshot() (idleSnapshot, error) {
	if f.snapErrAt != 0 && f.pass == f.snapErrAt {
		return idleSnapshot{}, idleBootCheck(100, 200)
	}
	if f.pass > len(f.passes) {
		return idleSnapshot{root: 1}, nil
	}
	return f.passes[f.pass-1], nil
}
func (f *fakeIdle) recheck(p daemonProc) (bool, bool) {
	if f.recheckF != nil {
		return f.recheckF(p)
	}
	return false, true // the same idle process: SIGKILL
}
func (f *fakeIdle) exclusive() (func(), bool) {
	if f.lockedAt[f.pass] {
		return nil, false
	}
	f.exclusiveHeld = true
	return func() { f.exclusiveHeld = false }, true
}
func (f *fakeIdle) signal(pid int, sig syscall.Signal) error {
	if !f.exclusiveHeld {
		f.signals = append(f.signals, fmt.Sprintf("%d@%d outside exclusive", pid, f.pass))
	}
	f.signals = append(f.signals, fmt.Sprintf("%d@%d %v", pid, f.pass, map[syscall.Signal]string{syscall.SIGTERM: "TERM", syscall.SIGKILL: "KILL"}[sig]))
	return nil
}

// dp is a daemon with flags, CPU time and start time 1 (before any app).
func dp(pid int, name string, flags uint32, cpu uint64) daemonProc {
	return daemonProc{pid: pid, name: name, flags: flags, usage: procUsage{cpu: cpu, start: 1}}
}

func snaps(ps ...[]daemonProc) []idleSnapshot {
	var s []idleSnapshot
	for _, p := range ps {
		s = append(s, idleSnapshot{root: 1, daemons: p})
	}
	return s
}

func TestIdlePassesNeedThreeQuietPasses(t *testing.T) {
	f := &fakeIdle{passes: snaps(
		[]daemonProc{dp(10, "maild", 3, 100), dp(11, "sleepd", 3, 100), dp(12, "dirtyd", 7, 0), dp(13, "SpringBoard", 1, 0), dp(14, "diagnosticd", 3, 0)},
		// sleepd used one tick of CPU: not idle (even 1 tick counts); dirtyd turned clean.
		[]daemonProc{dp(10, "maild", 3, 100), dp(11, "sleepd", 3, 101), dp(12, "dirtyd", 3, 0), dp(13, "SpringBoard", 1, 0), dp(14, "diagnosticd", 3, 0)},
		// maild: third quiet pass -> ended here.
		[]daemonProc{dp(10, "maild", 3, 100), dp(11, "sleepd", 3, 101), dp(12, "dirtyd", 3, 0), dp(13, "SpringBoard", 1, 0), dp(14, "diagnosticd", 3, 0)},
		// sleepd and dirtyd: third quiet pass since their change -> ended.
		[]daemonProc{dp(11, "sleepd", 3, 101), dp(12, "dirtyd", 3, 0), dp(13, "SpringBoard", 1, 0), dp(14, "diagnosticd", 3, 0)},
	)}
	stopped := idlePasses(f, "test")
	if want := "10@3 KILL,11@4 KILL,12@4 KILL"; strings.Join(f.signals, ",") != want {
		t.Fatalf("signals = %v, want %s", f.signals, want)
	}
	if fmt.Sprint(stopped) != "map[dirtyd:1 maild:1 sleepd:1]" {
		t.Fatalf("stopped = %v", stopped)
	}
}

func TestIdlePassesPeriodicUse(t *testing.T) {
	// An app calls the daemon every 60 s (every second interval) or every 45 s: each call costs
	// CPU, so there are never 2 quiet intervals in a row and it is never ended.
	var every60, every45 [][]daemonProc
	cpu60, cpu45 := uint64(0), uint64(0)
	for i := 0; i < 9; i++ {
		if i%2 == 1 {
			cpu60 += 5
		}
		if i%3 != 0 { // calls at +45 s, +90 s, +135 s, ... land in 2 of every 3 intervals
			cpu45 += 5
		}
		every60 = append(every60, []daemonProc{dp(20, "fileproviderd-like", 3, cpu60)})
		every45 = append(every45, []daemonProc{dp(21, "coreidvd-like", 3, cpu45)})
	}
	for _, ps := range [][][]daemonProc{every60, every45} {
		f := &fakeIdle{passes: snaps(ps...)}
		if s := idlePasses(f, "test"); len(s) != 0 {
			t.Fatalf("ended %v, a daemon in periodic use", s)
		}
	}
}

func TestIdlePassesAppStartedAfter(t *testing.T) {
	// A daemon started after the first app process of this boot is never ended, idle or not;
	// one started before is.
	early := daemonProc{pid: 30, name: "maild", flags: 3, usage: procUsage{cpu: 5, start: 100}}
	late := daemonProc{pid: 31, name: "sleepd", flags: 3, usage: procUsage{cpu: 5, start: 500}}
	var ss []idleSnapshot
	for i := 0; i < 4; i++ {
		s := idleSnapshot{root: 1, daemons: []daemonProc{early, late}}
		if i >= 1 {
			s.firstApp = 400 // seen from the second pass on
		}
		ss = append(ss, s)
	}
	f := &fakeIdle{passes: ss}
	if s := idlePasses(f, "test"); fmt.Sprint(s) != "map[maild:1]" {
		t.Fatalf("ended %v, want only maild", s)
	}
}

func TestIdlePassesRecheck(t *testing.T) {
	ps := []daemonProc{dp(40, "maild", 3, 0), dp(41, "sleepd", 3, 0), dp(42, "parsecd", 3, 0)}
	f := &fakeIdle{passes: snaps(ps, ps, ps, ps, ps, ps), recheckF: func(p daemonProc) (bool, bool) {
		switch p.name {
		case "maild":
			return false, true // still the same idle process: SIGKILL
		case "sleepd":
			return false, false // busy since the pass, or the device got held: no SIGKILL, kept
		}
		return true, false // exited by itself: counted, nothing to signal
	}}
	stopped := idlePasses(f, "test")
	if want := "40@3 KILL"; strings.Join(f.signals, ",") != want {
		t.Fatalf("signals = %v, want %s", f.signals, want)
	}
	if fmt.Sprint(stopped) != "map[maild:1 parsecd:1]" {
		t.Fatalf("stopped = %v", stopped)
	}
}

func TestIdlePassesHeld(t *testing.T) {
	// A pass while agemu installs/launches an app or the device is parked does nothing and starts
	// the count again: quiet at 1, 2, held at 3, quiet at 4, 5, 6 -> ended at 6.
	ps := []daemonProc{dp(50, "maild", 3, 0)}
	f := &fakeIdle{passes: snaps(ps, ps, ps, ps, ps, ps), heldAt: map[int]bool{3: true}}
	idlePasses(f, "test")
	if want := "50@6 KILL"; strings.Join(f.signals, ",") != want {
		t.Fatalf("signals = %v, want %s", f.signals, want)
	}
}

func TestIdlePassesStopOnOutsideBoot(t *testing.T) {
	ps := []daemonProc{dp(60, "maild", 3, 0)}
	s := snaps(ps, ps, ps)
	s[2].root = 2 // launchd_sim changed: booted again outside agemu
	f := &fakeIdle{passes: s}
	if st := idlePasses(f, "test"); len(st) != 0 || len(f.signals) != 0 {
		t.Fatalf("ended %v, signals %v after the simulator was booted again", st, f.signals)
	}
	s = snaps(ps, ps, ps)
	s[2].root = 0 // shut down
	f = &fakeIdle{passes: s}
	if st := idlePasses(f, "test"); len(st) != 0 {
		t.Fatalf("ended %v on a shut-down simulator", st)
	}
}

func TestIdlePassesEndOnNewerBoot(t *testing.T) {
	ps := []daemonProc{dp(10, "maild", 3, 0)}
	f := &fakeIdle{passes: snaps(ps, ps, ps, ps), stopAt: 2}
	if s := idlePasses(f, "test"); len(s) != 0 || len(f.signals) != 0 {
		t.Fatalf("ended %v, signals %v after a newer boot", s, f.signals)
	}
}

func TestIdlePassesEndAgainAfterALaterReturn(t *testing.T) {
	// A daemon that a one-off job starts again after a pass without it may be ended again, up to
	// idleMaxEnds times; one that is back by the next pass stays.
	a := func(pid int) daemonProc { return dp(pid, "oneoff", 3, 0) }
	c := func(pid int) daemonProc { return dp(pid, "churn", 3, 0) }
	f := &fakeIdle{passes: snaps(
		[]daemonProc{a(1), c(2)},
		[]daemonProc{a(1), c(2)},
		[]daemonProc{a(1), c(2)}, // pass 3: both ended
		[]daemonProc{c(3)},       // churn back at once: kept from now on
		[]daemonProc{a(4), c(3)},
		[]daemonProc{a(4), c(3)},
		[]daemonProc{a(4), c(3)}, // pass 7: oneoff ended again
		[]daemonProc{},
		[]daemonProc{a(5)},
	)}
	stopped := idlePasses(f, "test")
	if want := "1@3 KILL,2@3 KILL,4@7 KILL"; strings.Join(f.signals, ",") != want {
		t.Fatalf("signals = %v, want %s", f.signals, want)
	}
	if stopped["oneoff"] != 2 || stopped["churn"] != 1 {
		t.Fatalf("stopped = %v", stopped)
	}
}

func TestIdlePassesMaxEnds(t *testing.T) {
	// Each end needs 3 passes; with an idleMaxEnds of 3 and 9 passes the third end is the last.
	var ps [][]daemonProc
	for pid := 1; pid <= 3; pid++ {
		for k := 0; k < 3; k++ {
			ps = append(ps, []daemonProc{dp(pid, "oneoff", 3, 0)})
		}
	}
	f := &fakeIdle{passes: snaps(ps...)}
	if s := idlePasses(f, "test"); s["oneoff"] > idleMaxEnds {
		t.Fatalf("ended %d times, max %d", s["oneoff"], idleMaxEnds)
	}
}

func TestIdleKeepEnv(t *testing.T) {
	t.Setenv("AGEMU_IOS_IDLE_KEEP", " maild, ,parsecd")
	ps := []daemonProc{dp(10, "maild", 3, 0), dp(11, "parsecd", 3, 0), dp(12, "sleepd", 3, 0)}
	f := &fakeIdle{passes: snaps(ps, ps, ps)}
	if s := idlePasses(f, "test"); fmt.Sprint(s) != "map[sleepd:1]" {
		t.Fatalf("ended %v, want only sleepd", s)
	}
}

func TestIdlePassesAppFacing(t *testing.T) {
	ps := []daemonProc{dp(10, "contactsd", 3, 0), dp(11, "maild", 3, 0), dp(12, "weatherd", 3, 0), dp(13, "fileproviderd", 3, 0)}
	run := func() map[string]int {
		return idlePasses(&fakeIdle{passes: snaps(ps, ps, ps)}, "test")
	}
	t.Setenv("AGEMU_IOS_IDLE_EXIT", "")
	if s := run(); fmt.Sprint(s) != "map[maild:1]" {
		t.Fatalf("default: ended %v, want only maild (the others serve public APIs)", s)
	}
	t.Setenv("AGEMU_IOS_IDLE_EXIT", "all")
	if s := run(); fmt.Sprint(s) != "map[contactsd:1 fileproviderd:1 maild:1 weatherd:1]" {
		t.Fatalf("all: ended %v", s)
	}
	for _, v := range []string{"", "1", "all"} {
		t.Setenv("AGEMU_IOS_IDLE_EXIT", v)
		if !idleExitOn() {
			t.Errorf("AGEMU_IOS_IDLE_EXIT=%q: off", v)
		}
	}
	t.Setenv("AGEMU_IOS_IDLE_EXIT", "0")
	if idleExitOn() {
		t.Error("AGEMU_IOS_IDLE_EXIT=0: on")
	}
}

func TestIdleKeepNeverEnded(t *testing.T) {
	// The never-ended names stay never ended in the all mode too.
	t.Setenv("AGEMU_IOS_IDLE_EXIT", "all")
	var ps []daemonProc
	pid := 100
	for n := range idleKeep {
		ps = append(ps, dp(pid, n, 3, 0))
		pid++
	}
	if s := idlePasses(&fakeIdle{passes: snaps(ps, ps, ps)}, "test"); len(s) != 0 {
		t.Fatalf("ended %v from idleKeep", s)
	}
}

func TestAppOp(t *testing.T) {
	b := &Backend{appOps: map[string]int{}, appWork: map[string]time.Time{}}
	if b.appOpBusy("U") || !b.appWorkSince("U").IsZero() {
		t.Fatal("busy or marked before any op")
	}
	hold := b.appOp("U", false) // reset, permission, location: holds, sets no cut-off
	if !b.appOpBusy("U") || !b.appWorkSince("U").IsZero() {
		t.Fatal("a holding op must hold and not set the cut-off")
	}
	t0 := time.Now()
	done1, done2 := b.appOp("U", true), b.appOp("U", true)
	first := b.appWorkSince("U")
	if first.Before(t0) || first.After(time.Now()) {
		t.Fatalf("cut-off %v not set by the first work op", first)
	}
	done1()
	hold()
	if !b.appOpBusy("U") {
		t.Fatal("not busy with one op in flight")
	}
	done2()
	if b.appOpBusy("U") || len(b.appOps) != 0 || !b.appWorkSince("U").Equal(first) {
		t.Fatalf("after all ops: busy %v, ops %v, cut-off %v (must stay %v)", b.appOpBusy("U"), b.appOps, b.appWorkSince("U"), first)
	}
}

func TestProcDirtySelf(t *testing.T) {
	// A test binary does not take part in dirty tracking; the call itself must work on macOS.
	fl, err := procDirty(os.Getpid())
	if err != nil && strings.Contains(err.Error(), "needs macOS") {
		t.Skip(err)
	}
	if err != nil || idleExitable(fl) {
		t.Fatalf("procDirty(self) = %#x, %v", fl, err)
	}
	if _, err := procDirty(1 << 30); err == nil {
		t.Fatal("procDirty of a pid that does not exist: no error")
	}
}

func TestProcUsageSelf(t *testing.T) {
	u1, err := procUsageOf(os.Getpid())
	if err != nil && strings.Contains(err.Error(), "needs macOS") {
		t.Skip(err)
	}
	if err != nil || u1.start == 0 || u1.cpu == 0 {
		t.Fatalf("procUsageOf(self) = %+v, %v", u1, err)
	}
	x := 0
	for i := 0; i < 20_000_000; i++ {
		x += i
	}
	_ = x
	u2, _ := procUsageOf(os.Getpid())
	if u2.start != u1.start || u2.cpu <= u1.cpu {
		t.Fatalf("second read %+v after %+v: start must stay, CPU must grow", u2, u1)
	}
	if _, err := procUsageOf(1 << 30); err == nil {
		t.Fatal("procUsageOf of a pid that does not exist: no error")
	}
}

func TestIdleSnapshotFrontmostAndSpaces(t *testing.T) {
	// A device folder with a space; Safari (a system app) in front; SpringBoard in front does not
	// count as an app.
	const udid = "ED6C55BD-83FB-49B9-93A2-54BA0AD521DD"
	dev := "/Users/me/My Sims/" + udid
	ps := fmt.Sprintf(`  100     1 launchd_sim %[1]s/data/var/run/launchd_bootstrap.plist
  101   100 %[2]s/System/Library/CoreServices/SpringBoard.app/SpringBoard
  102   100 %[2]s/Applications/MobileSafari.app/MobileSafari
  103   100 %[1]s/data/Containers/Bundle/Application/0A1B/My App.app/My App -flag
  104   100 %[2]s/usr/libexec/com.apple.Safari.History
`, dev, rtRoot)
	root, procs, _ := simRootChildren(ps, udid)
	starts := map[int]int64{101: 5, 102: 70, 103: 90, 104: 71}
	usage := func(pid int) (procUsage, error) { return procUsage{start: starts[pid]}, nil }
	dirty := func(int) (uint32, error) { return 0x3, nil }
	if s := idleSnapshotOf(root, procs, 102, dirty, usage); s.firstApp != 70 {
		t.Fatalf("Safari in front: firstApp = %d, want 70", s.firstApp)
	}
	if s := idleSnapshotOf(root, procs, 101, dirty, usage); s.firstApp != 90 {
		t.Fatalf("SpringBoard in front: firstApp = %d, want 90 (the installed app in a folder with spaces)", s.firstApp)
	}
	var names []string
	for _, d := range idleSnapshotOf(root, procs, 0, dirty, usage).daemons {
		names = append(names, d.name)
	}
	sort.Strings(names)
	if fmt.Sprint(names) != "[MobileSafari SpringBoard com.apple.Safari.History]" {
		t.Fatalf("daemons = %v (the installed app must not be one)", names)
	}
}

func TestIdlePassesCutAtFirstAppWork(t *testing.T) {
	// The first install/launch/open/push of the boot sets the cut-off even before any app process
	// exists (an install starts daemons first) and after the app has exited again: daemons started
	// after it are never ended; one started before is.
	before := daemonProc{pid: 70, name: "maild", flags: 3, usage: procUsage{cpu: 5, start: 100}}
	byInstall := daemonProc{pid: 71, name: "privacyaccountingd", flags: 3, usage: procUsage{cpu: 5, start: 210}}
	var ss []idleSnapshot
	for i := 0; i < 4; i++ {
		ss = append(ss, idleSnapshot{root: 1, daemons: []daemonProc{before, byInstall}, appWork: 200})
	}
	if s := idlePasses(&fakeIdle{passes: ss}, "test"); fmt.Sprint(s) != "map[maild:1]" {
		t.Fatalf("ended %v, want only maild", s)
	}
	// The earliest of the two marks wins, and a mark seen once stays.
	ss = nil
	for i := 0; i < 4; i++ {
		s := idleSnapshot{root: 1, daemons: []daemonProc{before, byInstall}}
		if i == 0 {
			s.firstApp = 90 // an app ran at the first pass only, then exited
		}
		ss = append(ss, s)
	}
	if s := idlePasses(&fakeIdle{passes: ss}, "test"); len(s) != 0 {
		t.Fatalf("ended %v after an app that ran before both started", s)
	}
}

func TestIdlePassesNoSIGTERM(t *testing.T) {
	ps := []daemonProc{dp(90, "maild", 3, 0)}
	f := &fakeIdle{passes: snaps(ps, ps, ps)}
	idlePasses(f, "test")
	for _, sg := range f.signals {
		if strings.HasSuffix(sg, "TERM") {
			t.Fatalf("SIGTERM sent: %v", f.signals)
		}
	}
	if len(f.signals) != 1 {
		t.Fatalf("signals %v, want one SIGKILL", f.signals)
	}
}

func TestRecheckOf(t *testing.T) {
	p := daemonProc{pid: 5, name: "maild", flags: 3, usage: procUsage{cpu: 100, start: 7}}
	ok := func(u procUsage, uerr error, fl uint32, ferr error, held bool) (bool, bool) {
		return recheckOf(p, func(int) (procUsage, error) { return u, uerr }, func(int) (uint32, error) { return fl, ferr }, func() bool { return held })
	}
	for _, c := range []struct {
		name           string
		u              procUsage
		uerr           error
		fl             uint32
		ferr           error
		held           bool
		gone, killable bool
	}{
		{"same idle process", procUsage{100, 7}, nil, 3, nil, false, false, true},
		{"used CPU since the pass", procUsage{101, 7}, nil, 3, nil, false, false, false},
		{"dirty now", procUsage{100, 7}, nil, 7, nil, false, false, false},
		{"device held", procUsage{100, 7}, nil, 3, nil, true, false, false},
		{"pid reused (other start)", procUsage{100, 8}, nil, 3, nil, false, true, false},
		{"exited", procUsage{}, syscall.ESRCH, 0, nil, false, true, false},
		{"exited before the flags read", procUsage{100, 7}, nil, 0, syscall.ESRCH, false, true, false},
	} {
		if g, k := ok(c.u, c.uerr, c.fl, c.ferr, c.held); g != c.gone || k != c.killable {
			t.Errorf("%s: gone %v killable %v, want %v %v", c.name, g, k, c.gone, c.killable)
		}
	}
}

// A pass whose ends cannot keep parks out (a park or a shutdown holds the device) ends nothing
// and counts again from zero, as a held pass; every SIGKILL is sent while parks are kept out.
func TestIdlePassesSkipWhileParkLockHeld(t *testing.T) {
	ps := []daemonProc{dp(10, "maild", dirtyTracked|dirtyAllowsIdleExit, 5)}
	f := &fakeIdle{passes: snaps(ps, ps, ps, ps, ps, ps, ps), lockedAt: map[int]bool{3: true}}
	idlePasses(f, "test")
	// Due at pass 3 (as in TestIdlePassesNeedThreeQuietPasses) but locked: the counting starts
	// again at pass 4 and the daemon is ended at pass 6.
	if want := "[10@6 KILL]"; fmt.Sprint(f.signals) != want {
		t.Fatalf("signals = %v, want %s", f.signals, want)
	}
	if f.exclusiveHeld {
		t.Fatal("exclusive not released after the pass")
	}
}

// The integration with parking: a park marks the device parked before it stops anything, so
// held() sees a park in progress (an agent's or auto-park's, both go through Park); while a pass
// ends daemons, a park waits for it (it cannot mark the device yet); and a pass cannot take the
// device while a park or a shutdown holds it.
func TestDeviceIdleExclusiveWithPark(t *testing.T) {
	const udid = "11111111-2222-3333-4444-555555555555"
	b := &Backend{parked: map[string]bool{}, parking: map[string]bool{}, appOps: map[string]int{}, appWork: map[string]time.Time{}}
	e := newDeviceIdle(b, simDevice{UDID: udid, Name: "agemu-test"}, 0)
	if e.held() {
		t.Fatal("held before any park")
	}
	b.mu.Lock()
	b.parked[udid], b.parking[udid] = true, true // Park's marks, set before stopTree
	b.mu.Unlock()
	if !e.held() {
		t.Fatal("a park in progress must hold the passes")
	}
	b.mu.Lock()
	delete(b.parked, udid)
	delete(b.parking, udid)
	b.mu.Unlock()

	release, ok := e.exclusive()
	if !ok {
		t.Fatal("exclusive failed with no park or shutdown")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	err := b.Park(ctx, "ios:"+udid)
	cancel()
	if err == nil || !strings.Contains(err.Error(), "waiting") {
		t.Fatalf("Park during a pass's ends = %v, want it to wait for the pass", err)
	}
	if e.held() {
		t.Fatal("the waiting park marked the device before the pass released it")
	}
	release()

	l := b.parkLock(udid) // a park or a shutdown in progress
	l.Lock()
	if _, ok := e.exclusive(); ok {
		t.Fatal("exclusive succeeded while a park or a shutdown holds the device")
	}
	l.Unlock()
	if r, ok := e.exclusive(); !ok {
		t.Fatal("exclusive failed after the park finished")
	} else {
		r()
	}
}

// A park that began and ended between two held checks (the pass waited it out in waitUntil) still
// holds the next check once, so the counting starts again after the resume: a daemon frozen for
// the park used no CPU and must not count as idle for that interval.
func TestDeviceIdleHeldOnceAfterAPark(t *testing.T) {
	const udid = "11111111-2222-3333-4444-666666666666"
	b := &Backend{parked: map[string]bool{}, parking: map[string]bool{}, appOps: map[string]int{}, appWork: map[string]time.Time{},
		parks: map[string]int{udid: 2}} // parks before this boot do not count
	e := newDeviceIdle(b, simDevice{UDID: udid, Name: "agemu-test"}, 0)
	if e.held() {
		t.Fatal("held with no park since the passes started")
	}
	b.mu.Lock()
	b.parks[udid]++ // Park began (and the resume ended it) between two checks
	b.mu.Unlock()
	if !e.held() {
		t.Fatal("a park since the last check must hold the pass")
	}
	if e.held() {
		t.Fatal("held again with no new park")
	}
}

// An app call that starts while a pass is ending daemons waits for the ends: no SIGKILL can follow
// the start of app work, and a call registered before the pass holds the device is seen by its
// held check.
func TestAppCallsWaitForAPassEnding(t *testing.T) {
	const udid = "11111111-2222-3333-4444-777777777777"
	b := &Backend{parked: map[string]bool{}, parking: map[string]bool{}, appOps: map[string]int{}, appWork: map[string]time.Time{}}
	e := newDeviceIdle(b, simDevice{UDID: udid, Name: "agemu-test"}, 0)
	release, ok := e.exclusive()
	if !ok {
		t.Fatal("exclusive refused on an idle device")
	}
	started := make(chan func())
	go func() { started <- b.appOp(udid, true) }()
	select {
	case <-started:
		t.Fatal("an app call registered while the pass held the device")
	case <-time.After(100 * time.Millisecond):
	}
	if e.held() {
		t.Fatal("the waiting app call made the device busy before the ends were done")
	}
	release()
	var done func()
	select {
	case done = <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("the app call did not start after the pass released the device")
	}
	if !e.held() {
		t.Fatal("an app call in flight must hold the passes")
	}
	done()
	// Registered before the pass takes the device: the pass sees it under the gate.
	done = b.appOp(udid, false)
	r, ok := e.exclusive()
	if !ok || !e.held() {
		t.Fatalf("exclusive %v, held %v: an app call in flight must hold the ends", ok, e.held())
	}
	r()
	done()
}

// A late pass followed at once by the next one does not count as an idle interval: a daemon
// whose CPU did not move over a few seconds is not ended before idleQuietPasses intervals of at
// least idleMinInterval each.
func TestAShortIntervalDoesNotCountAsIdle(t *testing.T) {
	t0 := time.Unix(1000, 0)
	p := dp(10, "periodicd", 3, 5) // 3: tracked, idle exit allowed, clean
	at := func(d time.Duration) idleSnapshot {
		return idleSnapshot{root: 1, daemons: []daemonProc{p}, taken: t0.Add(d)}
	}
	// 30 s, then two back-to-back passes 1 s apart: only one full interval so far.
	f := &fakeIdle{passes: []idleSnapshot{at(0), at(30 * time.Second), at(31 * time.Second), at(32 * time.Second)}, stopAt: 5}
	idlePasses(f, "test")
	if len(f.signals) != 0 {
		t.Fatalf("ended after short intervals: %v", f.signals)
	}
	// The same daemon over full intervals is ended at the third snapshot.
	f = &fakeIdle{passes: []idleSnapshot{at(0), at(30 * time.Second), at(60 * time.Second)}, stopAt: 4}
	idlePasses(f, "test")
	if len(f.signals) == 0 {
		t.Fatal("not ended after two full idle intervals")
	}
}

// The passes act only on the boot they were scheduled for: a simulator booted again outside agemu
// (another launchd_sim, also before the first pass) or an unknown boot stops them.
func TestIdleBootCheck(t *testing.T) {
	if idleBootCheck(100, 100) != nil || idleBootCheck(100, 0) != nil {
		t.Fatal("the same boot (or a stopped simulator, left to idlePasses) must not stop the passes here")
	}
	if idleBootCheck(100, 200) == nil {
		t.Fatal("another launchd_sim (booted again outside agemu) must stop the passes")
	}
	if idleBootCheck(0, 200) == nil {
		t.Fatal("an unknown boot must stop the passes")
	}
}

// Two counted intervals that add up to less than a minute (+75, +90 and +120 s: 15 s, then 30 s)
// do not end a daemon: it must have been seen idle for idleQuietSpan from the first observation of
// the run to the last.
func TestTwoIntervalsUnderAMinuteDoNotEndADaemon(t *testing.T) {
	t0 := time.Unix(1000, 0)
	p := dp(10, "periodicd", 3, 5)
	at := func(d time.Duration) idleSnapshot {
		return idleSnapshot{root: 1, daemons: []daemonProc{p}, taken: t0.Add(d)}
	}
	f := &fakeIdle{passes: []idleSnapshot{at(75 * time.Second), at(90 * time.Second), at(120 * time.Second)}, stopAt: 4}
	idlePasses(f, "test")
	if len(f.signals) != 0 {
		t.Fatalf("ended after 45 s of observed quiet: %v", f.signals)
	}
	// The next pass (+150 s) completes a span of 75 s: ended then.
	f = &fakeIdle{passes: []idleSnapshot{at(75 * time.Second), at(90 * time.Second), at(120 * time.Second), at(150 * time.Second)}, stopAt: 5}
	idlePasses(f, "test")
	if len(f.signals) == 0 {
		t.Fatal("not ended after a quiet span of 75 s")
	}
}

// A reboot outside agemu before the first pass (deviceIdle.snapshot then returns idleBootCheck's
// error) ends the passes before anything is signalled, also when idle daemons were seen earlier.
func TestARebootOutsideAgemuStopsThePassesBeforeAnySignal(t *testing.T) {
	idle := []daemonProc{dp(10, "maild", 3, 100)}
	for _, at := range []int{1, 3} {
		f := &fakeIdle{passes: snaps(idle, idle, idle, idle, idle), snapErrAt: at}
		idlePasses(f, "test")
		if len(f.signals) != 0 {
			t.Fatalf("snapshot failing at pass %d (a reboot): signals %v", at, f.signals)
		}
	}
}
