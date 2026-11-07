package ios

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/api"
)

// Lines as runningboardd, CoreFoundation and the Swift runtime print them on iOS 26.5.
const (
	rbSignal    = "Notifying connection [osservice<com.apple.SpringBoard>:48658] that process [app<dev.agemu.testapp((null))>:12263] exited with context <RBSProcessExitContext| specific, status:<RBSProcessExitStatus| domain:signal(2) code:SIGTRAP(5)>>"
	rbVoluntary = "Notifying connection [osservice<com.apple.SpringBoard>:48658] that process [app<dev.agemu.testapp((null))>:22466] exited with context <RBSProcessExitContext| voluntary>"
	rbForceQuit = "Notifying connection [osservice<com.apple.SpringBoard>:48658] that process [app<dev.agemu.testapp((null))>:32478] exited with context <RBSProcessExitContext| specific, status:<RBSProcessExitStatus| domain:frontboard(10) code:force-quit(0xfbfbfbfb)>>"
	rbTCC       = "Notifying connection [osservice<com.apple.SpringBoard>:48658] that process [app<dev.agemu.testapp((null))>:51951] exited with context <RBSProcessExitContext| specific, status:<RBSProcessExitStatus| domain:tcc(11) code:0>>"
	nsLine      = "*** Terminating app due to uncaught exception 'NSInvalidArgumentException', reason: 'agemu probe exception'\n*** First throw call stack:\n(\n\t0   CoreFoundation                      0x00000001804c3dec __exceptionPreprocess + 172\n\t1   libobjc.A.dylib                     0x0000000180063438 objc_exception_throw + 72\n\t2   CoreFoundation                      0x00000001804c3980 -[NSException init] + 0\n\t3   AgemuTestApp                        0x000000010225eb9c $s12AgemuTestApp8LogProbeO5crashyySSFZTf4nd_n + 832\n\t4   SwiftUI                             0x00000001d34e9ee0 $s7SwiftUI12ButtonActionO14callAsFunctionyyF + <…>"
)

func TestParseExitContext(t *testing.T) {
	cases := []struct {
		msg               string
		pid               int
		ctx, domain, code string
	}{
		{rbSignal, 12263, "specific, status:<RBSProcessExitStatus| domain:signal(2) code:SIGTRAP(5)>", "signal", "SIGTRAP(5)"},
		{rbVoluntary, 22466, "voluntary", "", ""},
		{rbForceQuit, 32478, "specific, status:<RBSProcessExitStatus| domain:frontboard(10) code:force-quit(0xfbfbfbfb)>", "frontboard", "force-quit(0xfbfbfbfb)"},
		{rbTCC, 51951, "specific, status:<RBSProcessExitStatus| domain:tcc(11) code:0>", "tcc", "0"},
	}
	for _, c := range cases {
		pid, app, ctx, domain, code, ok := parseExitContext(c.msg)
		if !ok || pid != c.pid || app != "dev.agemu.testapp" || ctx != c.ctx || domain != c.domain || code != c.code {
			t.Errorf("parseExitContext(%q) = %d %q %q %q %q %v", c.msg, pid, app, ctx, domain, code, ok)
		}
	}
	if _, _, _, _, _, ok := parseExitContext("[app<dev.agemu.testapp((null))>:12263] termination reported by launchd (2, 5, 5)"); ok {
		t.Error("parsed a line that is not the exit line")
	}
}

func TestParseUncaught(t *testing.T) {
	exc, reason, frames, ok := parseUncaught(nsLine)
	want := []string{"CoreFoundation __exceptionPreprocess + 172", "libobjc.A.dylib objc_exception_throw + 72",
		"CoreFoundation -[NSException init] + 0", "AgemuTestApp $s12AgemuTestApp8LogProbeO5crashyySSFZTf4nd_n + 832"}
	if !ok || exc != "NSInvalidArgumentException" || reason != "agemu probe exception" || strings.Join(frames, "|") != strings.Join(want, "|") {
		t.Fatalf("parseUncaught = %q %q %q %v (the frame the log cut must be left out)", exc, reason, frames, ok)
	}
	// A reason with quotes and newlines, and no call stack.
	exc, reason, frames, ok = parseUncaught("*** Terminating app due to uncaught exception 'X', reason: 'it's\nbroken'")
	if !ok || exc != "X" || reason != "it's\nbroken" || len(frames) != 0 {
		t.Errorf("quoted reason: %q %q %v %v", exc, reason, frames, ok)
	}
	if _, _, _, ok := parseUncaught("Terminating soon"); ok {
		t.Error("parsed an ordinary line")
	}
}

func TestParseSwiftFatal(t *testing.T) {
	cases := map[string][2]string{
		"AgemuTestApp/App.swift:399: Fatal error: agemu probe fatalError":        {"Fatal error", "agemu probe fatalError (AgemuTestApp/App.swift:399)"},
		"Fatal error: Unexpectedly found nil while unwrapping an Optional value": {"Fatal error", "Unexpectedly found nil while unwrapping an Optional value"},
		"Lib/A.swift:7: Precondition failed":                                     {"Precondition failed", "(Lib/A.swift:7)"},
	}
	for in, want := range cases {
		exc, reason, ok := parseSwiftFatal(in)
		if !ok || exc != want[0] || reason != want[1] {
			t.Errorf("parseSwiftFatal(%q) = %q %q %v", in, exc, reason, ok)
		}
	}
	if _, _, ok := parseSwiftFatal("all is fine"); ok {
		t.Error("parsed an ordinary line")
	}
}

func TestCrashKinds(t *testing.T) {
	at := t0
	cases := []struct {
		name string
		p    appProc
		kind api.CrashKind
		exc  string
	}{
		{"exit seen, cause not yet", appProc{}, api.CrashKindExited, ""},
		{"exit()", appProc{exitContext: "voluntary"}, api.CrashKindExited, ""},
		{"SIGSEGV", appProc{exitContext: "x", domain: "signal", code: "SIGSEGV(11)"}, api.CrashKindCrash, "SIGSEGV"},
		{"SIGABRT", appProc{exitContext: "x", domain: "signal", code: "SIGABRT(6)"}, api.CrashKindCrash, "SIGABRT"},
		{"SIGKILL", appProc{exitContext: "x", domain: "signal", code: "SIGKILL(9)"}, api.CrashKindKilled, "SIGKILL"},
		{"watchdog", appProc{exitContext: "x", domain: "frontboard", code: "0x8badf00d"}, api.CrashKindKilled, "watchdog (0x8badf00d)"},
		{"other domain", appProc{exitContext: "x", domain: "jetsam", code: "1"}, api.CrashKindKilled, "jetsam 1"},
		{"exception wins over the signal", appProc{exitContext: "x", domain: "signal", code: "SIGABRT(6)", exception: "NSInvalidArgumentException", reason: "r"}, api.CrashKindCrash, "NSInvalidArgumentException"},
		{"Swift error before runningboardd", appProc{exception: "Fatal error", reason: "r"}, api.CrashKindCrash, "Fatal error"},
	}
	for _, c := range cases {
		p := c.p
		p.pid, p.app, p.exited = 1, "a", at
		got := p.crash()
		if got == nil || got.Kind != c.kind || got.Exception != c.exc || !got.Time.Equal(at) {
			t.Errorf("%s: %+v", c.name, got)
		}
	}
	running := appProc{pid: 1, app: "a"}
	if running.crash() != nil {
		t.Error("a running process has a crash")
	}
}

func TestReportUpgradesTheCrash(t *testing.T) {
	r := &ipsReport{path: "/r/x.ips", process: "AgemuTestApp", kind: api.CrashKindCrash, exception: "EXC_CRASH (SIGABRT)",
		reason: "Abort trap: 6", thread: "Thread 0 (queue com.apple.main-thread)", frames: []string{"CoreFoundation __exceptionPreprocess + 160", "AgemuTestApp specialized static LogProbe.crash(_:) + 832"}}
	// The exception the app named stays; the report adds the demangled frames, thread and path,
	// and the time stays the exit's.
	p := appProc{pid: 1, app: "a", exited: t0, exitContext: "x", domain: "signal", code: "SIGABRT(6)",
		exception: "NSInvalidArgumentException", reason: "agemu probe exception", frames: []string{"log frame"}, report: r}
	c := p.crash()
	if c.Exception != "NSInvalidArgumentException" || c.Reason != "agemu probe exception" || len(c.Frames) != 2 ||
		c.Thread != r.thread || c.Report != r.path || !c.Time.Equal(t0) || c.Process != "AgemuTestApp" {
		t.Errorf("with the app's exception: %+v", c)
	}
	// A signal alone takes the report's exception and reason.
	p = appProc{pid: 1, app: "a", exited: t0, exitContext: "x", domain: "signal", code: "SIGSEGV(11)",
		report: &ipsReport{exception: "EXC_BAD_ACCESS (SIGSEGV)", reason: "KERN_INVALID_ADDRESS at 0x10", kind: api.CrashKindCrash}}
	if c := p.crash(); c.Exception != "EXC_BAD_ACCESS (SIGSEGV)" || c.Reason != "KERN_INVALID_ADDRESS at 0x10" {
		t.Errorf("signal with report: %+v", c)
	}
}

func TestExpectedExits(t *testing.T) {
	be, dl, _ := testCapture(t)
	ended := func(pid int, app string, at time.Time) *appProc {
		p := &appProc{pid: pid, app: app, exited: at, exitContext: "x", domain: "signal", code: "SIGKILL(9)"}
		dl.procs[pid] = p
		return p
	}
	before := ended(1, "a", time.Now().Add(-time.Second))
	time.Sleep(time.Millisecond)
	done := be.expectExits(testUDID, "a")
	during := ended(2, "a", time.Now())
	launchedBefore := ended(6, "a", time.Now())
	launchedBefore.launched = time.Now().Add(-time.Minute)
	otherApp := ended(3, "b", time.Now())
	keep := ended(4, "a", time.Now())
	done(4)
	// An app agemu launched after the call began (after an install, say) and that crashed at
	// once, within the slack, is reported.
	time.Sleep(time.Millisecond)
	launchedAfter := ended(7, "a", time.Now().Add(time.Second))
	launchedAfter.launched = time.Now()
	dl.mu.Lock()
	defer dl.mu.Unlock()
	if dl.expectedLocked(before) || !dl.expectedLocked(during) || dl.expectedLocked(otherApp) || dl.expectedLocked(keep) ||
		dl.expectedLocked(launchedAfter) {
		t.Errorf("expected: before %v during %v other app %v the relaunched one %v launched after %v; want false true false false false",
			dl.expectedLocked(before), dl.expectedLocked(during), dl.expectedLocked(otherApp), dl.expectedLocked(keep), dl.expectedLocked(launchedAfter))
	}
	if !dl.expectedLocked(launchedBefore) {
		t.Error("a process agemu launched before the call is not covered")
	}
	if late := (&appProc{pid: 5, app: "a", exited: time.Now().Add(expectSlack + time.Second)}); dl.expectedLocked(late) {
		t.Error("an exit after the slack counts as agemu's")
	}
	// Exits someone asked for are never reports.
	for _, p := range []*appProc{{domain: "frontboard", code: "force-quit(0xfbfbfbfb)"}, {domain: "tcc", code: "0"}} {
		p.pid, p.app, p.exited = 9, "z", time.Now().Add(time.Hour)
		if !dl.expectedLocked(p) {
			t.Errorf("%s %s is reported", p.domain, p.code)
		}
	}
}

// A crash inside an expectation's window is the app's, not agemu's: a process launched before a
// permission call that segfaults during it (or right after an install, known only from
// runningboardd's lines) is reported by AppState and Crashes, with its .ips.
func TestCrashInsideAnExpectationIsReported(t *testing.T) {
	be, dl, _ := testCapture(t)
	id := "ios:" + testUDID
	since := time.Now().Add(-time.Minute)
	gone := deadPid(t)
	done := be.expectExits(testUDID, "a")
	at := time.Now()
	dl.mu.Lock()
	dl.procs[gone] = &appProc{pid: gone, app: "a", launched: since, exited: at, exitContext: "x", domain: "signal", code: "SIGSEGV(11)"}
	dl.mu.Unlock()
	done(0)
	st, err := be.AppState(context.Background(), id, "a", since)
	if err != nil || st.Crash == nil || st.Crash.Kind != api.CrashKindCrash || st.Crash.Exception != "SIGSEGV" {
		t.Fatalf("segfault during a permission call: %+v %v", st, err)
	}
	// Each kind of evidence counts; agemu's own ends (and SIGKILL/SIGTERM, exit()) stay hidden.
	cases := []struct {
		p        appProc
		reported bool
	}{
		{appProc{exitContext: "x", domain: "signal", code: "SIGABRT(6)"}, true},
		{appProc{exception: "Fatal error", reason: "r"}, true},
		{appProc{report: &ipsReport{path: "/r/x.ips"}}, true},
		{appProc{exitContext: "x", domain: "signal", code: "SIGKILL(9)"}, false},
		{appProc{exitContext: "x", domain: "signal", code: "SIGTERM(15)"}, false},
		{appProc{exitContext: "x", domain: "runningboard", code: "0"}, false},
		{appProc{exitContext: "voluntary"}, false},
		{appProc{}, false},
	}
	dl.mu.Lock()
	defer dl.mu.Unlock()
	for i, c := range cases {
		p := c.p
		p.pid, p.app, p.exited = 100+i, "a", at
		if got := !dl.expectedLocked(&p); got != c.reported {
			t.Errorf("%+v: reported %v, want %v", c.p, got, c.reported)
		}
	}
}

// deadPid returns the pid of a process that has exited.
func deadPid(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Skip("cannot run true:", err)
	}
	return cmd.Process.Pid
}

func TestAppState(t *testing.T) {
	be, dl, s := testCapture(t)
	id := "ios:" + testUDID
	since := time.Now().Add(-time.Minute)
	// Running: the launched process is alive (this test's own pid stands in for it).
	dl.procs[os.Getpid()] = &appProc{pid: os.Getpid(), app: "a", launched: since}
	st, err := be.AppState(context.Background(), id, "a", since)
	if err != nil || !st.Running || st.Crash != nil {
		t.Fatalf("running: %+v %v", st, err)
	}
	// A launched process that is gone without an exit seen is noticed as exited.
	gone := deadPid(t)
	dl.procs[gone] = &appProc{pid: gone, app: "b", launched: since}
	st, _ = be.AppState(context.Background(), id, "b", since)
	if st.Running || st.Crash == nil || st.Crash.Kind != api.CrashKindExited || st.Crash.Pid != gone {
		t.Fatalf("gone: %+v", st)
	}
	// A process only seen in the log waits for runningboardd's line.
	dl.procs[gone+1] = &appProc{pid: gone + 1, app: "c"}
	if st, _ := be.AppState(context.Background(), id, "c", since); st.Crash != nil {
		t.Fatalf("log-only process reported: %+v", st)
	}
	// Right after an exit, AppState waits for the stream to say how it ended.
	pid := gone + 2
	dl.mu.Lock()
	dl.procs[pid] = &appProc{pid: pid, app: "d", launched: since, exited: time.Now()}
	dl.mu.Unlock()
	go func() {
		time.Sleep(30 * time.Millisecond)
		app := testApp + "/AgemuTestApp"
		ingestLine(t, be, dl, s, line(time.Now(), "Default", pid, app, testRT+"/usr/lib/swift/libswiftCore.dylib", "", "", "A.swift:3: Fatal error: boom"))
	}()
	start := time.Now()
	st, _ = be.AppState(context.Background(), id, "d", since)
	if st.Crash == nil || st.Crash.Kind != api.CrashKindCrash || st.Crash.Exception != "Fatal error" || time.Since(start) > detailWait+50*time.Millisecond {
		t.Fatalf("detail wait: %+v after %v", st.Crash, time.Since(start))
	}
	// An older crash than since is not reported; an unknown device knows nothing.
	if st, _ := be.AppState(context.Background(), id, "d", time.Now().Add(time.Minute)); st.Crash != nil {
		t.Errorf("crash before since: %+v", st.Crash)
	}
	if st, err := be.AppState(context.Background(), "ios:00000000-0000-0000-0000-000000000000", "a", since); err != nil || st.Running || st.Crash != nil {
		t.Errorf("unknown device: %+v %v", st, err)
	}
}

func TestObserveStreamLines(t *testing.T) {
	be, dl, s := testCapture(t)
	rb := testRT + "/usr/libexec/runningboardd"
	app := testApp + "/AgemuTestApp"
	cf := testRT + "/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation"
	ingestLine(t, be, dl, s, line(t0, "Default", 48661, rb, rb, "com.apple.runningboard", "process", "[app<dev.agemu.testapp((null))>:12263] Memory Limits: active 0 inactive 0"))
	ingestLine(t, be, dl, s, line(t0.Add(time.Second), "Default", 12263, app, cf, "", "", nsLine))
	ingestLine(t, be, dl, s, line(t0.Add(time.Second+10*time.Millisecond), "Default", 48661, rb, rb, "com.apple.runningboard", "process", strings.Replace(rbSignal, "SIGTRAP(5)", "SIGABRT(6)", 1)))
	p := dl.procs[12263]
	if p == nil || p.app != "dev.agemu.testapp" || p.process != "AgemuTestApp" || !p.exited.Equal(t0.Add(time.Second+10*time.Millisecond)) ||
		p.domain != "signal" || p.exception != "NSInvalidArgumentException" || len(p.frames) != 4 {
		t.Fatalf("process record: %+v", p)
	}
	// A FrontBoard force quit is recorded and is not a report.
	ingestLine(t, be, dl, s, line(t0.Add(2*time.Second), "Default", 48661, rb, rb, "", "", rbForceQuit))
	if q := dl.procs[32478]; q == nil || !q.requested() {
		t.Fatalf("force quit: %+v", q)
	}
	// A user app whose executable is named runningboardd cannot report other apps' exits.
	fake := testApp[:strings.LastIndex(testApp, "/")] + "/Fake.app/runningboardd"
	ingestLine(t, be, dl, s, line(t0.Add(3*time.Second), "Default", 777, fake, fake, "", "", strings.Replace(rbSignal, ":12263]", ":44444]", 1)))
	if q := dl.procs[44444]; q != nil {
		t.Fatalf("a user app's runningboardd line made a record: %+v", q)
	}
}

func TestParseIPS(t *testing.T) {
	read := func(name string) (*ipsReport, bool) {
		t.Helper()
		file := filepath.Join("testdata", name)
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		r, ok, err := parseIPS(file, data)
		if err != nil {
			t.Fatal(err)
		}
		return r, ok
	}
	r, ok := read("exception.ips")
	if !ok || r.udid != testUDID || r.pid != 7881 || r.app != "dev.agemu.testapp" || r.process != "AgemuTestApp" ||
		r.exception != "EXC_CRASH (SIGABRT)" || r.reason != "Abort trap: 6" || r.thread != "Thread 0 (queue com.apple.main-thread)" || r.kind != api.CrashKindCrash {
		t.Fatalf("exception.ips: %+v", r)
	}
	// An uncaught NSException's frames are the exception's own backtrace.
	if len(r.frames) != api.MaxCrashFrames || r.frames[0] != "CoreFoundation __exceptionPreprocess + 160" || r.frames[3] != "AgemuTestApp specialized static LogProbe.crash(_:) + 832" {
		t.Errorf("exception frames: %q", r.frames[:4])
	}
	if want := time.Date(2026, 11, 7, 6, 45, 35, 71500000, time.FixedZone("", 3600)); !r.time.Equal(want) {
		t.Errorf("time %v, want %v", r.time, want)
	}
	r, ok = read("badaccess.ips")
	if !ok || r.exception != "EXC_BAD_ACCESS (SIGSEGV)" || r.reason != "KERN_INVALID_ADDRESS at 0x0000000000000010" || !strings.HasPrefix(r.frames[0], "AgemuTestApp ") {
		t.Errorf("badaccess.ips: %+v", r)
	}
	r, ok = read("fatal.ips")
	if !ok || r.exception != "EXC_BREAKPOINT (SIGTRAP)" || len(r.frames) == 0 {
		t.Errorf("fatal.ips: %+v", r)
	}
	// A Mac process's report is not a simulator's.
	if r, ok := read("mac.ips"); ok || r != nil {
		t.Errorf("mac.ips: %+v", r)
	}
	if _, ok, err := parseIPS("x", []byte(`{"bug_type":"298"}`+"\n{}")); ok || err != nil {
		t.Errorf("a jetsam report: ok %v err %v", ok, err)
	}
	if _, _, err := parseIPS("x", []byte(`{"bug_type":"309"}`+"\n{\"procPath\": \"/CoreSimulator/Devices/")); err == nil {
		t.Error("a half-written report parsed")
	}
}

func TestReportDirAndCrashes(t *testing.T) {
	dir := t.TempDir()
	copyFile := func(name, as string, mod time.Time) {
		data, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, as)
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mod, mod); err != nil {
			t.Fatal(err)
		}
	}
	written := time.Now().Add(-2 * time.Hour)
	copyFile("exception.ips", "AgemuTestApp-1.ips", written)
	copyFile("badaccess.ips", "AgemuTestApp-2.ips", written)
	copyFile("mac.ips", "watchman-1.ips", written)
	other, _ := os.ReadFile(filepath.Join("testdata", "fatal.ips"))
	other = []byte(strings.ReplaceAll(string(other), testUDID, "AAAAAAAA-95B5-4D1D-A495-E08DDE41A661"))
	if err := os.WriteFile(filepath.Join(dir, "AgemuTestApp-3.ips"), other, 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o600)

	// A file written before since cannot hold a crash after it: it is listed, not read.
	old := &reportDir{dir: dir, seen: map[string]reportEntry{}}
	if _, err := old.scan(time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if old.seen["AgemuTestApp-1.ips"].parsed || !old.seen["AgemuTestApp-3.ips"].parsed {
		t.Errorf("read before since: %+v", old.seen)
	}

	rd := &reportDir{dir: dir, seen: map[string]reportEntry{}}
	reports, err := rd.scan(time.Time{})
	if err != nil || len(reports) != 3 {
		t.Fatalf("scan: %d reports, %v", len(reports), err)
	}

	be, dl, _ := testCapture(t)
	be.reports = rd
	// The NSException crash, seen by the watcher with its log details; the report's frames join.
	dl.procs[7881] = &appProc{pid: 7881, app: "dev.agemu.testapp", process: "AgemuTestApp", exited: t0, exitContext: "x",
		domain: "signal", code: "SIGABRT(6)", exception: "NSInvalidArgumentException", reason: "agemu probe exception"}
	// A force quit is not a report.
	dl.procs[5] = &appProc{pid: 5, app: "dev.agemu.testapp", exited: t0, domain: "frontboard", code: "force-quit(0xfbfbfbfb)"}
	cs, err := be.crashes(testUDID, api.CrashQuery{App: "dev.agemu.testapp"})
	if err != nil || len(cs) != 2 {
		t.Fatalf("crashes: %d, %v: %+v", len(cs), err, cs)
	}
	// Oldest first: the watched crash (its exit time) with the report's frames, then the report
	// of a crash the watcher did not see; not the other device's.
	first, second := cs[0], cs[1]
	if first.Pid != 7881 || first.Exception != "NSInvalidArgumentException" || !first.Time.Equal(t0) || first.Report == "" ||
		len(first.Frames) != api.MaxCrashFrames || second.Pid != 16612 || second.Exception != "EXC_BAD_ACCESS (SIGSEGV)" {
		t.Errorf("merged crashes: %+v / %+v", first, second)
	}
	if cs, _ := be.crashes(testUDID, api.CrashQuery{App: "other.app"}); len(cs) != 0 {
		t.Errorf("other app: %+v", cs)
	}
}

func TestProcWatcher(t *testing.T) {
	exits := make(chan int, 4)
	w, err := newProcWatcher(func(pid int, _ time.Time) { exits <- pid })
	if err != nil {
		t.Skip("no process watcher here:", err)
	}
	defer w.close()
	cmd := exec.Command("sleep", "0.2")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := w.add(cmd.Process.Pid); err != nil {
		t.Fatal(err)
	}
	go func() { _ = cmd.Wait() }()
	select {
	case pid := <-exits:
		if pid != cmd.Process.Pid {
			t.Errorf("exit of %d, want %d", pid, cmd.Process.Pid)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no exit seen")
	}
	if err := w.add(deadPid(t)); err == nil {
		t.Error("watching a process that is gone did not fail")
	}
}

func TestBundleID(t *testing.T) {
	if _, err := exec.LookPath("plutil"); err != nil {
		t.Skip("no plutil")
	}
	dir := t.TempDir()
	app := filepath.Join(dir, "A.app")
	if err := os.Mkdir(app, 0o755); err != nil {
		t.Fatal(err)
	}
	plist := filepath.Join(app, "Info.plist")
	xml := `<?xml version="1.0" encoding="UTF-8"?><plist version="1.0"><dict><key>CFBundleIdentifier</key><string>com.example.a</string></dict></plist>`
	if err := os.WriteFile(plist, []byte(xml), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := bundleID(context.Background(), app); got != "com.example.a" {
		t.Errorf("XML Info.plist: %q", got)
	}
	if err := exec.Command("plutil", "-convert", "binary1", plist).Run(); err != nil {
		t.Fatal(err)
	}
	if got := bundleID(context.Background(), app); got != "com.example.a" {
		t.Errorf("binary Info.plist: %q", got)
	}
	if got := bundleID(context.Background(), filepath.Join(dir, "missing.app")); got != "" {
		t.Errorf("no bundle: %q", got)
	}
}

func BenchmarkAppState(b *testing.B) {
	be := &Backend{logs: map[string]*deviceLogs{}}
	dl := be.devLogs(testUDID)
	since := time.Now().Add(-time.Minute)
	dl.procs[os.Getpid()] = &appProc{pid: os.Getpid(), app: "a", launched: since}
	for i := 0; i < 20; i++ {
		dl.procs[1000000+i] = &appProc{pid: 1000000 + i, app: "other", exited: since}
	}
	ctx := context.Background()
	for i := 0; i < b.N; i++ {
		if st, _ := be.AppState(ctx, "ios:"+testUDID, "a", since); !st.Running {
			b.Fatal("not running")
		}
	}
}
