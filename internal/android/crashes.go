package android

// Crash records and the app watcher, from the records the log capture takes (logs.go):
//
//	crash   an uncaught Java/Kotlin exception: AndroidRuntime's "FATAL EXCEPTION" in the crash
//	        buffer (the process logs it before it dies), confirmed by am_crash;
//	        a native crash: libc's "Fatal signal" line (logged by the crashing process at
//	        once), then crash_dump's tombstone lines (tag DEBUG: cause, abort message,
//	        backtrace) and am_crash "Native crash" when the dump is done (0.8 s later here);
//	anr     am_anr; the main thread's stack comes from the dropbox entry when crashes reads it;
//	killed  lmkd's "Kill '<process>'" line, or an am_kill for excessive resource use or by the
//	        freezer;
//	exited  am_proc_died of a user app's process with no crash, ANR or kill before it, and no
//	        exit agemu asked for (terminate, uninstall, clear-data, a permission revoke or
//	        reset): System.exit, a kill -9 from outside.
//
// A process killed by ActivityManager (force-stop, a crash, an ANR's "close app", cache
// trimming) gets am_kill and no am_proc_died; agemu's own exits are explained that way, and are
// also marked (expectExit) for the paths that only log am_proc_died.

import (
	"context"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/api"
)

// ActivityManager's event tags (/system/etc/event-log-tags; stable across releases).
const (
	evAmANR       = 30008
	evAmProcDied  = 30011
	evAmProcStart = 30014
	evAmKill      = 30023
	evAmCrash     = 30039
)

const (
	maxCrashRecords = 200
	// sameCrash: evidence of one crash (the process's own line, the tombstone, am_crash, the
	// process's death) arrives within this window.
	sameCrash = 30 * time.Second
	// expectFor: how long a process marked by expectExit may take to die and still be agemu's.
	expectFor = 30 * time.Second
	maxDump   = 64 << 10 // bytes of one tombstone kept while it is parsed
	maxKilled = 512      // am_kill reasons kept until the process's death event
)

// crashRec is one crash record and what the watcher needs besides the report.
type crashRec struct {
	c     api.Crash
	uid   int
	fatal bool      // the process is going down: it no longer counts as running
	anr   bool      // frames are read from dropbox on demand (crashes)
	tried time.Time // the last look in dropbox
	made  time.Time // when the record was made (the daemon's clock)
}

// tombstone collects crash_dump's lines (tag DEBUG, one logging pid) until they name the
// crashing pid.
type tombstone struct {
	text strings.Builder
	at   time.Time
}

// watch is the crash side of a capture; its fields are guarded by logCapture.mu.
type watch struct {
	// procs are the live app processes: the process list at capture start, then every start and
	// death in the order logd holds them, the backfilled ones too. A process forked just before
	// the list is read can be in it still as a root-owned zygote child, so the start events of
	// the backfill are needed; a death in the backfill of a pid the list holds again would
	// remove a reused pid's process (pids are not reused that quickly here).
	procs   map[int]procInfo
	recs    []*crashRec // oldest first
	dumps   map[int]*tombstone
	killed  map[int]string    // pid -> the am_kill reason, until its death is seen
	expect  map[int]time.Time // pid -> until when its death is one agemu asked for (expectExit)
	dumpPid int               // the pid tombstoned last said it dumps
}

func newWatch() watch {
	return watch{procs: map[int]procInfo{}, dumps: map[int]*tombstone{}, killed: map[int]string{}, expect: map[int]time.Time{}}
}

// ------------------------------------------------------------------ records

// rec returns the newest record of pid (or, with pid 0, of the process name) of one of kinds,
// within sameCrash of t.
func (c *logCapture) rec(pid int, process string, t time.Time, kinds ...api.CrashKind) *crashRec {
	for i := len(c.w.recs) - 1; i >= 0; i-- {
		r := c.w.recs[i]
		if d := t.Sub(r.c.Time); d > sameCrash || d < -sameCrash {
			continue
		}
		if (pid != 0 && r.c.Pid == pid) || (pid == 0 && r.c.Process == process) {
			if slices.Contains(kinds, r.c.Kind) {
				return r
			}
		}
	}
	return nil
}

// newRec adds a record of a process.
func (c *logCapture) newRec(t time.Time, pid, uid int, process string, kind api.CrashKind) *crashRec {
	if process == "" {
		process = c.names[pid]
	}
	if uid < 0 {
		if p, ok := c.w.procs[pid]; ok {
			uid = p.uid
		}
	}
	r := &crashRec{c: api.Crash{Time: t, App: c.appOf(uid, process), Process: process, Pid: pid, Kind: kind}, uid: uid, made: time.Now()}
	c.w.recs = append(c.w.recs, r)
	if len(c.w.recs) > maxCrashRecords {
		c.w.recs = slices.Delete(c.w.recs, 0, len(c.w.recs)-maxCrashRecords)
	}
	if trace {
		logf("%s: %s of %s (%s pid %d) at guest %s", c.name, kind, r.c.App, process, pid, t.Format("15:04:05.000"))
	}
	return r
}

// appOf is the package a process belongs to: its name's package when that package has the
// process's uid (or the uid is unknown), else a package of the uid. For an app's uid, a name
// that is not one of the uid's packages is not believed (an app can name itself anything in a
// record it writes): the record gets a package of its uid, or none when the name's package has
// another uid.
func (c *logCapture) appOf(uid int, process string) string {
	pkg := pkgOf(process)
	u, known := c.pkgUID[pkg]
	if known && (uid < 0 || u == uid) {
		return pkg
	}
	var ofUID []string
	if uid >= 0 {
		for p, pu := range c.pkgUID {
			if pu != uid {
				continue
			}
			if strings.HasPrefix(process, p) {
				return p
			}
			ofUID = append(ofUID, p)
		}
	}
	if isAppUID(uid) && ((known && u >= 0) || len(ofUID) > 0) {
		if len(ofUID) == 0 {
			return ""
		}
		slices.Sort(ofUID)
		return ofUID[0]
	}
	return pkg // the name is all there is
}

// expected reports whether the death of process pid at guest time t is one agemu asked for
// (expectExit).
func (c *logCapture) expected(pid int, t time.Time) bool {
	until, ok := c.w.expect[pid]
	return ok && !c.toMac(t).After(until)
}

// report returns a record as results show it: a copy, its time on the Mac's clock.
func (c *logCapture) report(r *crashRec) api.Crash {
	cr := r.c
	cr.Time = c.toMac(r.c.Time)
	cr.Frames = slices.Clone(r.c.Frames)
	return cr
}

// ------------------------------------------------------------------ writers

// Whose records the watcher believes. Any app can write any event tag (EventLog.writeEvent) and
// any tag and text in the main and crash buffers, but logd stamps each record with the writer's
// pid and uid. So a record about another process counts only from the system: ActivityManager's
// events from system_server (uid 1000), lmkd's and tombstoned's lines from system daemons (uids
// below the first app uid). The crashing process's own lines (AndroidRuntime's FATAL EXCEPTION,
// libc's Fatal signal, both in the crash buffer) count for the process that wrote them only, and
// crash_dump's tombstone (it runs as the crashed app's uid) only for a process of that uid.
const aidSystem = 1000

// systemUID: a record written by the system (not an app). A header without a uid (v3 and older,
// -1) cannot tell and is believed.
func systemUID(uid int) bool { return uid < 0 || uid%100000 < 10000 }

// fromSystemServer: an events record written by system_server.
func fromSystemServer(rec logRecord) bool { return rec.uid < 0 || rec.uid == aidSystem }

// ------------------------------------------------------------------ events

func (c *logCapture) event(rec logRecord, t time.Time) {
	if !fromSystemServer(rec) {
		return // an app's EventLog.writeEvent with one of ActivityManager's tags
	}
	switch rec.event {
	case evAmProcStart: // [user, pid, uid, process, type, component]
		pid, ok1 := eventInt(rec.data, 1)
		uid, ok2 := eventInt(rec.data, 2)
		name, ok3 := eventStr(rec.data, 3)
		if !ok1 || !ok2 || !ok3 {
			return
		}
		c.setName(int(pid), name)
		if isAppUID(int(uid)) && !strings.Contains(name, ":") {
			c.pkgUID[name] = int(uid) // the newest start wins: a reinstalled app has a new uid
		}
		if isAppUID(int(uid)) {
			c.w.procs[int(pid)] = procInfo{uid: int(uid), name: name}
		}
	case evAmProcDied: // [user, pid, process, oom adj, proc state]
		pid, ok1 := eventInt(rec.data, 1)
		name, ok2 := eventStr(rec.data, 2)
		if !ok1 || !ok2 {
			return
		}
		c.died(int(pid), name, t)
	case evAmKill: // [user, pid, process, oom adj, reason, rss]
		pid, ok1 := eventInt(rec.data, 1)
		name, ok2 := eventStr(rec.data, 2)
		reason, _ := eventStr(rec.data, 4)
		if !ok1 || !ok2 {
			return
		}
		delete(c.w.procs, int(pid)) // ActivityManager kills it now and logs no death for it
		if len(c.w.killed) >= maxKilled {
			clear(c.w.killed) // kills whose death was never logged (force-stop, cache trimming)
		}
		c.w.killed[int(pid)] = reason
		// A process that crashed or stopped responding stays alive while the system shows its
		// dialog, and is killed later (seen: "excessive binder traffic during cached" 15 s after
		// a crash): that kill is part of the recorded crash.
		if systemKill(reason) && !c.expected(int(pid), t) &&
			c.rec(int(pid), name, t, api.CrashKindCrash, api.CrashKindANR, api.CrashKindKilled, api.CrashKindExited) == nil {
			r := c.newRec(t, int(pid), -1, name, api.CrashKindKilled)
			r.c.Exception, r.c.Reason, r.fatal = "ActivityManager", reason, true
		}
	case evAmCrash: // [pid, user, process, flags, exception, message, file, line, recoverable]
		pid, _ := eventInt(rec.data, 0)
		name, ok := eventStr(rec.data, 2)
		if !ok {
			return
		}
		exc, _ := eventStr(rec.data, 4)
		msg, _ := eventStr(rec.data, 5)
		// The pid is the reporting process: the app for a Java crash, system_server for a native
		// one (its NativeCrashListener reports what crash_dump sent).
		p := int(pid)
		if c.names[p] != name {
			p = 0
		}
		r := c.rec(p, name, t, api.CrashKindCrash, api.CrashKindExited)
		if r == nil {
			r = c.newRec(t, p, -1, name, api.CrashKindCrash)
		}
		r.c.Kind, r.fatal = api.CrashKindCrash, true
		if r.c.Exception == "" {
			r.c.Exception = exc
		}
		if r.c.Reason == "" {
			r.c.Reason = msg
		}
		if r.c.Report == "" && exc != "Native crash" {
			r.c.Report = "dropbox data_app_crash " + t.In(c.loc).Format("2006-01-02 15:04:05")
		}
	case evAmANR: // [user, pid, package, flags, reason]
		pid, ok1 := eventInt(rec.data, 1)
		pkg, ok2 := eventStr(rec.data, 2)
		reason, _ := eventStr(rec.data, 4)
		if !ok1 || !ok2 {
			return
		}
		name := c.names[int(pid)]
		if name == "" {
			name = pkg
		}
		r := c.newRec(t, int(pid), -1, name, api.CrashKindANR)
		r.c.App, r.c.Exception, r.c.Reason, r.c.Thread, r.anr = pkg, "ANR", reason, "main", true
		r.c.Report = "dropbox data_app_anr " + t.In(c.loc).Format("2006-01-02 15:04:05")
	}
}

// died handles a process death ActivityManager saw (am_proc_died).
func (c *logCapture) died(pid int, name string, t time.Time) {
	p, live := c.w.procs[pid]
	delete(c.w.procs, pid)
	_, killed := c.w.killed[pid]
	delete(c.w.killed, pid)
	uid := -1
	if live {
		uid = p.uid
	} else if u, ok := c.pkgUID[pkgOf(name)]; ok {
		uid = u
	}
	switch {
	case killed, !c.userApp(uid), c.expected(pid, t):
		return
	case c.rec(pid, name, t, api.CrashKindCrash, api.CrashKindANR, api.CrashKindKilled, api.CrashKindExited) != nil:
		return // the crash (or ANR, kill) it died of is recorded
	}
	r := c.newRec(t, pid, uid, name, api.CrashKindExited)
	r.c.Reason, r.fatal = "the process ended without a crash report", true
}

// systemKill: an am_kill reason that is the system's own decision about the app's resources,
// reported as killed. Other reasons are a user's or tool's request (force-stop, remove task),
// cache trimming of a background process, or a crash or ANR recorded on its own.
func systemKill(reason string) bool {
	r := strings.ToLower(reason)
	return strings.HasPrefix(r, "excessive") || strings.Contains(r, "freez") || strings.Contains(r, "low memory")
}

// ------------------------------------------------------------------ text records

var (
	fatalSignalRe = regexp.MustCompile(`^Fatal signal (\d+) \((\w+)\), code (-?\d+) \(([^)]*)\)(?:, fault addr (\S+))? in tid (\d+) \((.*?)\), pid (\d+) \((.*?)\)`)
	lmkRe         = regexp.MustCompile(`^Kill '([^']+)' \((\d+)\), uid (\d+), oom_score_adj (-?\d+)`)
	tombPidRe     = regexp.MustCompile(`(?m)^pid: (\d+), (?:ppid: \d+, )?tid: (\d+), name: (.*?)  >>> (.*?) <<<`)
	tombSignalRe  = regexp.MustCompile(`(?m)^signal (\d+) \((\w+)\), code (-?\d+) \(([^)]*)\)`)
	tombCauseRe   = regexp.MustCompile(`(?m)^Cause: (.*)$`)
	tombAbortRe   = regexp.MustCompile(`(?m)^Abort message: '(.*)'$`)
	tombFrameRe   = regexp.MustCompile(`^\s+#\d+ pc `)
	tombWrittenRe = regexp.MustCompile(`^Tombstone written to: (\S+)`)
	tombReqRe     = regexp.MustCompile(`^received crash request for pid (\d+)`)
)

func (c *logCapture) text(rec logRecord, t time.Time) {
	switch rec.tag {
	case "AndroidRuntime":
		if rec.lid == lidCrash && rec.prio == 6 && strings.HasPrefix(rec.msg, "FATAL EXCEPTION: ") {
			c.javaCrash(rec, t)
		}
	case "libc":
		if rec.lid == lidCrash && rec.prio == 7 && strings.HasPrefix(rec.msg, "Fatal signal ") {
			c.fatalSignal(rec, t)
		}
	case "DEBUG":
		if rec.lid == lidCrash {
			c.tombstoneLine(rec, t)
		}
	case "tombstoned":
		if !systemUID(rec.uid) {
			return
		}
		if m := tombReqRe.FindStringSubmatch(rec.msg); m != nil {
			c.w.dumpPid, _ = strconv.Atoi(m[1])
		} else if m := tombWrittenRe.FindStringSubmatch(rec.msg); m != nil && c.w.dumpPid != 0 {
			if r := c.rec(c.w.dumpPid, "", t, api.CrashKindCrash); r != nil && r.c.Report == "" {
				r.c.Report = "/data/tombstones/" + m[1]
			}
		}
	case "lowmemorykiller":
		if !systemUID(rec.uid) {
			return // lmkd runs as its own system uid; an app's line with this tag is the app's
		}
		if m := lmkRe.FindStringSubmatch(rec.msg); m != nil {
			pid, _ := strconv.Atoi(m[2])
			uid, _ := strconv.Atoi(m[3])
			if c.expected(pid, t) || c.rec(pid, m[1], t, api.CrashKindKilled) != nil {
				return
			}
			r := c.rec(pid, m[1], t, api.CrashKindExited) // its death may have come first
			if r == nil {
				r = c.newRec(t, pid, uid, m[1], api.CrashKindKilled)
			}
			r.c.Kind, r.c.Exception, r.c.Reason, r.fatal = api.CrashKindKilled, "low memory killer", rec.msg, true
		}
	}
}

// javaCrash records an uncaught exception from the process's own report: the process that wrote
// it (its report names its own PID; one naming another process is not believed).
func (c *logCapture) javaCrash(rec logRecord, t time.Time) {
	j := parseJavaCrash(rec.msg)
	pid := rec.pid
	if j.pid != 0 && j.pid != pid {
		return
	}
	process := c.names[pid]
	if process == "" {
		process = j.process
	}
	r := c.rec(pid, "", t, api.CrashKindCrash, api.CrashKindExited)
	if r == nil {
		r = c.newRec(t, pid, rec.uid, process, api.CrashKindCrash)
	}
	r.c.Kind, r.fatal = api.CrashKindCrash, true
	r.c.Exception, r.c.Reason, r.c.Thread, r.c.Frames = j.exception, j.reason, j.thread, j.frames
}

// fatalSignal records a native crash from libc's line in the crashing process (the line names
// the process that wrote it; one naming another process is not believed).
func (c *logCapture) fatalSignal(rec logRecord, t time.Time) {
	m := fatalSignalRe.FindStringSubmatch(rec.msg)
	if m == nil {
		return
	}
	pid, _ := strconv.Atoi(m[8])
	if pid != rec.pid {
		return
	}
	process := c.names[pid]
	if process == "" {
		process = m[9]
	}
	r := c.rec(pid, "", t, api.CrashKindCrash, api.CrashKindExited)
	if r == nil {
		r = c.newRec(t, pid, rec.uid, process, api.CrashKindCrash)
	}
	r.c.Kind, r.fatal = api.CrashKindCrash, true
	if r.c.Exception == "" {
		r.c.Exception = signalName(m[2], m[4])
	}
	if r.c.Reason == "" && m[5] != "" {
		r.c.Reason = "fault addr " + m[5]
	}
	if r.c.Thread == "" {
		r.c.Thread = m[7]
	}
}

// signalName: "SIGSEGV (SEGV_MAPERR)", or the signal alone for codes without a name of their own.
func signalName(sig, code string) string {
	if code == "" || strings.HasPrefix(code, "SI_") {
		return sig
	}
	return sig + " (" + code + ")"
}

// tombstoneLine adds one of crash_dump's lines and updates the crash it is about.
func (c *logCapture) tombstoneLine(rec logRecord, t time.Time) {
	for pid, ts := range c.w.dumps {
		if t.Sub(ts.at) > sameCrash {
			delete(c.w.dumps, pid)
		}
	}
	ts := c.w.dumps[rec.pid]
	if ts == nil {
		ts = &tombstone{at: t}
		c.w.dumps[rec.pid] = ts
	}
	if ts.text.Len() < maxDump {
		// One record per chunk of lines; a blank line inside the text ends the backtrace.
		ts.text.WriteString(strings.TrimRight(rec.msg, "\n"))
		ts.text.WriteByte('\n')
	}
	tb := parseTombstone(ts.text.String())
	if tb.pid == 0 {
		return
	}
	// crash_dump runs as the crashed process's uid: a tombstone about a process of another uid
	// was written by someone else.
	if p, ok := c.w.procs[tb.pid]; ok && rec.uid >= 0 && p.uid != rec.uid {
		return
	}
	r := c.rec(tb.pid, "", t, api.CrashKindCrash, api.CrashKindExited)
	if r != nil && r.uid >= 0 && rec.uid >= 0 && r.uid != rec.uid {
		return
	}
	if r == nil {
		process := c.names[tb.pid]
		if process == "" {
			process = tb.process
		}
		r = c.newRec(t, tb.pid, rec.uid, process, api.CrashKindCrash)
	}
	r.c.Kind, r.fatal = api.CrashKindCrash, true
	if tb.signal != "" && (r.c.Exception == "" || !strings.HasPrefix(r.c.Exception, "SIG")) {
		r.c.Exception = tb.signal
	}
	if tb.reason != "" {
		r.c.Reason = tb.reason
	}
	if r.c.Thread == "" {
		r.c.Thread = tb.thread
	}
	if len(tb.frames) > len(r.c.Frames) {
		r.c.Frames = tb.frames
	}
}

// javaReport is what parseJavaCrash finds in AndroidRuntime's report.
type javaReport struct {
	thread, process   string
	pid               int
	exception, reason string
	frames            []string
}

// parseJavaCrash reads AndroidRuntime's uncaught-exception report:
//
//	FATAL EXCEPTION: main
//	Process: com.example.app, PID: 4242
//	java.lang.RuntimeException: Unable to start service ...
//		at android.app.ActivityThread.handleServiceArgs(ActivityThread.java:5845)
//		...
//	Caused by: java.lang.IllegalArgumentException: remote crash #2
//		at com.example.app.RemoteService.onStartCommand(RemoteService.java:19)
//		... 10 more
//
// The exception and reason are the root cause's, as ActivityManager reports them (am_crash);
// the frames are the root cause's stack, completed with the frames "... N more" shares with
// the exception that wraps it, so they are the crashing thread's stack where the root cause was
// thrown, innermost first.
func parseJavaCrash(msg string) javaReport {
	var j javaReport
	type block struct {
		head   []string
		frames []string
		more   int
	}
	var blocks []*block
	var cur *block
	suppressed := false
	for i, line := range strings.Split(msg, "\n") {
		switch {
		case i == 0:
			j.thread = strings.TrimPrefix(line, "FATAL EXCEPTION: ")
			continue
		case strings.HasPrefix(line, "Process: ") && cur == nil:
			p, pid, _ := strings.Cut(strings.TrimPrefix(line, "Process: "), ", PID: ")
			j.process = p
			j.pid, _ = strconv.Atoi(strings.TrimSpace(pid))
			continue
		}
		trimmed := strings.TrimLeft(line, " \t")
		indent := len(line) - len(trimmed)
		switch {
		case strings.HasPrefix(trimmed, "Suppressed: "):
			suppressed = true
		case strings.HasPrefix(line, "Caused by: "):
			suppressed = false
			cur = &block{head: []string{strings.TrimPrefix(line, "Caused by: ")}}
			blocks = append(blocks, cur)
		case suppressed && indent > 1:
		case strings.HasPrefix(trimmed, "at ") && cur != nil:
			suppressed = false
			cur.frames = append(cur.frames, trimmed)
		case strings.HasPrefix(trimmed, "... ") && strings.HasSuffix(trimmed, " more") && cur != nil:
			cur.more, _ = strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(trimmed, "... "), " more"))
		case cur == nil:
			cur = &block{head: []string{line}}
			blocks = append(blocks, cur)
		case len(cur.frames) == 0:
			cur.head = append(cur.head, line) // a message of several lines
		}
	}
	if len(blocks) == 0 {
		return j
	}
	root := blocks[len(blocks)-1]
	exc, reason, _ := strings.Cut(strings.Join(root.head, "\n"), ": ")
	j.exception, j.reason = strings.TrimSpace(exc), strings.TrimSpace(reason)
	// Complete each cause's frames from the exception that wraps it ("... N more").
	full := blocks[0].frames
	for _, b := range blocks[1:] {
		f := append([]string(nil), b.frames...)
		if b.more > 0 && b.more <= len(full) {
			f = append(f, full[len(full)-b.more:]...)
		}
		full = f
	}
	if len(full) > api.MaxCrashFrames {
		full = full[:api.MaxCrashFrames]
	}
	j.frames = full
	return j
}

// tombstoneReport is what parseTombstone finds in crash_dump's lines.
type tombstoneReport struct {
	pid             int
	thread, process string
	signal, reason  string
	frames          []string
}

// parseTombstone reads the tombstone crash_dump logs (tag DEBUG): the crashing pid and thread,
// the signal, the cause or abort message, and the crashing thread's backtrace.
func parseTombstone(text string) tombstoneReport {
	var tb tombstoneReport
	if m := tombPidRe.FindStringSubmatch(text); m != nil {
		tb.pid, _ = strconv.Atoi(m[1])
		tb.thread, tb.process = m[3], m[4]
	}
	if m := tombSignalRe.FindStringSubmatch(text); m != nil {
		tb.signal = signalName(m[2], m[4])
	}
	if m := tombAbortRe.FindStringSubmatch(text); m != nil {
		tb.reason = m[1]
	} else if m := tombCauseRe.FindStringSubmatch(text); m != nil {
		tb.reason = m[1]
	}
	_, bt, ok := strings.Cut(text, "\nbacktrace:\n")
	if !ok {
		return tb
	}
	for _, line := range strings.Split(bt, "\n") {
		if !tombFrameRe.MatchString(line) {
			if strings.TrimSpace(line) == "" && len(tb.frames) == 0 {
				continue
			}
			break
		}
		if len(tb.frames) < api.MaxCrashFrames {
			tb.frames = append(tb.frames, strings.TrimSpace(line))
		}
	}
	return tb
}

// ------------------------------------------------------------------ agemu's own exits

// expectExit marks pkg's live processes as ending at agemu's request (call it before the request:
// terminate, uninstall, clear-data, a permission revoke or reset). Their deaths in the next
// expectFor are not reports; a process the app starts later is reported as usual. (A window of
// time instead hid a real System.exit of the app relaunched a second after a terminate.)
func (d *device) expectExit(pkg string) {
	d.mu.Lock()
	c := d.logs
	d.mu.Unlock()
	if c == nil || pkg == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for pid, until := range c.w.expect {
		if now.After(until) {
			delete(c.w.expect, pid)
		}
	}
	uid, haveUID := c.pkgUID[pkg]
	haveUID = haveUID && uid >= 0
	for pid, p := range c.w.procs {
		if belongs(pkg, uid, haveUID, p.uid, p.name) {
			c.w.expect[pid] = now.Add(expectFor)
		}
	}
}

// ------------------------------------------------------------------ backend interfaces

// Crashes returns the crash records of q.App (or every app) at or after q.Since, oldest first.
// An ANR's main-thread stack is read from its dropbox entry on the first call that finds the
// entry written.
func (b *Backend) Crashes(ctx context.Context, id string, q api.CrashQuery) ([]api.Crash, error) {
	d, err := b.devFor(id)
	if err != nil {
		return nil, err
	}
	c, err := d.capture(ctx)
	if err != nil {
		return nil, err
	}
	c.anrFrames(ctx, q)
	c.mu.Lock()
	defer c.mu.Unlock()
	out := []api.Crash{}
	for _, r := range c.w.recs {
		cr := c.report(r)
		if (q.App != "" && cr.App != q.App) || (!q.Since.IsZero() && cr.Time.Before(q.Since)) {
			continue
		}
		out = append(out, cr)
	}
	return out, nil
}

// AppState answers from memory: whether app has a live process that is not going down, and its
// newest crash record at or after since. Without a capture, or while one is starting, the app
// is reported running and without a crash.
func (b *Backend) AppState(ctx context.Context, id, app string, since time.Time) (api.AppState, error) {
	d, err := b.devFor(id)
	if err != nil {
		return api.AppState{}, err
	}
	d.mu.Lock()
	c := d.logs
	d.mu.Unlock()
	if c == nil {
		if trace {
			logf("%s: app state of %s: no capture", d.name, app)
		}
		return api.AppState{Running: true}, nil
	}
	// A starting capture is not waited for: the daemon asks right after the launch that starts
	// it, and waiting there (up to its 250 ms bound) slowed every first launch by as much. A
	// crash during the start is in the next call's answer.
	select {
	case <-c.ready:
	default:
		if trace {
			logf("%s: app state of %s: the capture is still starting", d.name, app)
		}
		return api.AppState{Running: true}, nil
	}
	st := c.appState(app, since)
	if trace {
		logf("%s: app state of %s since %s: running %v, crash %v", d.name, app, since.Format("15:04:05.000"), st.Running, st.Crash != nil)
	}
	return st, nil
}

func (c *logCapture) appState(app string, since time.Time) api.AppState {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return api.AppState{Running: true}
	}
	uid, haveUID := c.pkgUID[app]
	haveUID = haveUID && uid >= 0
	fatal := map[int]bool{}
	var newest *crashRec
	gsince := c.toGuest(since)
	for _, r := range c.w.recs {
		if r.c.App != app {
			continue
		}
		if r.fatal {
			fatal[r.c.Pid] = true
		}
		if !r.c.Time.Before(gsince) && (newest == nil || !r.c.Time.Before(newest.c.Time)) {
			newest = r
		}
	}
	st := api.AppState{}
	for pid, p := range c.w.procs {
		if belongs(app, uid, haveUID, p.uid, p.name) && !fatal[pid] {
			st.Running = true
			break
		}
	}
	if newest != nil {
		cr := c.report(newest)
		st.Crash = &cr
	}
	return st
}

// How crashes looks for an ANR's dropbox entry: the system writes it a moment after am_anr, so
// the first look often comes too early. A look is repeated no sooner than anrRetry after the
// previous one, and not for an ANR older than anrLookFor (Android keeps at most 6 ANR entries
// per process in 10 minutes, so some never get one).
const (
	anrRetry   = 150 * time.Millisecond
	anrLookFor = 30 * time.Second
)

// anrFrames fills the main-thread stack of ANR records from their dropbox entries (see above).
func (c *logCapture) anrFrames(ctx context.Context, q api.CrashQuery) {
	c.mu.Lock()
	var want []*crashRec
	for _, r := range c.w.recs {
		if r.anr && len(r.c.Frames) == 0 && time.Since(r.tried) > anrRetry && time.Since(r.made) < anrLookFor &&
			(q.App == "" || r.c.App == q.App) && (q.Since.IsZero() || !c.toMac(r.c.Time).Before(q.Since)) {
			want = append(want, r)
		}
	}
	c.mu.Unlock()
	if len(want) == 0 {
		return
	}
	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := c.shell(dctx, "dumpsys dropbox --print data_app_anr")
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range want {
		r.tried = time.Now()
	}
	if err != nil {
		logf("%s: reading ANR traces from dropbox: %v", c.name, err)
		return
	}
	entries := parseANRDropbox(out)
	for _, r := range want {
		if e, ok := entries[r.c.Pid]; ok {
			r.c.Frames, r.anr = e.frames, false
			if e.file != "" {
				r.c.Report = e.file
			}
		}
	}
}

// anrEntry is a dropbox data_app_anr entry's main-thread stack and trace file.
type anrEntry struct {
	frames []string
	file   string
}

// parseANRDropbox reads `dumpsys dropbox --print data_app_anr`: per entry its PID, the "Data
// File" (the full traces on the device) and the stack of the "main" thread, innermost first.
// A later entry for the same pid replaces an earlier one.
func parseANRDropbox(out string) map[int]anrEntry {
	m := map[int]anrEntry{}
	for _, entry := range strings.Split(out, "\n========================================\n")[1:] {
		var e anrEntry
		pid := 0
		inMain := false
		for _, line := range strings.Split(entry, "\n") {
			switch {
			case pid == 0 && strings.HasPrefix(line, "PID: "):
				pid, _ = strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "PID: ")))
			case strings.HasPrefix(line, "Data File: "):
				e.file = strings.TrimSpace(strings.TrimPrefix(line, "Data File: "))
			case strings.HasPrefix(line, `"main" `):
				inMain = len(e.frames) == 0
			case inMain && line == "":
				inMain = false
			case inMain:
				t := strings.TrimSpace(line)
				if (strings.HasPrefix(t, "at ") || strings.HasPrefix(t, "native: ")) && len(e.frames) < api.MaxCrashFrames {
					e.frames = append(e.frames, t)
				}
			}
		}
		if pid != 0 && len(e.frames) > 0 {
			m[pid] = e
		}
	}
	return m
}
