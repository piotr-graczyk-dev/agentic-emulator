package ios

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const parkUDID = "ED6C55BD-83FB-49B9-93A2-54BA0AD521DD"

// psLine is one `ps -axwwo pid=,ppid=,stat=,command=` line. Runtime paths have spaces ("iOS
// 26.5.simruntime"), as on a real host.
func psLine(pid, ppid int, stat, cmd string) string {
	return fmt.Sprintf("%6d %6d %-4s %s", pid, ppid, stat, cmd)
}

const runtime = "/Library/Developer/CoreSimulator/Volumes/iOS_23F77/Library/Developer/CoreSimulator/Profiles/Runtimes/iOS 26.5.simruntime/Contents/Resources/RuntimeRoot"

func launchdSim(pid int, stat, udid string) string {
	return psLine(pid, 1, stat, "launchd_sim /Users/me/Library/Developer/CoreSimulator/Devices/"+udid+"/data/var/run/launchd_bootstrap.plist")
}

func TestSimTreeFindsOneSimulatorsProcesses(t *testing.T) {
	other := "18962024-0BC7-40CD-9598-F9C4C8DD49E9"
	ps := strings.Join([]string{
		launchdSim(100, "Ss", parkUDID),
		psLine(101, 100, "S", runtime+"/System/Library/CoreServices/SpringBoard.app/SpringBoard"),
		psLine(102, 100, "T", runtime+"/usr/libexec/backboardd"),
		launchdSim(200, "Ss", other),
		psLine(201, 200, "S", runtime+"/System/Library/CoreServices/SpringBoard.app/SpringBoard"),
		psLine(300, 1, "S", "/usr/libexec/launchd_sim_helper "+parkUDID), // not launchd_sim
		psLine(400, 101, "S", "/bin/grandchild"),                         // not a direct child
		"garbage line",
	}, "\n")
	root, rootStopped, children := simTree(ps, parkUDID)
	if root != 100 || rootStopped {
		t.Fatalf("root = %d stopped %v, want 100 running", root, rootStopped)
	}
	want := map[int]bool{101: false, 102: true}
	if fmt.Sprint(children) != fmt.Sprint(want) {
		t.Fatalf("children = %v, want %v", children, want)
	}
	if root, _, _ := simTree(ps, "00000000-0000-0000-0000-000000000000"); root != 0 {
		t.Fatalf("an unknown simulator has root %d", root)
	}
}

// fakeHost replays ps listings (the last one repeats) and records signals.
type fakeHost struct {
	listings []string
	calls    int
	sent     []string
	fail     map[int]error
}

func (h *fakeHost) list() (string, error) {
	i := h.calls
	if i >= len(h.listings) {
		i = len(h.listings) - 1
	}
	h.calls++
	return h.listings[i], nil
}

func (h *fakeHost) kill(pid int, sig syscall.Signal) error {
	if err := h.fail[pid]; err != nil {
		return err
	}
	name := map[syscall.Signal]string{syscall.SIGSTOP: "STOP", syscall.SIGCONT: "CONT"}[sig]
	h.sent = append(h.sent, fmt.Sprintf("%s %d", name, pid))
	return nil
}

func TestStopTreeStopsLaunchdSimFirstThenEveryChild(t *testing.T) {
	first := strings.Join([]string{
		launchdSim(100, "Ss", parkUDID),
		psLine(101, 100, "S", runtime+"/SpringBoard"),
		psLine(102, 100, "S", runtime+"/backboardd"),
	}, "\n")
	// The second look finds what launchd_sim started while it was being stopped, with the
	// first children stopped; the third finds nothing new.
	second := strings.Join([]string{
		launchdSim(100, "Ts", parkUDID),
		psLine(101, 100, "T", runtime+"/SpringBoard"),
		psLine(102, 100, "T", runtime+"/backboardd"),
		psLine(103, 100, "S", runtime+"/xpcproxy"),
	}, "\n")
	third := strings.ReplaceAll(second, "103    100 S", "103    100 T")
	h := &fakeHost{listings: []string{first, second, third}}
	stopped, err := stopTree(h.list, parkUDID, h.kill)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(stopped); n != 4 || len(h.sent) != 4 || h.sent[0] != "STOP 100" {
		t.Fatalf("stopped %v: %v; want launchd_sim (100) first, then 101-103", stopped, h.sent)
	}
	got := map[string]bool{}
	for _, s := range h.sent {
		got[s] = true
	}
	for _, want := range []string{"STOP 101", "STOP 102", "STOP 103"} {
		if !got[want] {
			t.Fatalf("%s missing: %v", want, h.sent)
		}
	}
}

func TestStopTreeSkipsStoppedChildrenAndExitedOnes(t *testing.T) {
	ps := strings.Join([]string{
		launchdSim(100, "Ss", parkUDID),
		psLine(101, 100, "T", runtime+"/stopped-by-someone"),
		psLine(102, 100, "S", runtime+"/exits-now"),
	}, "\n")
	h := &fakeHost{listings: []string{ps, strings.Replace(ps, "102    100 S", "102    100 T", 1)},
		fail: map[int]error{102: syscall.ESRCH}}
	stopped, err := stopTree(h.list, parkUDID, h.kill)
	if err != nil {
		t.Fatal(err)
	}
	if len(stopped) != 1 || !stopped[100] || strings.Join(h.sent, ",") != "STOP 100" {
		t.Fatalf("stopped %v: %v; want only launchd_sim", stopped, h.sent)
	}
	// The resume continues what the park stopped: the child stopped before it stays stopped.
	h.sent = nil
	if _, err := continueTree(h.list, parkUDID, h.kill, stopped); err != nil {
		t.Fatal(err)
	}
	if strings.Join(h.sent, ",") != "CONT 100" {
		t.Fatalf("continued %v; want only launchd_sim, not the child stopped before the park", h.sent)
	}
}

func TestStopTreeGivesUpOnAChildStartedEveryLook(t *testing.T) {
	pid := 200
	h := &fakeHost{}
	list := func() (string, error) {
		pid++
		return launchdSim(100, "Ts", parkUDID) + "\n" + psLine(pid, 100, "S", runtime+"/respawning"), nil
	}
	if _, err := stopTree(list, parkUDID, h.kill); err == nil || !strings.Contains(err.Error(), "kept starting") {
		t.Fatalf("err = %v, want it to give up", err)
	}
	if len(h.sent) != 1+parkRounds {
		t.Fatalf("sent %d signals, want launchd_sim plus one per round (%d): %v", len(h.sent), 1+parkRounds, h.sent)
	}
}

func TestStopTreeWithoutLaunchdSimFails(t *testing.T) {
	h := &fakeHost{listings: []string{psLine(5, 1, "S", "/sbin/launchd")}}
	if _, err := stopTree(h.list, parkUDID, h.kill); err == nil || len(h.sent) != 0 {
		t.Fatalf("err = %v, sent %v", err, h.sent)
	}
	h = &fakeHost{listings: []string{launchdSim(100, "Ss", parkUDID)}, fail: map[int]error{100: errors.New("EPERM")}}
	if _, err := stopTree(h.list, parkUDID, h.kill); err == nil {
		t.Fatal("a launchd_sim that cannot be stopped must fail the park")
	}
}

func TestContinueTreeContinuesChildrenBeforeLaunchdSim(t *testing.T) {
	ps := strings.Join([]string{
		launchdSim(100, "Ts", parkUDID),
		psLine(101, 100, "T", runtime+"/SpringBoard"),
		psLine(102, 100, "T", runtime+"/backboardd"),
	}, "\n")
	h := &fakeHost{listings: []string{ps}}
	n, err := continueTree(h.list, parkUDID, h.kill, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 || len(h.sent) != 3 || h.sent[2] != "CONT 100" {
		t.Fatalf("continued %d: %v; want 101 and 102, then launchd_sim (100) last", n, h.sent)
	}
}

func TestSimUDID(t *testing.T) {
	cases := map[string]string{
		launchdSim(1, "Ts", parkUDID): parkUDID,
		"  7 1 T launchd_sim /tmp/Devices/short/data/var/run/launchd_bootstrap.plist": "",
		"  7 1 T launchd_sim /tmp/no-device-path":                                     "",
	}
	for line, want := range cases {
		if got := simUDID(line); got != want {
			t.Fatalf("simUDID(%q) = %q, want %q", line, got, want)
		}
	}
}

func TestParkedAndUnparkFollowTheMark(t *testing.T) {
	b := &Backend{parked: map[string]bool{}}
	b.parkScan.Do(func() {}) // no scan of the real host's processes
	id := "ios:" + parkUDID
	if b.Parked(id) || b.Parked("android:x") {
		t.Fatal("nothing is parked yet")
	}
	b.parked[parkUDID] = true
	if !b.Parked(id) {
		t.Fatal("a marked simulator is parked")
	}
	// unpark of a simulator that is not parked does nothing (no ps, no signal).
	if was, n, err := b.unpark("00000000-0000-0000-0000-000000000000"); was || n != 0 || err != nil {
		t.Fatalf("unpark of a running simulator: %v %d %v", was, n, err)
	}
}

func TestListingDropsParkMarksOfRunningSimulators(t *testing.T) {
	const running, parking = "11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222"
	b := &Backend{parked: map[string]bool{parkUDID: true, running: true, parking: true}, parking: map[string]bool{parking: true}}
	b.parkScan.Do(func() {})
	ps := strings.Join([]string{launchdSim(100, "Ts", parkUDID), launchdSim(200, "Ss", running), launchdSim(300, "Ss", parking)}, "\n")
	listed := 0
	list := func() (string, error) { listed++; return ps, nil }
	got := b.stillParked(list, []string{parkUDID, running, parking})
	if !got[parkUDID] || got[running] || !got[parking] {
		t.Fatalf("still parked = %v; want the stopped one and the one being parked", got)
	}
	if !b.Parked("ios:"+parkUDID) || b.Parked("ios:"+running) || !b.Parked("ios:"+parking) {
		t.Fatalf("marks after the check: %v", b.parked)
	}
	if b.stillParked(list, nil); listed != 1 {
		t.Fatalf("ps ran %d times; a listing with no marks must not run it", listed)
	}
}

func TestOnlyAgemuSimulatorsLeftStoppedAreMarkedParked(t *testing.T) {
	t.Setenv("AGEMU_IOS_ALLOW_FOREIGN", "")
	const foreign = "18962024-0BC7-40CD-9598-F9C4C8DD49E9"
	const running = "33333333-3333-3333-3333-333333333333"
	b := &Backend{parked: map[string]bool{}}
	b.parkScan.Do(func() {})
	ps := strings.Join([]string{launchdSim(100, "Ts", parkUDID), launchdSim(200, "Ts", foreign), launchdSim(300, "Ss", running)}, "\n")
	names := map[string]string{parkUDID: NamePrefix + "iphone-17-pro", foreign: "iPhone 17 Pro Max", running: NamePrefix + "ipad-mini"}
	var dirs []string
	b.markStopped(ps, func(dir string) string {
		dirs = append(dirs, dir)
		return names[filepath.Base(dir)]
	})
	if !b.Parked("ios:"+parkUDID) || b.Parked("ios:"+foreign) || b.Parked("ios:"+running) {
		t.Fatalf("marked %v; want only the stopped agemu simulator", b.parked)
	}
	if want := "/Users/me/Library/Developer/CoreSimulator/Devices/" + parkUDID; len(dirs) == 0 || dirs[0] != want {
		t.Fatalf("device directories %v, want %s first", dirs, want)
	}
	// With AGEMU_IOS_ALLOW_FOREIGN=1 agemu drives other simulators too, so it resumes them.
	t.Setenv("AGEMU_IOS_ALLOW_FOREIGN", "1")
	b.markStopped(ps, func(dir string) string { return names[filepath.Base(dir)] })
	if !b.Parked("ios:" + foreign) {
		t.Fatal("AGEMU_IOS_ALLOW_FOREIGN=1: the stopped foreign simulator is not marked")
	}
}

func TestALeanPassWaitsOutAPark(t *testing.T) {
	old := parkPoll
	parkPoll = 5 * time.Millisecond
	defer func() { parkPoll = old }()
	b := &Backend{parked: map[string]bool{parkUDID: true}, bootGen: map[string]int{parkUDID: 1}}
	b.parkScan.Do(func() {})
	b.bg, b.stopBg = context.WithCancel(context.Background())
	defer b.stopBg()
	go func() {
		time.Sleep(60 * time.Millisecond)
		b.mu.Lock()
		delete(b.parked, parkUDID)
		b.mu.Unlock()
	}()
	held, ok := b.waitWhileParked(parkUDID, 1)
	if !ok || held < 50*time.Millisecond {
		t.Fatalf("held %v ok %v; want the pass held until the resume", held, ok)
	}
	if held, ok := b.waitWhileParked(parkUDID, 1); !ok || held > 20*time.Millisecond {
		t.Fatalf("not parked: held %v ok %v; want no wait", held, ok)
	}
	// A newer boot of the device ends the wait.
	b.parked[parkUDID] = true
	go func() {
		time.Sleep(20 * time.Millisecond)
		b.mu.Lock()
		b.bootGen[parkUDID] = 2
		b.mu.Unlock()
	}()
	if _, ok := b.waitWhileParked(parkUDID, 1); ok {
		t.Fatal("a newer boot must end the wait with ok=false")
	}
}

func TestBackgroundPassesMoveByTheParkedTime(t *testing.T) {
	old := parkPoll
	parkPoll = 5 * time.Millisecond
	defer func() { parkPoll = old }()
	b := &Backend{parked: map[string]bool{parkUDID: true}, bootGen: map[string]int{parkUDID: 1}}
	b.parkScan.Do(func() {})
	b.bg, b.stopBg = context.WithCancel(context.Background())
	defer b.stopBg()
	ready := time.Now().Add(-time.Minute)
	e := &deviceBackground{b: b, d: simDevice{UDID: parkUDID}, gen: 1, ready: ready}
	go func() {
		time.Sleep(40 * time.Millisecond)
		b.mu.Lock()
		delete(b.parked, parkUDID)
		b.mu.Unlock()
	}()
	if !e.waitUntil(time.Second) {
		t.Fatal("waitUntil = false")
	}
	if moved := e.ready.Sub(ready); moved < 30*time.Millisecond {
		t.Fatalf("the later passes moved by %v; want about the parked time", moved)
	}
}

func TestParkLockIsOnePerSimulator(t *testing.T) {
	b := &Backend{}
	a1, a2, other := b.parkLock("A"), b.parkLock("A"), b.parkLock("B")
	if a1 != a2 || a1 == other {
		t.Fatal("parkLock must return the same lock for a simulator and another for the next")
	}
	// A shutdown of B is not held by a park of A.
	a1.Lock()
	defer a1.Unlock()
	if !other.TryLock() {
		t.Fatal("B's lock is held by A's park")
	}
	other.Unlock()
}
