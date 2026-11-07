package ios

// Crashes and the crash notice (docs/CAPABILITIES.md "Logs and crashes", docs/IOS.md "Logs and
// crashes").
//
// A Simulator app is a Mac process, so the daemon learns that it ended within milliseconds:
//   - kqueue (EVFILT_PROC, NOTE_EXIT) on the pid agemu's launch returned (procwatch_*.go);
//   - runningboardd's line in the device's log stream says how: "exited with context
//     <RBSProcessExitContext| specific, status:<RBSProcessExitStatus| domain:signal(2)
//     code:SIGSEGV(11)>>", "voluntary" for exit(), a FrontBoard force quit for a termination
//     the Simulator's host asked for (agemu's terminate, simctl terminate);
//   - the app's own last line names an uncaught exception ("*** Terminating app due to
//     uncaught exception 'X', reason: 'Y'" with the first throw call stack, from CoreFoundation)
//     or a Swift runtime error ("App.swift:12: Fatal error: ..." from libswiftCore).
// The Mac's ReportCrash writes the full report (.ips, with every thread's symbolicated frames)
// to ~/Library/Logs/DiagnosticReports later: seconds, and over two minutes on a loaded Mac.
// agemu reads only the reports whose process ran in this device, and never deletes them.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/api"
)

const (
	// expectSlack: an exit of an app's process from the start of a call that ends it
	// (terminate, a relaunch, uninstall, clear-data, a permission change, reset) until this
	// long after the call returned is agemu's, not a report.
	expectSlack = 5 * time.Second
	// detailWait bounds how long AppState waits for runningboardd's exit line right after an
	// exit, so the first notice says how the app ended; detailWindow is how soon after the exit.
	detailWait   = 150 * time.Millisecond
	detailWindow = 400 * time.Millisecond
	// reportScanEvery spaces the looks for a crash's .ips while the notice is asked for.
	reportScanEvery = 500 * time.Millisecond
	// reportPending: how long after a crash the watcher still looks for its .ips.
	reportPending = 10 * time.Minute
	// maxProcs bounds the process records per device (the oldest ended ones go first).
	maxProcs = 256
)

// appProc is one process of an app: launched through agemu (its exit seen by kqueue), or named
// in runningboardd's lines in the log stream.
type appProc struct {
	pid      int
	app      string    // bundle id ("" until known)
	process  string    // executable name ("" until known)
	launched time.Time // when agemu launched it (zero: seen in the log)
	exited   time.Time // when it ended (zero: running, or the end was not seen yet)
	// The exit as runningboardd reports it: "voluntary", or the domain and code
	// (signal, SIGSEGV(11); frontboard, force-quit(0xfbfbfbfb)).
	exitContext  string
	domain, code string
	// The app's own last words: an uncaught exception or a Swift runtime error.
	exception, reason string
	frames            []string
	report            *ipsReport // the .ips once found
}

// exitExpectation is a call of agemu's that ends app's processes (every app's when app is ""):
// their exits from from to until are not reports, except keep's (the process a relaunch
// started), those of processes agemu launched after the call began (an app launched right
// after an install that crashes at once is reported), and any with crash evidence
// (crashEvidence). until is zero while the call runs.
type exitExpectation struct {
	app         string
	from, until time.Time
	keep        int
}

func (e *exitExpectation) covers(p *appProc) bool {
	if p.pid == e.keep || p.launched.After(e.from) {
		return false
	}
	return (e.app == "" || e.app == p.app) && !p.exited.Before(e.from) &&
		(e.until.IsZero() || !p.exited.After(e.until))
}

// requested reports whether an exit was asked for, by someone, not the app's fault: a
// FrontBoard force quit (agemu's terminate and relaunch, simctl terminate, a user quitting the
// app in the switcher) or a privacy permission change (TCC ends the app, as Settings does).
func (p *appProc) requested() bool {
	return (p.domain == "frontboard" && strings.HasPrefix(p.code, "force-quit")) || p.domain == "tcc"
}

// crashEvidence reports whether p's end shows a crash: a signal other than SIGKILL or SIGTERM,
// the app's own last words (an uncaught exception, a Swift runtime error), or a crash report
// (.ips) of the process. The exits agemu's calls were seen to cause do not look like that (a
// terminate or relaunch is a FrontBoard force quit, an install over a running app runningboard
// code 0, a permission change tcc), so such an end is reported even inside an expectation's
// window: an app that crashes while agemu changes a permission, or right after an install, has
// crashed.
func (p *appProc) crashEvidence() bool {
	if p.exception != "" || p.report != nil {
		return true
	}
	if p.domain != "signal" {
		return false
	}
	sig := signalName(p.code)
	return sig != "" && sig != "SIGKILL" && sig != "SIGTERM"
}

// expectedLocked reports whether p's exit is not a report: one someone asked for, or one
// agemu's call caused (without crash evidence). An exit seen by kqueue before the stream says
// how it ended counts as agemu's until the details arrive; a crash then shows on a later call.
func (dl *deviceLogs) expectedLocked(p *appProc) bool {
	if p.requested() {
		return true
	}
	if p.crashEvidence() {
		return false
	}
	for i := range dl.expect {
		if dl.expect[i].covers(p) {
			return true
		}
	}
	return false
}

// exitDetailKnown: the log stream said how the process ended.
func (p *appProc) exitDetailKnown() bool { return p.exitContext != "" || p.exception != "" }

// signalName is "SIGSEGV" from "SIGSEGV(11)".
func signalName(code string) string {
	name, _, _ := strings.Cut(code, "(")
	return name
}

// crash describes the process's end for the API (nil while it runs). The caller has checked
// that the exit is a report (expectedLocked).
func (p *appProc) crash() *api.Crash {
	if p.exited.IsZero() {
		return nil
	}
	c := &api.Crash{Time: p.exited, App: p.app, Process: p.process, Pid: p.pid, Kind: api.CrashKindExited}
	switch {
	case p.domain == "signal":
		sig := signalName(p.code)
		c.Exception = sig
		if sig == "SIGKILL" || sig == "SIGTERM" {
			c.Kind = api.CrashKindKilled
			c.Reason = "ended by " + sig + " from outside the app"
		} else {
			c.Kind = api.CrashKindCrash
		}
	case p.domain == "frontboard" && strings.Contains(strings.ToLower(p.code), "8badf00d"):
		c.Kind, c.Exception, c.Reason = api.CrashKindKilled, "watchdog (0x8badf00d)", "FrontBoard's watchdog ended it"
	case p.domain != "":
		c.Kind, c.Exception = api.CrashKindKilled, p.domain+" "+p.code
	case p.exitContext == "voluntary":
		c.Reason = "the process exited on its own (exit() or a return from main)"
	}
	if p.exception != "" {
		c.Kind, c.Exception, c.Reason, c.Frames = api.CrashKindCrash, p.exception, p.reason, p.frames
	}
	if r := p.report; r != nil {
		if c.Process == "" {
			c.Process = r.process
		}
		if r.kind != "" && c.Kind != api.CrashKindCrash {
			c.Kind = r.kind
		}
		if p.exception == "" {
			c.Exception = r.exception
			if r.reason != "" {
				c.Reason = r.reason
			}
		}
		if len(r.frames) > 0 {
			c.Frames = r.frames
		}
		c.Thread = r.thread
		c.Report = r.path
	}
	if len(c.Frames) > api.MaxCrashFrames {
		c.Frames = c.Frames[:api.MaxCrashFrames]
	}
	return c
}

// procLocked returns the record of pid, creating it.
func (dl *deviceLogs) procLocked(pid int) *appProc {
	p := dl.procs[pid]
	if p == nil {
		p = &appProc{pid: pid}
		dl.procs[pid] = p
		dl.pruneLocked()
	}
	return p
}

// pruneLocked keeps at most maxProcs records, dropping the oldest ended ones.
func (dl *deviceLogs) pruneLocked() {
	if len(dl.procs) <= maxProcs {
		return
	}
	var ended []*appProc
	for _, p := range dl.procs {
		if !p.exited.IsZero() {
			ended = append(ended, p)
		}
	}
	sort.Slice(ended, func(i, j int) bool { return ended[i].exited.Before(ended[j].exited) })
	for _, p := range ended {
		if len(dl.procs) <= maxProcs {
			return
		}
		delete(dl.procs, p.pid)
	}
}

var (
	// runningboardd names an app's process "[app<com.example.app((null))>:1234]".
	rbAppProc = regexp.MustCompile(`\[app<([^(>]+)(?:\([^>]*\))?>:(\d+)\]`)
	// ... and its end: "... that process [app<...>:1234] exited with context <RBSProcessExitContext| voluntary>".
	rbExit      = regexp.MustCompile(`that process \[app<([^(>]+)(?:\([^>]*\))?>:(\d+)\] exited with context <RBSProcessExitContext\| (.*)>\s*$`)
	rbExitState = regexp.MustCompile(`domain:([\w-]+)\(\d+\) code:([^>]+?)>`)
	// CoreFoundation's last words for an uncaught NSException.
	nsUncaught = regexp.MustCompile(`(?s)^\*\*\* Terminating app due to uncaught exception '([^']*)', reason: '(.*?)'(?:\n\*\*\* First throw call stack:\n\((.*))?$`)
	// A frame of the first throw call stack: "3   AgemuTestApp   0x0000000102 symbol + 832".
	nsFrame = regexp.MustCompile(`^\s*\d+\s+(\S+)\s+0x[0-9a-fA-F]+\s+(.+?)\s*$`)
	// The Swift runtime's: "App/File.swift:12: Fatal error: message" (also Precondition failed).
	swiftFatal = regexp.MustCompile(`^(?:(.+?:\d+): )?(Fatal error|Precondition failed|Assertion failed)(?:: (.*))?$`)
)

// parseExitContext reads runningboardd's exit line: the app's pid and bundle, and how it ended.
func parseExitContext(msg string) (pid int, app, context, domain, code string, ok bool) {
	m := rbExit.FindStringSubmatch(msg)
	if m == nil {
		return 0, "", "", "", "", false
	}
	pid, _ = strconv.Atoi(m[2])
	context = strings.TrimSpace(m[3])
	if s := rbExitState.FindStringSubmatch(context); s != nil {
		domain, code = s[1], strings.TrimSpace(s[2])
	}
	return pid, m[1], context, domain, code, true
}

// parseUncaught reads CoreFoundation's uncaught-exception line: the exception's name, its
// reason and the first throw call stack ("<image> <symbol> + <offset>", innermost first). The
// unified log cuts long messages, so the stack can end early.
func parseUncaught(msg string) (exception, reason string, frames []string, ok bool) {
	m := nsUncaught.FindStringSubmatch(msg)
	if m == nil {
		return "", "", nil, false
	}
	for _, line := range strings.Split(m[3], "\n") {
		if f := nsFrame.FindStringSubmatch(line); f != nil && !strings.HasSuffix(f[2], "<…>") {
			frames = append(frames, f[1]+" "+f[2])
		}
	}
	return m[1], m[2], frames, true
}

// parseSwiftFatal reads the Swift runtime's message for a runtime error: "Fatal error" and the
// message, with the source location after it.
func parseSwiftFatal(msg string) (exception, reason string, ok bool) {
	m := swiftFatal.FindStringSubmatch(strings.TrimSpace(msg))
	if m == nil {
		return "", "", false
	}
	reason = m[3]
	if m[1] != "" {
		if reason != "" {
			reason += " "
		}
		reason += "(" + m[1] + ")"
	}
	return m[2], reason, true
}

// observeLocked takes what the crash watcher needs from a stream line: which app a pid is
// (runningboardd), how it ended (runningboardd), and the app's own last words.
func (dl *deviceLogs) observeLocked(ev *streamEvent, rec *logRecord, t time.Time) {
	msg := ev.EventMessage
	switch {
	case rec.entry.Process == "runningboardd" && rec.bundle == "" && strings.Contains(msg, "[app<"):
		// rec.bundle is "" for the system's runningboardd: a user app whose executable is named
		// runningboardd (its path is in an app bundle) cannot speak for other apps' processes.
		if pid, app, ctx, domain, code, ok := parseExitContext(msg); ok {
			p := dl.procLocked(pid)
			p.app = app
			if p.exited.IsZero() {
				p.exited = t
			}
			if p.exitContext == "" {
				p.exitContext, p.domain, p.code = ctx, domain, code
			}
			dl.notifyLocked()
			return
		}
		// Any other line naming an app's process tells which app the pid is.
		if m := rbAppProc.FindStringSubmatch(msg); m != nil {
			if pid, err := strconv.Atoi(m[2]); err == nil {
				if p := dl.procLocked(pid); p.app == "" {
					p.app = m[1]
				}
			}
		}
	case rec.bundle != "" && ev.ProcessID > 0:
		sender := path.Base(ev.SenderImagePath)
		var exc, reason string
		var frames []string
		ok := false
		switch sender {
		case "CoreFoundation":
			exc, reason, frames, ok = parseUncaught(msg)
		case "libswiftCore.dylib":
			exc, reason, ok = parseSwiftFatal(msg)
		}
		if !ok {
			return
		}
		p := dl.procLocked(ev.ProcessID)
		p.process = rec.entry.Process
		if p.app == "" {
			p.app = dl.bundles[rec.bundle]
		}
		p.exception, p.reason, p.frames = exc, reason, frames
		dl.notifyLocked()
	}
}

// notifyLocked wakes AppState callers waiting for an exit's details.
func (dl *deviceLogs) notifyLocked() {
	if dl.changed != nil {
		close(dl.changed)
		dl.changed = nil
	}
}

// changedLocked returns a channel closed at the next detail the stream adds.
func (dl *deviceLogs) changedLocked() chan struct{} {
	if dl.changed == nil {
		dl.changed = make(chan struct{})
	}
	return dl.changed
}

// watchLaunch records the process agemu launched and watches for its end.
func (b *Backend) watchLaunch(d simDevice, app string, pid int, at time.Time) {
	if pid <= 0 {
		return
	}
	dl := b.devLogs(d.UDID)
	dl.mu.Lock()
	p := dl.procLocked(pid)
	p.app, p.launched = app, at
	dl.mu.Unlock()
	w, err := b.procWatcher()
	if err == nil {
		err = w.add(pid)
	}
	if err != nil {
		// Gone already (it crashed during the launch), or no watcher: AppState checks the pid.
		if !alive(pid) {
			b.procExited(pid, time.Now())
		}
	}
}

// procWatcher returns the backend's exit watcher, starting it.
func (b *Backend) procWatcher() (*procWatcher, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, errors.New("the iOS backend is shutting down")
	}
	if b.pw != nil {
		return b.pw, nil
	}
	w, err := newProcWatcher(b.procExited)
	if err != nil {
		return nil, err
	}
	b.pw = w
	return w, nil
}

// procExited records that pid ended at t (kqueue, or a pid found gone).
func (b *Backend) procExited(pid int, t time.Time) {
	b.mu.Lock()
	devs := make([]*deviceLogs, 0, len(b.logs))
	for _, dl := range b.logs {
		devs = append(devs, dl)
	}
	b.mu.Unlock()
	for _, dl := range devs {
		dl.mu.Lock()
		if p := dl.procs[pid]; p != nil && p.exited.IsZero() {
			p.exited = t
			dl.notifyLocked()
		}
		dl.mu.Unlock()
	}
}

// alive reports whether pid is a live process (it may belong to someone else: kill(pid, 0)
// answers EPERM then).
func alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// expectExits records that a call of agemu's is about to end app's processes (every app's when
// app is ""): terminate, a relaunch, uninstall, clear-data, a permission change, reset. Their
// exits from now until 5 s after done is called are not reports; keep names a process that is
// not part of it (the one a relaunch started), 0 for none.
func (b *Backend) expectExits(udid, app string) (done func(keep int)) {
	dl := b.devLogs(udid)
	dl.mu.Lock()
	defer dl.mu.Unlock()
	now := time.Now()
	// Drop the expectations that ended long ago.
	kept := dl.expect[:0]
	for _, e := range dl.expect {
		if e.until.IsZero() || now.Sub(e.until) < time.Hour {
			kept = append(kept, e)
		}
	}
	dl.expect = append(kept, exitExpectation{app: app, from: now})
	e := &dl.expect[len(dl.expect)-1]
	from := e.from
	return func(keep int) {
		dl.mu.Lock()
		defer dl.mu.Unlock()
		for i := range dl.expect {
			if x := &dl.expect[i]; x.app == app && x.from.Equal(from) && x.until.IsZero() {
				x.until, x.keep = time.Now().Add(expectSlack), keep
			}
		}
	}
}

// AppState reports whether app has a live process and its newest crash, system kill or
// unexpected exit at or after since, from memory: no device round trip. Right after an exit
// it waits up to 150 ms for runningboardd's line, so the first notice says how the app ended.
func (b *Backend) AppState(ctx context.Context, id, app string, since time.Time) (api.AppState, error) {
	udid, err := udidOf(id)
	if err != nil {
		return api.AppState{}, err
	}
	b.mu.Lock()
	dl := b.logs[udid]
	b.mu.Unlock()
	if dl == nil {
		return api.AppState{}, nil
	}
	deadline := time.Now().Add(detailWait)
	if cd, ok := ctx.Deadline(); ok && cd.Before(deadline) {
		deadline = cd
	}
	for {
		dl.mu.Lock()
		st, newest := dl.appStateLocked(app, since)
		var wait chan struct{}
		if newest != nil && !newest.exitDetailKnown() && time.Since(newest.exited) < detailWindow && dl.stream != nil {
			wait = dl.changedLocked()
		}
		pending := newest != nil && newest.report == nil && st.Crash != nil && st.Crash.Kind != api.CrashKindExited &&
			time.Since(newest.exited) < reportPending
		dl.mu.Unlock()
		if pending {
			b.scanReportsAsync()
		}
		if wait == nil || !time.Now().Before(deadline) {
			return st, nil
		}
		t := time.NewTimer(time.Until(deadline))
		select {
		case <-wait:
		case <-t.C:
		case <-ctx.Done():
		}
		t.Stop()
	}
}

// appStateLocked answers AppState from the records; newest is the process of the crash.
func (dl *deviceLogs) appStateLocked(app string, since time.Time) (api.AppState, *appProc) {
	var st api.AppState
	var newest *appProc
	for _, p := range dl.procs {
		if p.app != app {
			continue
		}
		if p.exited.IsZero() {
			if alive(p.pid) {
				st.Running = true
				continue
			}
			if p.launched.IsZero() {
				continue // seen in the log only: its end comes from runningboardd's line
			}
			p.exited = time.Now() // launched by agemu, its end not seen (no watcher): noticed now
		}
		if p.exited.Before(since) || dl.expectedLocked(p) {
			continue
		}
		if newest == nil || p.exited.After(newest.exited) {
			newest = p
		}
	}
	if newest != nil {
		st.Crash = newest.crash()
	}
	return st, newest
}

// Crashes returns the crash reports of q.App (every app when ""), at or after q.Since, oldest
// first: the ends the watcher saw (with the app's last words, then the .ips once written) and
// the device's .ips reports on the Mac. Only agemu's simulators are read (target), like every
// other call: the Mac's reports of other simulators are not listed.
func (b *Backend) Crashes(ctx context.Context, id string, q api.CrashQuery) ([]api.Crash, error) {
	d, err := b.target(ctx, id)
	if err != nil {
		return nil, err
	}
	return b.crashes(d.UDID, q)
}

// crashes is Crashes for a resolved device.
func (b *Backend) crashes(udid string, q api.CrashQuery) ([]api.Crash, error) {
	reports, err := b.reports.scan(q.Since)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	dl := b.logs[udid]
	b.mu.Unlock()
	var out []api.Crash
	matched := map[string]bool{}
	if dl != nil {
		dl.mu.Lock()
		for _, p := range dl.procs {
			if p.report == nil && !p.exited.IsZero() {
				p.report = findReport(reports, udid, p.pid)
			}
			if p.report != nil {
				matched[p.report.path] = true
			}
			if p.exited.IsZero() || dl.expectedLocked(p) {
				continue
			}
			if c := p.crash(); c != nil && (q.App == "" || c.App == q.App) && !c.Time.Before(q.Since) {
				out = append(out, *c)
			}
		}
		dl.mu.Unlock()
	}
	for _, r := range reports {
		if r.udid != udid || matched[r.path] || (q.App != "" && r.app != q.App) || r.time.Before(q.Since) {
			continue
		}
		out = append(out, r.crash())
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out, nil
}

func findReport(reports []*ipsReport, udid string, pid int) *ipsReport {
	for _, r := range reports {
		if r.udid == udid && r.pid == pid {
			return r
		}
	}
	return nil
}

// scanReportsAsync looks for new .ips reports in the background (at most every 500 ms) and
// attaches them to the processes they describe.
func (b *Backend) scanReportsAsync() {
	if !b.reports.due(reportScanEvery) {
		return
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.bgWG.Add(1)
	b.mu.Unlock()
	go func() {
		defer b.bgWG.Done()
		defer b.reports.done()
		reports, err := b.reports.scan(time.Now().Add(-reportPending))
		if err != nil {
			return
		}
		b.mu.Lock()
		devs := make([]*deviceLogs, 0, len(b.logs))
		for _, dl := range b.logs {
			devs = append(devs, dl)
		}
		b.mu.Unlock()
		for _, dl := range devs {
			dl.mu.Lock()
			for _, p := range dl.procs {
				if p.report == nil && !p.exited.IsZero() {
					p.report = findReport(reports, dl.udid, p.pid)
				}
			}
			dl.mu.Unlock()
		}
	}()
}

// ipsReport is what agemu reads from a crash report (.ips) of a Simulator process.
type ipsReport struct {
	path      string
	udid      string // the simulator the process ran in
	pid       int
	app       string // bundle id
	process   string
	time      time.Time // when the crash was captured
	kind      api.CrashKind
	exception string
	reason    string
	thread    string
	frames    []string
}

func (r *ipsReport) crash() api.Crash {
	return api.Crash{Time: r.time, App: r.app, Process: r.process, Pid: r.pid, Kind: r.kind, Exception: r.exception,
		Reason: r.reason, Thread: r.thread, Frames: r.frames, Report: r.path}
}

// ipsBody is the part of an .ips report's body agemu reads.
type ipsBody struct {
	ProcPath    string `json:"procPath"`
	ProcName    string `json:"procName"`
	Pid         int    `json:"pid"`
	CaptureTime string `json:"captureTime"`
	BundleInfo  struct {
		ID string `json:"CFBundleIdentifier"`
	} `json:"bundleInfo"`
	Exception struct {
		Type    string `json:"type"`
		Signal  string `json:"signal"`
		Subtype string `json:"subtype"`
	} `json:"exception"`
	Termination struct {
		Namespace string   `json:"namespace"`
		Code      any      `json:"code"`
		Indicator string   `json:"indicator"`
		Reasons   []string `json:"reasons"`
	} `json:"termination"`
	ASI                    map[string][]string `json:"asi"`
	LastExceptionBacktrace []ipsFrame          `json:"lastExceptionBacktrace"`
	FaultingThread         int                 `json:"faultingThread"`
	Threads                []struct {
		Name   string     `json:"name"`
		Queue  string     `json:"queue"`
		Frames []ipsFrame `json:"frames"`
	} `json:"threads"`
	UsedImages []struct {
		Name string `json:"name"`
		Path string `json:"path"`
	} `json:"usedImages"`
}

type ipsFrame struct {
	ImageOffset    uint64 `json:"imageOffset"`
	ImageIndex     int    `json:"imageIndex"`
	Symbol         string `json:"symbol"`
	SymbolLocation uint64 `json:"symbolLocation"`
}

var simDevicePath = regexp.MustCompile(`/CoreSimulator/Devices/([0-9A-Fa-f-]{36})/`)

// ipsTimeLayout is captureTime: "2026-11-07 06:27:34.3842 +0100".
const ipsTimeLayout = "2006-01-02 15:04:05.9999 -0700"

// parseIPS reads a crash report (bug_type 309): a JSON header line, then a JSON body. ok is
// false for other reports and for processes outside a simulator.
func parseIPS(file string, data []byte) (r *ipsReport, ok bool, err error) {
	nl := bytes.IndexByte(data, '\n')
	if nl < 0 {
		return nil, false, errors.New("no header line")
	}
	var head struct {
		BugType  string `json:"bug_type"`
		BundleID string `json:"bundleID"`
		AppName  string `json:"app_name"`
	}
	if err := json.Unmarshal(data[:nl], &head); err != nil {
		return nil, false, err
	}
	// Only a simulator process's report names its device (ReportCrash escapes the slashes).
	rest := data[nl:]
	if head.BugType != "309" || (!bytes.Contains(rest, []byte(`CoreSimulator\/Devices\/`)) && !bytes.Contains(rest, []byte("CoreSimulator/Devices/"))) {
		return nil, false, nil
	}
	var body ipsBody
	if err := json.Unmarshal(data[nl+1:], &body); err != nil {
		return nil, false, err
	}
	m := simDevicePath.FindStringSubmatch(body.ProcPath)
	if m == nil {
		return nil, false, nil
	}
	r = &ipsReport{path: file, udid: strings.ToUpper(m[1]), pid: body.Pid, process: body.ProcName, kind: api.CrashKindCrash}
	r.app = body.BundleInfo.ID
	if r.app == "" {
		r.app = head.BundleID
	}
	if r.process == "" {
		r.process = path.Base(body.ProcPath)
	}
	if t, err := time.Parse(ipsTimeLayout, body.CaptureTime); err == nil {
		r.time = t
	}
	r.exception = body.Exception.Type
	if body.Exception.Signal != "" {
		r.exception += " (" + body.Exception.Signal + ")"
	}
	var asi []string
	images := make([]string, 0, len(body.ASI))
	for img := range body.ASI {
		images = append(images, img)
	}
	sort.Strings(images)
	for _, img := range images {
		asi = append(asi, body.ASI[img]...)
	}
	switch {
	case len(asi) > 0:
		r.reason = strings.Join(asi, "\n")
	case body.Exception.Subtype != "":
		r.reason = body.Exception.Subtype
	case len(body.Termination.Reasons) > 0:
		r.reason = strings.Join(body.Termination.Reasons, "; ")
	default:
		r.reason = body.Termination.Indicator
	}
	ns := strings.ToUpper(body.Termination.Namespace)
	code := strings.ToLower(fmt.Sprint(body.Termination.Code))
	if ns == "JETSAM" || ns == "MEMORYSTATUS" || (ns == "FRONTBOARD" && (strings.Contains(code, "8badf00d") || code == "2343432205")) {
		r.kind = api.CrashKindKilled
	}
	name := func(i int) string {
		if i >= 0 && i < len(body.UsedImages) {
			if n := body.UsedImages[i].Name; n != "" {
				return n
			}
			if p := body.UsedImages[i].Path; p != "" {
				return path.Base(p)
			}
		}
		return "???"
	}
	frames := body.LastExceptionBacktrace
	if ft := body.FaultingThread; ft >= 0 && ft < len(body.Threads) {
		th := body.Threads[ft]
		switch {
		case th.Name != "":
			r.thread = fmt.Sprintf("Thread %d (%s)", ft, th.Name)
		case th.Queue != "":
			r.thread = fmt.Sprintf("Thread %d (queue %s)", ft, th.Queue)
		default:
			r.thread = fmt.Sprintf("Thread %d", ft)
		}
		if len(frames) == 0 {
			frames = th.Frames
		}
	}
	for _, f := range frames {
		if len(r.frames) == api.MaxCrashFrames {
			break
		}
		if f.Symbol != "" {
			r.frames = append(r.frames, fmt.Sprintf("%s %s + %d", name(f.ImageIndex), f.Symbol, f.SymbolLocation))
		} else {
			r.frames = append(r.frames, fmt.Sprintf("%s + 0x%x", name(f.ImageIndex), f.ImageOffset))
		}
	}
	return r, true, nil
}

// reportDir reads the Mac's crash reports: ~/Library/Logs/DiagnosticReports, or
// AGEMU_IOS_CRASH_REPORTS (tests).
type reportDir struct {
	mu   sync.Mutex
	dir  string
	seen map[string]reportEntry // file name -> what it holds
	busy bool
	last time.Time
}

type reportEntry struct {
	mod    time.Time
	parsed bool
	report *ipsReport // nil: not read yet, or not a simulator crash report
}

func newReportDir() *reportDir {
	dir := os.Getenv("AGEMU_IOS_CRASH_REPORTS")
	if dir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			dir = filepath.Join(home, "Library", "Logs", "DiagnosticReports")
		}
	}
	return &reportDir{dir: dir, seen: map[string]reportEntry{}}
}

// due claims the next background scan when none runs and the last began interval ago.
func (d *reportDir) due(interval time.Duration) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.busy || time.Since(d.last) < interval {
		return false
	}
	d.busy, d.last = true, time.Now()
	return true
}

func (d *reportDir) done() {
	d.mu.Lock()
	d.busy = false
	d.mu.Unlock()
}

// scan returns the simulator crash reports written at or after since. Each file is listed
// once and parsed at most once (ReportCrash writes a report once); a file written before since
// cannot hold a crash after it and is not read.
func (d *reportDir) scan(since time.Time) ([]*ipsReport, error) {
	if d.dir == "" {
		return nil, nil
	}
	ents, err := os.ReadDir(d.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []*ipsReport
	for _, e := range ents {
		name := e.Name()
		if !strings.HasSuffix(name, ".ips") {
			continue
		}
		c, ok := d.seen[name]
		if !ok {
			info, err := e.Info()
			if err != nil {
				continue
			}
			c = reportEntry{mod: info.ModTime()}
		}
		if !c.parsed && !c.mod.Before(since) {
			var retry bool
			c.report, retry = readReport(filepath.Join(d.dir, name))
			// A report that does not parse may still be being written: read it again later.
			c.parsed = !retry || time.Since(c.mod) > time.Minute
		}
		d.seen[name] = c
		if c.report != nil && !c.report.time.Before(since) {
			out = append(out, c.report)
		}
	}
	return out, nil
}

// readReport parses one report file. It returns nil for a report that is not a simulator
// crash report; retry is set when the file could not be read or parsed.
func readReport(file string) (r *ipsReport, retry bool) {
	f, err := os.Open(file)
	if err != nil {
		return nil, true
	}
	defer f.Close()
	// Reports are tens of KB; 8 MB bounds a broken one.
	data, err := io.ReadAll(io.LimitReader(f, 8<<20))
	if err != nil {
		return nil, true
	}
	r, ok, err := parseIPS(file, data)
	if err != nil {
		return nil, true
	}
	if !ok {
		return nil, false
	}
	return r, false
}
