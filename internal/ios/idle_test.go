package ios

import (
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
		0x0: false, // not tracked (SpringBoard's backboardd, apps)
		0x1: false, // tracked, no idle exit (SpringBoard)
		0x3: true,  // tracked, idle exit allowed, clean
		0x7: false, // dirty: a transaction is open
		0x5: false,
		0xb: true, // launch-in-progress bit (0x8) is not looked at
	} {
		if got := idleExitable(flags); got != want {
			t.Errorf("idleExitable(%#x) = %v, want %v", flags, got, want)
		}
	}
}

func TestParsePsTime(t *testing.T) {
	for s, want := range map[string]time.Duration{
		"0:00.00":       0,
		"0:01.25":       1250 * time.Millisecond,
		"12:03.50":      12*time.Minute + 3500*time.Millisecond,
		"1:02:03.00":    time.Hour + 2*time.Minute + 3*time.Second,
		"2-01:00:00.00": 49 * time.Hour,
	} {
		got, err := parsePsTime(s)
		if err != nil || got.Round(time.Millisecond) != want {
			t.Errorf("parsePsTime(%q) = %v, %v; want %v", s, got, err, want)
		}
	}
	if _, err := parsePsTime("abc"); err == nil {
		t.Error("parsePsTime(abc): no error")
	}
	m := parseCPUTimes("  101   0:01.50\n  102 1:00:00.00\nbad\n")
	if m[101] != 1500*time.Millisecond || m[102] != time.Hour || len(m) != 2 {
		t.Fatalf("parseCPUTimes = %v", m)
	}
}

func TestRuntimeDaemons(t *testing.T) {
	const udid = "ED6C55BD-83FB-49B9-93A2-54BA0AD521DD"
	app := "/Users/me/Library/Developer/CoreSimulator/Devices/" + udid + "/data/Containers/Bundle/Application/0A1B/MyApp.app"
	ps := fmt.Sprintf(`  100     1 launchd_sim /Users/me/Library/Developer/CoreSimulator/Devices/%[1]s/data/var/run/launchd_bootstrap.plist
  101   100 %[2]s/System/Library/PrivateFrameworks/EmailDaemon.framework/maild
  102   100 %[2]s/usr/libexec/securityd
  103   100 %[3]s/MyApp
  104   100 %[2]s/Applications/MobileSafari.app/Extensions/SafariLinkExtension.appex/SafariLinkExtension -LaunchArguments eyJ9
  105   100 /usr/bin/grep %[2]s/usr/libexec/securityd
  106   100 %[2]s/usr/libexec/gone
`, udid, rtRoot, app)
	procs, _ := simChildren(ps, udid)
	cpu := map[int]time.Duration{101: time.Second, 102: 2 * time.Second, 103: 0, 104: 0, 105: 0}
	dirty := func(pid int) (uint32, error) {
		if pid == 104 {
			return 0, syscall.ESRCH
		}
		return 0x3, nil
	}
	ds := runtimeDaemons(procs, cpu, dirty)
	sort.Slice(ds, func(i, j int) bool { return ds[i].pid < ds[j].pid })
	var got []string
	for _, d := range ds {
		got = append(got, fmt.Sprintf("%d %s %v", d.pid, d.name, d.cpu))
	}
	// Runtime executables only (not the app, not a process naming the path), with a CPU time
	// (106 has none) and readable flags (104 exited).
	if want := "[101 maild 1s 102 securityd 2s]"; fmt.Sprint(got) != want {
		t.Fatalf("runtimeDaemons = %v, want %s", got, want)
	}
}

// fakeIdle replays a scripted process table per pass and records signals.
type fakeIdle struct {
	passes  [][]daemonProc // what daemons() returns at each pass
	pass    int
	alive   map[int]bool // processes that are still running when dirty() is asked after SIGTERM
	dirtyAt map[int]uint32
	signals []string
	stopAt  int // current() turns false at this pass (0: never)
}

func (f *fakeIdle) waitUntil(time.Duration) bool { return true }
func (f *fakeIdle) current() bool                { return f.stopAt == 0 || f.pass < f.stopAt }
func (f *fakeIdle) daemons() ([]daemonProc, error) {
	if f.pass >= len(f.passes) {
		f.pass++
		return nil, nil
	}
	p := f.passes[f.pass]
	f.pass++
	return p, nil
}
func (f *fakeIdle) dirty(pid int) (uint32, error) {
	if !f.alive[pid] {
		return 0, syscall.ESRCH
	}
	if fl, ok := f.dirtyAt[pid]; ok {
		return fl, nil
	}
	return 0x3, nil
}
func (f *fakeIdle) signal(pid int, sig syscall.Signal) error {
	f.signals = append(f.signals, fmt.Sprintf("%d %v", pid, map[syscall.Signal]string{syscall.SIGTERM: "TERM", syscall.SIGKILL: "KILL"}[sig]))
	return nil
}
func (f *fakeIdle) sleep(time.Duration) {}

func dp(pid int, name string, flags uint32, cpuMs int) daemonProc {
	return daemonProc{pid: pid, name: name, flags: flags, cpu: time.Duration(cpuMs) * time.Millisecond}
}

func TestIdlePasses(t *testing.T) {
	f := &fakeIdle{
		passes: [][]daemonProc{
			// pass 1: everything is new; nothing is ended yet.
			{dp(10, "maild", 3, 100), dp(11, "busyd", 3, 100), dp(12, "dirtyd", 7, 0), dp(13, "SpringBoard", 1, 0),
				dp(14, "diagnosticd", 3, 0), dp(15, "survivor", 3, 0), dp(16, "relapsed", 3, 0)},
			// pass 2: maild idle (10 ms), busyd used 300 ms, dirtyd clean now but was dirty, a new
			// daemon (20) appears; survivor and relapsed are idle.
			{dp(10, "maild", 3, 110), dp(11, "busyd", 3, 400), dp(12, "dirtyd", 3, 0), dp(13, "SpringBoard", 1, 0),
				dp(14, "diagnosticd", 3, 0), dp(15, "survivor", 3, 0), dp(16, "relapsed", 3, 0), dp(20, "newd", 3, 0)},
			// pass 3: relapsed was started again (new pid), survivor kept its pid (it became busy
			// before SIGKILL); busyd idle now; dirtyd idle and clean; newd idle.
			{dp(11, "busyd", 3, 405), dp(12, "dirtyd", 3, 0), dp(13, "SpringBoard", 1, 0), dp(14, "diagnosticd", 3, 0),
				dp(15, "survivor", 3, 0), dp(30, "relapsed", 3, 0), dp(20, "newd", 3, 0)},
			// pass 4: relapsed again idle: it stays (it came back once).
			{dp(13, "SpringBoard", 1, 0), dp(14, "diagnosticd", 3, 0), dp(15, "survivor", 3, 0), dp(30, "relapsed", 3, 0)},
		},
		// After SIGTERM: maild (10) ignores it and is still clean -> SIGKILL; survivor (15) ignores it
		// and turned dirty -> no SIGKILL; relapsed (16) exits on SIGTERM.
		alive:   map[int]bool{10: true, 15: true, 11: true, 12: false, 20: false},
		dirtyAt: map[int]uint32{15: 7},
	}
	stopped := idlePasses(f, "test")
	want := []string{
		// pass 2
		"10 TERM", "15 TERM", "16 TERM", "10 KILL",
		// pass 3: busyd (idle since pass 2), dirtyd (clean at both 2 and 3), newd
		"11 TERM", "12 TERM", "20 TERM", "11 KILL",
	}
	if strings.Join(f.signals, ",") != strings.Join(want, ",") {
		t.Fatalf("signals = %v,\nwant %v", f.signals, want)
	}
	if fmt.Sprint(stopped) != "map[busyd:1 dirtyd:1 maild:1 newd:1 relapsed:1]" {
		t.Fatalf("stopped = %v", stopped)
	}
}

func TestIdlePassesEndOnNewerBoot(t *testing.T) {
	procs := []daemonProc{dp(10, "maild", 3, 0)}
	f := &fakeIdle{passes: [][]daemonProc{procs, procs, procs}, stopAt: 1, alive: map[int]bool{}}
	if s := idlePasses(f, "test"); len(s) != 0 || len(f.signals) != 0 {
		t.Fatalf("ended %v, signals %v after a newer boot", s, f.signals)
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

func TestIdlePassesEndAgainAfterALaterReturn(t *testing.T) {
	// A daemon that a one-off job starts again after a pass without it may be ended again, up to
	// idleMaxEnds times; one that is back by the next pass stays.
	a := func(pid int) daemonProc { return dp(pid, "oneoff", 3, 0) }
	c := func(pid int) daemonProc { return dp(pid, "churn", 3, 0) }
	f := &fakeIdle{
		passes: [][]daemonProc{
			{a(1), c(2)},
			{a(1), c(2)}, // end oneoff 1, churn 2
			{c(3)},       // churn back at once: stays from now on
			{a(4), c(3)},
			{a(4), c(3)}, // oneoff ended again (2nd)
			{},
			{a(5)},
			{a(5)}, // 3rd
			{},
		},
		alive: map[int]bool{},
	}
	stopped := idlePasses(f, "test")
	if want := "1 TERM,2 TERM,4 TERM,5 TERM"; strings.Join(f.signals, ",") != want {
		t.Fatalf("signals = %v, want %s", f.signals, want)
	}
	if stopped["oneoff"] != 3 || stopped["churn"] != 1 {
		t.Fatalf("stopped = %v", stopped)
	}
	if idleMaxEnds != 3 {
		t.Fatalf("idleMaxEnds = %d; the pass count of this test assumes 3", idleMaxEnds)
	}
}

func TestIdlePassesMaxEnds(t *testing.T) {
	var passes [][]daemonProc
	for pid := 1; pid <= 5; pid++ {
		passes = append(passes, []daemonProc{dp(pid, "oneoff", 3, 0)}, []daemonProc{dp(pid, "oneoff", 3, 0)}, nil)
	}
	f := &fakeIdle{passes: passes, alive: map[int]bool{}}
	// The fake has 15 scripted passes; the real schedule has 9 (+60..+300 s), so only the count
	// matters here: ended at most idleMaxEnds times.
	if s := idlePasses(f, "test"); s["oneoff"] != idleMaxEnds {
		t.Fatalf("ended %d times, want %d", s["oneoff"], idleMaxEnds)
	}
}

func TestIdleKeepEnv(t *testing.T) {
	t.Setenv("AGEMU_IOS_IDLE_KEEP", " maild, ,parsecd")
	procs := []daemonProc{dp(10, "maild", 3, 0), dp(11, "parsecd", 3, 0), dp(12, "adprivacyd", 3, 0)}
	f := &fakeIdle{passes: [][]daemonProc{procs, procs}, alive: map[int]bool{}}
	if s := idlePasses(f, "test"); fmt.Sprint(s) != "map[adprivacyd:1]" {
		t.Fatalf("ended %v, want only adprivacyd", s)
	}
}

func TestIdlePassesAppFacing(t *testing.T) {
	procs := []daemonProc{dp(10, "contactsd", 3, 0), dp(11, "adprivacyd", 3, 0)}
	run := func() map[string]int {
		f := &fakeIdle{passes: [][]daemonProc{procs, procs}, alive: map[int]bool{}}
		return idlePasses(f, "test")
	}
	t.Setenv("AGEMU_IOS_IDLE_EXIT", "")
	if s := run(); fmt.Sprint(s) != "map[adprivacyd:1]" {
		t.Fatalf("default: ended %v, want only adprivacyd (contactsd serves Contacts)", s)
	}
	t.Setenv("AGEMU_IOS_IDLE_EXIT", "all")
	if s := run(); fmt.Sprint(s) != "map[adprivacyd:1 contactsd:1]" {
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
