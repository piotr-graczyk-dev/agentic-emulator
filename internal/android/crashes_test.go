package android

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/api"
)

// The texts below are what agemu-lab (Android 17, emulator 37.1) logged for the probe app's
// lab buttons (testapps/android, LabActivity), shortened.

const javaCrashText = `FATAL EXCEPTION: main
Process: dev.agemu.probe, PID: 5267
java.lang.IllegalStateException: probe crash #2
	at dev.agemu.probe.LabActivity.crashJava(LabActivity.java:152)
	at dev.agemu.probe.LabActivity.act(LabActivity.java:119)
	at dev.agemu.probe.LabActivity$1.onClick(LabActivity.java:72)
	at android.view.View.performClick(View.java:8220)
	at android.os.Looper.loop(Looper.java:398)
	at android.app.ActivityThread.main(ActivityThread.java:9569)`

const remoteCrashText = `FATAL EXCEPTION: main
Process: dev.agemu.probe:remote, PID: 5494
java.lang.RuntimeException: Unable to start service dev.agemu.probe.RemoteService@7ef5d78 with Intent { xflg=0x4 cmp=dev.agemu.probe/.RemoteService (has extras) }
	at android.app.ActivityThread.handleServiceArgs(ActivityThread.java:5845)
	at android.app.ActivityThread.-$$Nest$mhandleServiceArgs(ActivityThread.java:0)
	at android.app.ActivityThread$H.handleMessage(ActivityThread.java:2870)
	at android.os.Looper.loop(Looper.java:398)
	at android.app.ActivityThread.main(ActivityThread.java:9569)
Caused by: java.lang.IllegalArgumentException: remote crash #2
	at dev.agemu.probe.RemoteService.onStartCommand(RemoteService.java:19)
	at android.app.ActivityThread.handleServiceArgs(ActivityThread.java:5829)
	... 3 more`

// The tombstone crash_dump logs (tag DEBUG), in the records it came in.
var tombstoneRecords = []string{
	"*** *** *** *** *** *** *** *** *** *** *** *** *** *** *** ***\n",
	`Build fingerprint: 'google/sdk_gphone16k_arm64/emu64a16k:17/CP31.260623.012/16064790:user/dev-keys'
Revision: '0'
ABI: 'arm64'
`,
	`Executable: /system/bin/app_process64
Cmdline: dev.agemu.probe
pid: 5344, ppid: 437, tid: 5344, name: dev.agemu.probe  >>> dev.agemu.probe <<<
uid: 10229
signal 11 (SIGSEGV), code 1 (SEGV_MAPERR), fault addr 0x0000000000000000 (write)
Cause: null pointer dereference

    x0  b400780f3e0405f0  x1  0000780d44030130  x2  b400780f4e059ef8  x3  b400780ede057970
    lr  0000780e09fc8274  sp  00007fffcfc975a0  pc  0000780dd2bdc6dc  pst 0000000060001000

25 total frames
backtrace:
      #00 pc 00000000000046dc  /data/app/~~G_5T==/dev.agemu.probe-sO==/lib/arm64/libprobe.so (probe_null_write+8)
      #01 pc 0000000000288270  /apex/com.android.art/lib64/libart.so (art_quick_generic_jni_trampoline+144) (BuildId: a686)
`,
	`      #02 pc 00000000006bf164  /apex/com.android.art/lib64/libart.so (nterp_helper+116) (BuildId: a686)
      #03 pc 00000000000016ba  [anon:dalvik-classes.dex extracte...base.apk] (dev.agemu.probe.LabActivity.act+974)

Note: To display stack pointer information, use the pbtombstone tool:
        pbtombstone --display-sp tombstone_XX.pb
`,
	// A full tombstone goes on with the other threads: not the crashing thread's frames.
	`--- --- --- --- --- --- --- --- --- --- --- --- --- --- --- ---
pid: 5344, tid: 5350, name: RenderThread  >>> dev.agemu.probe <<<
backtrace:
      #00 pc 00000000000dfe48  /apex/com.android.runtime/lib64/bionic/libc.so (__ioctl+8)
`,
}

const anrDropbox = `Drop box contents: 16 entries
Searching for: data_app_anr

========================================
2026-11-07 08:05:04 data_app_anr (compressed text, 7872 bytes)
Process: dev.agemu.probe
PID: 5538
UID: 10229
Subject: Input dispatching timed out (f41a67 dev.agemu.probe/dev.agemu.probe.LabActivity is not responding. Waited 15004ms for MotionEvent).
Data File: /data/anr/anr_2026-11-07-08-05-04-319

----- pid 5538 at 2026-11-07 08:05:04.320 -----
"main" prio=5 tid=1 Sleeping
  | group="main" sCount=1 ucsCount=0 flags=1 obj=0x71ae8000 self=0xb400780ece00d380
  | held mutexes=
  at java.lang.Thread.sleep(Native method)
  - sleeping on <0x0a8512e8> (a java.lang.Object)
  at java.lang.Thread.sleep0(Thread.java:687)
  at android.os.SystemClock.sleep(SystemClock.java:141)
  at dev.agemu.probe.LabActivity.act(LabActivity.java:131)

"Signal Catcher" daemon prio=10 tid=2 Runnable
  at nothing.Else(Else.java:1)
`

func TestParseJavaCrash(t *testing.T) {
	j := parseJavaCrash(javaCrashText)
	if j.thread != "main" || j.process != "dev.agemu.probe" || j.pid != 5267 {
		t.Errorf("head %+v", j)
	}
	if j.exception != "java.lang.IllegalStateException" || j.reason != "probe crash #2" {
		t.Errorf("exception %q reason %q", j.exception, j.reason)
	}
	if len(j.frames) != 6 || j.frames[0] != "at dev.agemu.probe.LabActivity.crashJava(LabActivity.java:152)" {
		t.Errorf("frames %q", j.frames)
	}
}

func TestParseJavaCrashRootCause(t *testing.T) {
	j := parseJavaCrash(remoteCrashText)
	// The root cause, as ActivityManager reports it (am_crash).
	if j.exception != "java.lang.IllegalArgumentException" || j.reason != "remote crash #2" || j.process != "dev.agemu.probe:remote" {
		t.Errorf("got %+v", j)
	}
	// Its frames, completed with the 3 it shares with the wrapping exception.
	want := []string{
		"at dev.agemu.probe.RemoteService.onStartCommand(RemoteService.java:19)",
		"at android.app.ActivityThread.handleServiceArgs(ActivityThread.java:5829)",
		"at android.app.ActivityThread$H.handleMessage(ActivityThread.java:2870)",
		"at android.os.Looper.loop(Looper.java:398)",
		"at android.app.ActivityThread.main(ActivityThread.java:9569)",
	}
	if strings.Join(j.frames, "\n") != strings.Join(want, "\n") {
		t.Errorf("frames\n%s\nwant\n%s", strings.Join(j.frames, "\n"), strings.Join(want, "\n"))
	}
}

func TestParseJavaCrashMessageAndSuppressed(t *testing.T) {
	j := parseJavaCrash("FATAL EXCEPTION: worker-1\nProcess: com.x, PID: 9\njava.lang.Error: line one\nline two\n\tat a.B.c(B.java:1)\n\tSuppressed: java.io.IOException: closing\n\t\tat a.C.close(C.java:2)\n\tat a.B.d(B.java:3)")
	if j.thread != "worker-1" || j.exception != "java.lang.Error" || j.reason != "line one\nline two" {
		t.Errorf("got %+v", j)
	}
	if strings.Join(j.frames, "|") != "at a.B.c(B.java:1)|at a.B.d(B.java:3)" {
		t.Errorf("frames %q (a suppressed exception's frames are not the thread's)", j.frames)
	}
	// An exception without a message.
	if j := parseJavaCrash("FATAL EXCEPTION: main\nProcess: com.x, PID: 9\njava.lang.NullPointerException\n\tat a.B.c(B.java:1)"); j.exception != "java.lang.NullPointerException" || j.reason != "" {
		t.Errorf("no message: %+v", j)
	}
}

func TestParseTombstone(t *testing.T) {
	text := strings.Join(tombstoneRecords, "")
	tb := parseTombstone(text)
	if tb.pid != 5344 || tb.thread != "dev.agemu.probe" || tb.process != "dev.agemu.probe" {
		t.Errorf("head %+v", tb)
	}
	if tb.signal != "SIGSEGV (SEGV_MAPERR)" || tb.reason != "null pointer dereference" {
		t.Errorf("signal %q reason %q", tb.signal, tb.reason)
	}
	if len(tb.frames) != 4 || !strings.HasPrefix(tb.frames[0], "#00 pc 00000000000046dc") || !strings.Contains(tb.frames[0], "probe_null_write+8") {
		t.Errorf("frames %q", tb.frames)
	}
	// Before the backtrace arrives.
	if early := parseTombstone(strings.Join(tombstoneRecords[:2], "")); early.pid != 0 || len(early.frames) != 0 {
		t.Errorf("early %+v", early)
	}
	abort := parseTombstone("pid: 5390, ppid: 437, tid: 5390, name: dev.agemu.probe  >>> dev.agemu.probe <<<\nsignal 6 (SIGABRT), code -1 (SI_QUEUE), fault addr --------\nAbort message: 'probe abort #2'\nCause: ignored when there is an abort message\n")
	if abort.signal != "SIGABRT" || abort.reason != "probe abort #2" {
		t.Errorf("abort %+v", abort)
	}
}

func TestParseANRDropbox(t *testing.T) {
	m := parseANRDropbox(anrDropbox)
	e, ok := m[5538]
	if !ok || len(m) != 1 {
		t.Fatalf("entries %v", m)
	}
	if e.file != "/data/anr/anr_2026-11-07-08-05-04-319" {
		t.Errorf("file %q", e.file)
	}
	want := "at java.lang.Thread.sleep(Native method)|at java.lang.Thread.sleep0(Thread.java:687)|at android.os.SystemClock.sleep(SystemClock.java:141)|at dev.agemu.probe.LabActivity.act(LabActivity.java:131)"
	if strings.Join(e.frames, "|") != want {
		t.Errorf("frames %q", e.frames)
	}
}

// crashOf returns the capture's records (oldest first).
func crashOf(c *logCapture) []api.Crash {
	var out []api.Crash
	for _, r := range c.w.recs {
		out = append(out, c.report(r))
	}
	return out
}

func TestWatchJavaCrash(t *testing.T) {
	c := testCapture(t)
	since := guest0.Add(guestOffset)
	c.add(line(lidMain, 5267, probeUID, 100, 4, "AgemuLab", "lab: crash-java"), time.Time{})
	c.add(line(lidCrash, 5267, probeUID, 113, 6, "AndroidRuntime", javaCrashText), time.Time{})
	// The notice can go out now: the process's own report has it all.
	st := c.appState("dev.agemu.probe", since)
	if st.Crash == nil || st.Crash.Kind != api.CrashKindCrash || st.Crash.Exception != "java.lang.IllegalStateException" || len(st.Crash.Frames) != 6 {
		t.Fatalf("state after the report: %+v", st.Crash)
	}
	if !st.Running {
		t.Error("the :remote process still lives: running")
	}
	want := guest0.Add(113*time.Millisecond + guestOffset)
	if !st.Crash.Time.Equal(want) || st.Crash.Thread != "main" || st.Crash.Pid != 5267 || st.Crash.App != "dev.agemu.probe" {
		t.Errorf("crash %+v", st.Crash)
	}
	c.add(event(115, evAmCrash, 5267, 0, "dev.agemu.probe", 816332356, "java.lang.IllegalStateException", "probe crash #2", "LabActivity.java", 152, 0), time.Time{})
	c.add(event(169, evAmProcDied, 0, 5267, "dev.agemu.probe", 0, 2), time.Time{})
	delete(c.w.procs, 5494)
	cs := crashOf(c)
	if len(cs) != 1 {
		t.Fatalf("records %+v", cs)
	}
	if !strings.HasPrefix(cs[0].Report, "dropbox data_app_crash ") || !cs[0].Time.Equal(want) {
		t.Errorf("record %+v", cs[0])
	}
	if c.appState("dev.agemu.probe", since).Running {
		t.Error("running after the crash")
	}
	if c.appState("dev.agemu.probe", want.Add(time.Millisecond)).Crash != nil {
		t.Error("a crash before since")
	}
}

func TestWatchNativeCrash(t *testing.T) {
	c := testCapture(t)
	c.names[5344] = "dev.agemu.probe"
	c.w.procs = map[int]procInfo{5344: {uid: probeUID, name: "dev.agemu.probe"}}
	c.add(line(lidCrash, 5344, probeUID, 1, 7, "libc", "Fatal signal 11 (SIGSEGV), code 1 (SEGV_MAPERR), fault addr 0x0 in tid 5344 (dev.agemu.probe), pid 5344 (dev.agemu.probe)"), time.Time{})
	st := c.appState("dev.agemu.probe", guest0)
	if st.Crash == nil || st.Crash.Exception != "SIGSEGV (SEGV_MAPERR)" || st.Crash.Reason != "fault addr 0x0" || st.Crash.Thread != "dev.agemu.probe" {
		t.Fatalf("at the signal: %+v", st.Crash)
	}
	if st.Running {
		t.Error("a process being dumped counts as running")
	}
	c.add(line(lidMain, 350, 1058, 106, 4, "tombstoned", "received crash request for pid 5344"), time.Time{})
	for i, r := range tombstoneRecords {
		c.add(line(lidCrash, 5371, probeUID, 810+i, 7, "DEBUG", r), time.Time{})
	}
	c.add(line(lidMain, 350, 1058, 816, 6, "tombstoned", "Tombstone written to: tombstone_01"), time.Time{})
	// am_crash comes from system_server's pid for a native crash.
	c.add(event(818, evAmCrash, 702, 0, "dev.agemu.probe", 814235204, "Native crash", "Segmentation fault", "unknown", 0, 0), time.Time{})
	c.add(event(871, evAmProcDied, 0, 5344, "dev.agemu.probe", 0, 2), time.Time{})
	cs := crashOf(c)
	if len(cs) != 1 {
		t.Fatalf("records %+v", cs)
	}
	cr := cs[0]
	if !cr.Time.Equal(guest0.Add(time.Millisecond+guestOffset)) || cr.Exception != "SIGSEGV (SEGV_MAPERR)" || cr.Reason != "null pointer dereference" {
		t.Errorf("record %+v", cr)
	}
	if len(cr.Frames) != 4 || !strings.Contains(cr.Frames[0], "probe_null_write") || cr.Report != "/data/tombstones/tombstone_01" {
		t.Errorf("frames %q report %q", cr.Frames, cr.Report)
	}
}

func TestWatchANR(t *testing.T) {
	c := testCapture(t)
	c.names[5538] = "dev.agemu.probe"
	c.w.procs = map[int]procInfo{5538: {uid: probeUID, name: "dev.agemu.probe"}}
	c.add(event(15000, evAmANR, 0, 5538, "dev.agemu.probe", 816332356, "Input dispatching timed out (f41a67 dev.agemu.probe/dev.agemu.probe.LabActivity is not responding. Waited 15004ms for MotionEvent)."), time.Time{})
	st := c.appState("dev.agemu.probe", guest0)
	if st.Crash == nil || st.Crash.Kind != api.CrashKindANR || st.Crash.Exception != "ANR" || !strings.HasPrefix(st.Crash.Reason, "Input dispatching timed out") {
		t.Fatalf("anr %+v", st.Crash)
	}
	if !st.Running {
		t.Error("an ANR leaves the app running")
	}
	// crashes reads the main thread's stack from dropbox.
	asked := 0
	c.shell = func(ctx context.Context, cmd string) (string, error) {
		asked++
		if cmd != "dumpsys dropbox --print data_app_anr" {
			t.Errorf("command %q", cmd)
		}
		return anrDropbox, nil
	}
	c.w.recs[0].tried = time.Time{}
	c.anrFrames(context.Background(), api.CrashQuery{App: "dev.agemu.probe"})
	cr := crashOf(c)[0]
	if len(cr.Frames) != 4 || cr.Report != "/data/anr/anr_2026-11-07-08-05-04-319" {
		t.Fatalf("after dropbox: %+v", cr)
	}
	c.anrFrames(context.Background(), api.CrashQuery{})
	if asked != 1 {
		t.Errorf("dropbox read %d times", asked)
	}
	// An entry not written yet is looked for again after anrRetry, not a second later, and no
	// longer once the ANR is anrLookFor old.
	c.add(event(16000, evAmANR, 0, 5539, "dev.agemu.probe", 0, "later"), time.Time{})
	r := c.w.recs[len(c.w.recs)-1]
	c.anrFrames(context.Background(), api.CrashQuery{})
	c.anrFrames(context.Background(), api.CrashQuery{})
	if asked != 2 {
		t.Errorf("looked %d times right away, want once more", asked-1)
	}
	r.tried = time.Now().Add(-anrRetry - time.Millisecond)
	c.anrFrames(context.Background(), api.CrashQuery{})
	if asked != 3 {
		t.Errorf("no look %v after the last", anrRetry)
	}
	r.tried, r.made = time.Time{}, time.Now().Add(-anrLookFor)
	c.anrFrames(context.Background(), api.CrashQuery{})
	if asked != 3 {
		t.Errorf("looked for an ANR %v old", anrLookFor)
	}
}

func TestWatchExitWithoutCrash(t *testing.T) {
	c := testCapture(t)
	c.add(line(lidMain, 5267, probeUID, 1, 4, "AndroidRuntime", "VM exiting with result code 0, cleanup skipped."), time.Time{})
	c.add(event(5, evAmProcDied, 0, 5267, "dev.agemu.probe", 0, 2), time.Time{})
	st := c.appState("dev.agemu.probe", guest0)
	if st.Crash == nil || st.Crash.Kind != api.CrashKindExited || st.Crash.Pid != 5267 {
		t.Fatalf("state %+v", st.Crash)
	}
	if !st.Running {
		t.Error("the :remote process lives on")
	}
	delete(c.w.procs, 5494)
	if c.appState("dev.agemu.probe", guest0).Running {
		t.Error("running with no process left")
	}
}

func TestWatchIgnoresExitsAgemuAskedFor(t *testing.T) {
	c := testCapture(t)
	c.pkgUID["com.example.other"] = 10300
	c.w.procs[6000] = procInfo{uid: 10300, name: "com.example.other"}
	d := &device{logs: c}
	d.expectExit("dev.agemu.probe") // its live processes: 5267 and 5494 (:remote)
	// pm clear or a permission revoke can end them with only a death event.
	c.add(event(10, evAmProcDied, 0, 5267, "dev.agemu.probe", 0, 2), time.Time{})
	c.add(event(11, evAmProcDied, 0, 5494, "dev.agemu.probe:remote", 0, 2), time.Time{})
	if cs := crashOf(c); len(cs) != 0 {
		t.Fatalf("records for exits agemu asked for: %+v", cs)
	}
	// The app started again right after: its own exit a second later is reported.
	c.add(event(500, evAmProcStart, 0, 5600, probeUID, "dev.agemu.probe", "next-top-activity", "{x}"), time.Time{})
	c.add(event(1500, evAmProcDied, 0, 5600, "dev.agemu.probe", 0, 2), time.Time{})
	// Another app was not asked to exit.
	c.add(event(1600, evAmProcDied, 0, 6000, "com.example.other", 0, 2), time.Time{})
	cs := crashOf(c)
	if len(cs) != 2 || cs[0].Pid != 5600 || cs[0].Kind != api.CrashKindExited || cs[1].App != "com.example.other" {
		t.Fatalf("records %+v", cs)
	}
}

func TestWatchKills(t *testing.T) {
	c := testCapture(t)
	// Force-stop (terminate, Settings): am_kill and no death event; not a report.
	c.add(event(1, evAmKill, 0, 5494, "dev.agemu.probe:remote", 0, "stop dev.agemu.probe due to from pid 2185", 80880), time.Time{})
	c.add(event(2, evAmKill, 0, 5267, "dev.agemu.probe", 0, "stop dev.agemu.probe due to from pid 2185", 122384), time.Time{})
	if cs := crashOf(c); len(cs) != 0 {
		t.Fatalf("force-stop recorded: %+v", cs)
	}
	if c.appState("dev.agemu.probe", guest0).Running {
		t.Error("running after force-stop")
	}
	// The system's own kills are reported.
	c.w.procs[6000] = procInfo{uid: probeUID, name: "dev.agemu.probe"}
	c.add(event(10, evAmKill, 0, 6000, "dev.agemu.probe", 900, "excessive cpu 61000 during 300000 dur=300000 limit=25", 1000), time.Time{})
	cs := crashOf(c)
	if len(cs) != 1 || cs[0].Kind != api.CrashKindKilled || cs[0].Exception != "ActivityManager" || !strings.HasPrefix(cs[0].Reason, "excessive cpu") {
		t.Fatalf("excessive cpu: %+v", cs)
	}
	// lmkd's kill, its death event first: the exit becomes the kill, at the death's time.
	c.w.procs[6100] = procInfo{uid: probeUID, name: "dev.agemu.probe"}
	c.add(event(20, evAmProcDied, 0, 6100, "dev.agemu.probe", 900, 19), time.Time{})
	c.add(line(lidMain, 400, 1069, 21, 6, "lowmemorykiller", "Kill 'dev.agemu.probe' (6100), uid 10229, oom_score_adj 900 to free 120000kB rss, 30000kB swap; reason: low watermark is breached"), time.Time{})
	cs = crashOf(c)
	if len(cs) != 2 || cs[1].Kind != api.CrashKindKilled || cs[1].Exception != "low memory killer" || !cs[1].Time.Equal(guest0.Add(20*time.Millisecond+guestOffset)) {
		t.Fatalf("lmk: %+v", cs)
	}
	for _, r := range []string{"empty #17", "cached #33", "remove task", "anr", "crash", "stop com.x due to from pid 1"} {
		if systemKill(r) {
			t.Errorf("%q reported as the system's kill", r)
		}
	}
	for _, r := range []string{"excessive cpu 1 during 2", "freezer binder ioctl", "Unable to freeze binder interface"} {
		if !systemKill(r) {
			t.Errorf("%q not reported", r)
		}
	}
}

// Another app (uid 10300, pid 7000) writes records that claim the probe crashed, was killed or
// exited, or that it started a process. None of them may reach the probe's state: the probe
// keeps running without a crash, and its uid stays the probe's.
func TestOtherAppsCannotForgeTheProbesCrash(t *testing.T) {
	const evilUID, evilPid = 10300, 7000
	c := testCapture(t)
	c.pkgUID["com.evil"] = evilUID
	c.names[evilPid] = "com.evil"
	c.w.procs[evilPid] = procInfo{uid: evilUID, name: "com.evil"}
	forged := []logRecord{
		// (1) AndroidRuntime's report naming the probe's process and pid: in the main buffer, and
		// in the crash buffer (any app can write there).
		line(lidMain, evilPid, evilUID, 1, 6, "AndroidRuntime", javaCrashText),
		line(lidCrash, evilPid, evilUID, 2, 6, "AndroidRuntime", javaCrashText),
		// libc's line naming the probe's pid.
		line(lidCrash, evilPid, evilUID, 3, 7, "libc", "Fatal signal 11 (SIGSEGV), code 1 (SEGV_MAPERR), fault addr 0x0 in tid 5267 (dev.agemu.probe), pid 5267 (dev.agemu.probe)"),
		// (2) lmkd's line from an app.
		line(lidMain, evilPid, evilUID, 4, 6, "lowmemorykiller", "Kill 'dev.agemu.probe' (5267), uid 10229, oom_score_adj 0 to free 1kB rss, 0kB swap; reason: x"),
		// A tombstone (crash_dump's tag) about the probe's pid, from another uid.
		line(lidCrash, evilPid, evilUID, 5, 7, "DEBUG", "pid: 5267, tid: 5267, name: dev.agemu.probe  >>> dev.agemu.probe <<<\nsignal 11 (SIGSEGV), code 1 (SEGV_MAPERR), fault addr 0x0\n"),
	}
	// (3) ActivityManager's events written by the app (EventLog.writeEvent with their tags).
	for _, ev := range []logRecord{
		event(6, evAmProcDied, 0, 5267, "dev.agemu.probe", 0, 2),
		event(7, evAmCrash, 5267, 0, "dev.agemu.probe", 0, "java.lang.Error", "forged", "x", 1, 0),
		event(8, evAmKill, 0, 5267, "dev.agemu.probe", 900, "excessive cpu 1 during 2", 1),
		event(9, evAmANR, 0, 5267, "dev.agemu.probe", 0, "forged"),
		event(10, evAmProcStart, 0, 8000, evilUID, "dev.agemu.probe", "activity", "x"),
	} {
		ev.pid, ev.uid = evilPid, evilUID
		forged = append(forged, ev)
	}
	for _, r := range forged {
		c.add(r, time.Time{})
	}
	st := c.appState("dev.agemu.probe", guest0)
	if st.Crash != nil || !st.Running {
		t.Fatalf("forged records reached the probe's state: %+v", st)
	}
	if c.pkgUID["dev.agemu.probe"] != probeUID {
		t.Errorf("the probe's uid became %d", c.pkgUID["dev.agemu.probe"])
	}
	for _, cr := range crashOf(c) {
		if cr.App == "dev.agemu.probe" {
			t.Errorf("a record of the probe: %+v", cr)
		}
	}
	// The app's own crash (its own pid, a report naming another process) is its own.
	c.add(line(lidCrash, evilPid, evilUID, 20, 7, "libc", "Fatal signal 11 (SIGSEGV), code 1 (SEGV_MAPERR), fault addr 0x0 in tid 7000 (dev.agemu.probe), pid 7000 (dev.agemu.probe)"), time.Time{})
	cs := crashOf(c)
	if len(cs) != 1 || cs[0].App != "com.evil" || cs[0].Pid != evilPid {
		t.Errorf("the writer's own crash: %+v", cs)
	}
	if st := c.appState("dev.agemu.probe", guest0); st.Crash != nil || !st.Running {
		t.Errorf("the writer's own crash reached the probe: %+v", st)
	}
}

func TestWatchSystemAppsAndOtherApps(t *testing.T) {
	c := testCapture(t)
	// A system app's process ending is not an exit report (only user apps get those).
	c.add(event(1, evAmProcDied, 0, 4477, "com.android.vending", 0, 19), time.Time{})
	if cs := crashOf(c); len(cs) != 0 {
		t.Fatalf("system app exit recorded: %+v", cs)
	}
	// A system app's crash is a report, under its own package.
	c.add(line(lidCrash, 4477, gmsUID, 2, 6, "AndroidRuntime", "FATAL EXCEPTION: main\nProcess: com.android.vending, PID: 4477\njava.lang.RuntimeException: x\n\tat a.B.c(B.java:1)"), time.Time{})
	if st := c.appState("dev.agemu.probe", guest0); st.Crash != nil || !st.Running {
		t.Errorf("another app's crash reaches the probe's state: %+v", st)
	}
	if cs := crashOf(c); len(cs) != 1 || cs[0].App != "com.android.vending" {
		t.Errorf("records %+v", cs)
	}
}

func TestWatchRemoteProcessCrash(t *testing.T) {
	c := testCapture(t)
	c.add(line(lidCrash, 5494, probeUID, 1, 6, "AndroidRuntime", remoteCrashText), time.Time{})
	c.add(event(3, evAmCrash, 5494, 0, "dev.agemu.probe:remote", 814235204, "java.lang.IllegalArgumentException", "remote crash #2", "RemoteService.java", 19, 0), time.Time{})
	c.add(event(8, evAmProcDied, 0, 5494, "dev.agemu.probe:remote", 0, 10), time.Time{})
	st := c.appState("dev.agemu.probe", guest0)
	if st.Crash == nil || st.Crash.Process != "dev.agemu.probe:remote" || st.Crash.App != "dev.agemu.probe" || st.Crash.Exception != "java.lang.IllegalArgumentException" {
		t.Fatalf("remote crash %+v", st.Crash)
	}
	if !st.Running {
		t.Error("the main process lives: running")
	}
	if n := len(crashOf(c)); n != 1 {
		t.Errorf("%d records", n)
	}
}

func TestWatchBackfilledProcessEvents(t *testing.T) {
	c := testCapture(t)
	// The launch forked the app just before the process list was read: the list shows a
	// root-owned zygote child, and only the backfilled start event names it.
	c.w.procs = map[int]procInfo{}
	c.add(event(10, evAmProcStart, 0, 7000, probeUID, "dev.agemu.probe", "next-top-activity", "{x}"), time.Time{})
	if !c.appState("dev.agemu.probe", guest0).Running {
		t.Fatal("a process started in the backfill does not count as running")
	}
	// A backfilled death removes its process, and is an exit report (the app's history since
	// the last snapshot load).
	c.add(event(20, evAmProcDied, 0, 7000, "dev.agemu.probe", 0, 2), time.Time{})
	if c.appState("dev.agemu.probe", guest0).Running {
		t.Error("running after the backfilled death")
	}
	if cs := crashOf(c); len(cs) != 1 || cs[0].Kind != api.CrashKindExited {
		t.Errorf("records %+v", cs)
	}
}

func TestWatchKillAfterACrashIsPartOfIt(t *testing.T) {
	c := testCapture(t)
	// The second crash in a minute: the system shows "keeps stopping", the process waits in its
	// crash handler, and is killed later as a cached process.
	c.add(line(lidCrash, 5267, probeUID, 0, 6, "AndroidRuntime", javaCrashText), time.Time{})
	c.add(event(15000, evAmKill, 0, 5267, "dev.agemu.probe", 900, "excessive binder traffic during cached", 1000), time.Time{})
	if cs := crashOf(c); len(cs) != 1 || cs[0].Kind != api.CrashKindCrash {
		t.Fatalf("records %+v", cs)
	}
}

func TestAppStateDoesNotWaitForAStartingCapture(t *testing.T) {
	b := New()
	d := b.dev("agemu-test-none")
	c := newLogCapture(d.name, "emulator-0", time.Time{}, 1) // not ready: still starting
	defer c.cancel()
	d.logs = c
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	t0 := time.Now()
	st, err := b.AppState(ctx, "android:agemu-test-none", "dev.agemu.probe", time.Time{})
	if err != nil || !st.Running || st.Crash != nil {
		t.Fatalf("state %+v %v", st, err)
	}
	// The daemon asks right after the launch that starts the capture: it must not wait for it.
	if d := time.Since(t0); d > 100*time.Millisecond {
		t.Fatalf("took %v", d)
	}
}

func TestCrashRecordsAreBounded(t *testing.T) {
	c := testCapture(t)
	for i := 0; i < maxCrashRecords+20; i++ {
		c.w.procs[8000+i] = procInfo{uid: probeUID, name: "dev.agemu.probe"}
		c.add(event(i*100, evAmProcDied, 0, 8000+i, "dev.agemu.probe", 0, 2), time.Time{})
	}
	cs := crashOf(c)
	if len(cs) != maxCrashRecords || cs[0].Pid != 8020 {
		t.Fatalf("%d records, oldest pid %d", len(cs), cs[0].Pid)
	}
}
