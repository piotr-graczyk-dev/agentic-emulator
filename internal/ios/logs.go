//go:build !windows

package ios

// Logs (docs/CAPABILITIES.md "Logs and crashes", docs/IOS.md "Logs and crashes").
//
// The Simulator's processes log to the simulator's own unified log (its logd and diagnosticd),
// which the Mac's `log` does not show. agemu reads it the way `xcrun simctl spawn <udid> log
// stream` does: one long-lived `log stream --style ndjson --level info` per booted simulator,
// spawned inside the device by CoreSimulator's simctl, read line by line into two ring buffers
// (user apps' processes, and everything else). A logs call reads the rings in memory; nothing
// runs on the device per call.
//
// Observe only: the stream asks for info, never debug (a debug stream turns debug logging on in
// every process), and launched apps get no logging environment. The capture starts when an app
// is installed or launched through agemu, or at the first logs call, and runs until the device
// shuts down or the daemon exits. User apps' lines logged in the 30 s before the capture
// started, up to the stream's first line, are read once from the unified log's store (`log
// show`), which keeps default, error and fault lines but not info lines.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/api"
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/backend"
)

var (
	_ backend.LogReader   = (*Backend)(nil)
	_ backend.CrashReader = (*Backend)(nil)
	_ backend.AppWatcher  = (*Backend)(nil)
)

const (
	// ringBytes bounds each of a device's two rings, by api.LogEntry.Size.
	ringBytes = 2 << 20
	// backfillLead is how far before the capture's start the one-time backfill reads: an app
	// launched (or installed and launched) while the stream starts is inside it.
	backfillLead = 30 * time.Second
	// firstLineWait bounds how long the first logs call after a capture started waits for the
	// stream's first line, so that call already returns lines.
	firstLineWait = 3 * time.Second
	// maxStreamLine drops a stream line longer than this (a message is cut at
	// api.MaxLogMessage anyway; the unified log itself cuts long arguments).
	maxStreamLine = 256 << 10
)

// logCaptureOn: AGEMU_IOS_LOG_CAPTURE=0 turns the live log stream off (logs then answers
// that capture is off; the crash watcher still sees exits and .ips reports).
func logCaptureOn() bool {
	switch strings.ToLower(os.Getenv("AGEMU_IOS_LOG_CAPTURE")) {
	case "0", "false", "no":
		return false
	}
	return true
}

// streamEvent is the part of a `log stream --style ndjson` line agemu uses.
type streamEvent struct {
	EventType        string `json:"eventType"`
	MessageType      string `json:"messageType"`
	Timestamp        string `json:"timestamp"`
	ProcessID        int    `json:"processID"`
	ThreadID         int64  `json:"threadID"`
	ProcessImagePath string `json:"processImagePath"`
	SenderImagePath  string `json:"senderImagePath"`
	Subsystem        string `json:"subsystem"`
	Category         string `json:"category"`
	EventMessage     string `json:"eventMessage"`
}

// streamTimeLayout is the ndjson timestamp: local time with the zone offset.
const streamTimeLayout = "2006-01-02 15:04:05.999999-0700"

// logLevel maps a unified log type to the neutral level (docs/CAPABILITIES.md: default and
// info are info, fault is fatal).
func logLevel(messageType string) api.LogLevel {
	switch messageType {
	case "Default", "Info":
		return api.LogInfo
	case "Debug":
		return api.LogDebug
	case "Error":
		return api.LogError
	case "Fault":
		return api.LogFatal
	}
	return ""
}

// logTag is "subsystem:category", the subsystem alone, or else the name of the image that
// logged the line ("Foundation" for NSLog).
func logTag(ev *streamEvent) string {
	switch {
	case ev.Subsystem != "" && ev.Category != "":
		return ev.Subsystem + ":" + ev.Category
	case ev.Subsystem != "":
		return ev.Subsystem
	}
	return path.Base(ev.SenderImagePath)
}

// bundleKey names the app bundle an executable belongs to, independent of where the device's
// data or the runtime live: "user/<container UUID>/<Name>.app" for an installed app,
// "system/<path>/<Name>.app" for a built-in one, "" for anything else. An extension
// (Name.app/PlugIns/X.appex/X) belongs to its app. p is an executable or a bundle path.
func bundleKey(p string) string {
	const user = "/Containers/Bundle/Application/"
	if i := strings.Index(p, user); i >= 0 {
		rest := p[i+len(user):]
		parts := strings.SplitN(rest, "/", 3)
		if len(parts) >= 2 && strings.HasSuffix(parts[1], ".app") {
			return "user/" + parts[0] + "/" + parts[1]
		}
		return ""
	}
	const runtime = "/RuntimeRoot/"
	if i := strings.Index(p, runtime); i >= 0 {
		rest := p[i+len(runtime):]
		if j := strings.Index(rest+"/", ".app/"); j >= 0 {
			return "system/" + rest[:j+4]
		}
	}
	return ""
}

// logRecord is one captured line with its capture sequence number and the bundle of the
// process that logged it.
type logRecord struct {
	seq    uint64
	bundle string
	entry  api.LogEntry
}

// logRing keeps the newest records within max bytes (api.LogEntry.Size), oldest first, in
// capture order (seq increases).
type logRing struct {
	recs  []logRecord
	head  int // index of the oldest record kept
	bytes int
	max   int
	// The newest sequence number and time among the records dropped to stay within max: a
	// window reaching back to them is partial.
	dropSeq  uint64
	dropTime time.Time
}

func (r *logRing) add(rec logRecord) {
	r.recs = append(r.recs, rec)
	r.bytes += rec.entry.Size()
	for r.bytes > r.max && r.head < len(r.recs)-1 {
		old := &r.recs[r.head]
		r.bytes -= old.entry.Size()
		r.dropSeq = max(r.dropSeq, old.seq)
		if old.entry.Time.After(r.dropTime) {
			r.dropTime = old.entry.Time
		}
		*old = logRecord{}
		r.head++
	}
	if r.head > 4096 && r.head > len(r.recs)/2 {
		n := copy(r.recs, r.recs[r.head:])
		clear(r.recs[n:])
		r.recs = r.recs[:n]
		r.head = 0
	}
}

func (r *logRing) live() []logRecord { return r.recs[r.head:] }

// prepend puts records older than everything held (the backfill) in front, within the bound.
func (r *logRing) prepend(older []logRecord) {
	keep := append(append([]logRecord(nil), older...), r.live()...)
	r.recs, r.head, r.bytes = nil, 0, 0
	for _, rec := range keep {
		r.add(rec)
	}
}

// logStream is one running `log stream`: its simctl relay on the Mac and the reader's end.
type logStream struct {
	cmd  *exec.Cmd
	done chan struct{} // closed when the reader has seen the stream end
}

// The stream runs inside the simulator, under its launchd_sim; simctl only relays it. When the
// relay is killed (SIGKILL, or SIGPIPE after the daemon died) the simulator's `log` process can
// stay behind and keep running (1 of 2 SIGKILLs in a check; 19 left over from daemon restarts
// cost about 1.6% of a core each). So agemu stops the relay with SIGTERM, which simctl passes
// on, and marks its streams with a --timeout of streamMarkBase plus its own pid, in minutes (a
// valid timeout of about two years): a later start finds and ends the streams whose daemon is
// gone, or that its own daemon no longer reads.
const streamMarkBase = 1_000_000

// streamArgs are agemu's `simctl spawn` arguments for a device's stream, owned by daemon pid
// owner.
func streamArgs(udid string, owner int) []string {
	return []string{"spawn", udid, "log", "stream", "--style", "ndjson", "--level", "info",
		"--timeout", strconv.Itoa(streamMarkBase+owner) + "m"}
}

var streamCommand = regexp.MustCompile(`^log stream --style ndjson --level info --timeout (\d+)m$`)

// strayStreams finds, in `ps -axwwo pid=,ppid=,command=` output, agemu's streams inside the
// device that nobody reads: those of a daemon that is no longer running, and every one of this
// daemon's (self) when it has no stream on the device (a new start, or a stop).
func strayStreams(ps, udid string, self int) []int {
	daemons := map[int]bool{}
	for _, line := range strings.Split(ps, "\n") {
		f := strings.Fields(line)
		if len(f) >= 3 && strings.HasSuffix(f[2], "agemud") {
			if pid, err := strconv.Atoi(f[0]); err == nil {
				daemons[pid] = true
			}
		}
	}
	children, _ := simChildren(ps, udid)
	var out []int
	for _, c := range children {
		m := streamCommand.FindStringSubmatch(c.cmd)
		if m == nil {
			continue
		}
		mark, err := strconv.Atoi(m[1])
		if err != nil || mark < streamMarkBase {
			continue
		}
		if owner := mark - streamMarkBase; owner == self || !daemons[owner] {
			out = append(out, c.pid)
		}
	}
	sort.Ints(out)
	return out
}

// endStrayStreams ends agemu's streams inside the device that nobody reads (strayStreams). The
// caller has no stream running on the device.
func endStrayStreams(udid string) {
	out, err := exec.Command("ps", "-axwwo", "pid=,ppid=,command=").Output()
	if err != nil {
		return
	}
	for _, pid := range strayStreams(string(out), udid, os.Getpid()) {
		if err := syscall.Kill(pid, syscall.SIGTERM); err == nil {
			log.Printf("ios logs %s: ended a log stream left in the device (pid %d)", udid, pid)
		}
	}
}

// stopStream ends a stream: SIGTERM to its relay, which passes it on to the stream in the
// device; SIGKILL if the relay has not ended within 2 s; then any of agemu's streams left in
// the device.
func stopStream(udid string, s *logStream) {
	_ = s.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-s.done:
	case <-time.After(2 * time.Second):
		_ = s.cmd.Process.Kill()
	}
	endStrayStreams(udid)
}

// deviceLogs is everything agemu captured on one booted simulator: the log rings and the
// process records the crash watcher keeps (crashes.go). Guarded by mu.
type deviceLogs struct {
	mu   sync.Mutex
	udid string

	stream    *logStream
	started   time.Time // when the current stream was started
	firstLine time.Time // time of the first line the current stream delivered (zero before)
	firstSeen chan struct{}
	startSeq  uint64 // the first sequence number of the current capture
	seq       uint64 // the newest sequence number used
	user      logRing
	rest      logRing
	gapAfter  time.Time // a stream restart or a loss: windows reaching before this are partial
	backfill  bool      // the backfill ran (or is running) for this capture
	lines     uint64    // lines read from the stream, for the daemon log
	parseErrs uint64

	// bundles maps a bundleKey to its app's bundle id; "" marks a key the helper could not
	// name (the app was uninstalled). unresolved holds user keys seen in the stream and not
	// looked up yet.
	bundles    map[string]string
	unresolved map[string]bool

	intern map[string]string

	// paste is what the stream delivered from the pasteboard daemon (paste.go).
	paste pasteLog

	// procs are the app processes the crash watcher knows (crashes.go); expect lists agemu's
	// calls that end apps; changed is closed when the stream adds an exit's details.
	procs   map[int]*appProc
	expect  []exitExpectation
	changed chan struct{}
}

func newDeviceLogs(udid string) *deviceLogs {
	return &deviceLogs{
		udid: udid,
		user: logRing{max: ringBytes}, rest: logRing{max: ringBytes},
		bundles: map[string]string{}, unresolved: map[string]bool{}, intern: map[string]string{},
		procs: map[int]*appProc{},
	}
}

// devLogs returns the device's capture state, creating it.
func (b *Backend) devLogs(udid string) *deviceLogs {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.logs == nil {
		b.logs = map[string]*deviceLogs{}
	}
	dl := b.logs[udid]
	if dl == nil {
		dl = newDeviceLogs(udid)
		b.logs[udid] = dl
	}
	return dl
}

// internLocked shares the storage of repeated strings (process names, tags, bundle keys).
func (dl *deviceLogs) internLocked(s string) string {
	if v, ok := dl.intern[s]; ok {
		return v
	}
	if len(dl.intern) < 8192 {
		dl.intern[s] = s
	}
	return s
}

// startCapture starts the device's log stream if it is not running. Starting one is not free:
// it lists the Mac's processes (`ps`, about 30 ms here) to end streams nobody reads, and starts
// simctl, with the device's capture locked; then it returns (the stream's first lines arrive
// later). A running stream costs nothing here. Called when an app is installed or launched
// through agemu, and by logs.
func (b *Backend) startCapture(d simDevice) *deviceLogs {
	dl := b.devLogs(d.UDID)
	if !logCaptureOn() {
		return dl
	}
	// Count the reader before Close can collect the devices to stop (Close sets closed under
	// b.mu first), so Close either stops this stream or this call starts none.
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return dl
	}
	b.bgWG.Add(1)
	b.mu.Unlock()
	reading := false
	defer func() {
		if !reading {
			b.bgWG.Done()
		}
	}()
	dl.mu.Lock()
	defer dl.mu.Unlock()
	if dl.stream != nil {
		select {
		case <-dl.stream.done:
		default:
			return dl // running
		}
	}
	// A stream that ended may have left its process in the device, as may a daemon that died.
	endStrayStreams(d.UDID)
	now := time.Now()
	cmd := simctlCommand(context.Background(), streamArgs(d.UDID, os.Getpid())...)
	cmd.Stderr = io.Discard
	out, err := cmd.StdoutPipe()
	if err != nil {
		log.Printf("ios logs %s: %v", d.UDID, err)
		return dl
	}
	if err := cmd.Start(); err != nil {
		log.Printf("ios logs %s: starting log stream: %v", d.UDID, err)
		return dl
	}
	s := &logStream{cmd: cmd, done: make(chan struct{})}
	restart := dl.stream != nil || dl.seq > 0
	dl.stream = s
	dl.started = now
	dl.firstLine = time.Time{}
	dl.firstSeen = make(chan struct{})
	if seq := uint64(now.UnixMicro()); seq > dl.seq {
		dl.seq = seq
	}
	dl.startSeq = dl.seq + 1
	if restart {
		dl.gapAfter = now
	}
	reading = true
	go func() {
		defer b.bgWG.Done()
		b.readStream(dl, s, out)
	}()
	log.Printf("ios logs %s: log stream started (pid %d)", d.UDID, cmd.Process.Pid)
	return dl
}

// readStream reads one stream until it ends.
func (b *Backend) readStream(dl *deviceLogs, s *logStream, out io.Reader) {
	defer close(s.done)
	defer func() { _ = s.cmd.Wait() }()
	br := bufio.NewReaderSize(out, maxStreamLine)
	for {
		line, err := br.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			// A very long line: drop it whole.
			for err == bufio.ErrBufferFull {
				_, err = br.ReadSlice('\n')
			}
			continue
		}
		if len(line) > 1 && line[0] == '{' {
			var ev streamEvent
			if jerr := json.Unmarshal(line, &ev); jerr != nil {
				dl.mu.Lock()
				dl.parseErrs++
				dl.mu.Unlock()
			} else {
				b.ingest(dl, s, &ev)
			}
		}
		if err != nil {
			break
		}
	}
	dl.mu.Lock()
	lines, bad := dl.lines, dl.parseErrs
	if dl.stream == s {
		dl.gapAfter = time.Now()
		if dl.firstLine.IsZero() {
			close(dl.firstSeen) // nothing to wait for
			dl.firstSeen = nil
		}
	}
	dl.mu.Unlock()
	log.Printf("ios logs %s: log stream ended after %d lines (%d did not parse)", dl.udid, lines, bad)
}

// ingest adds one stream event: a log line to its ring, and what the crash watcher needs.
func (b *Backend) ingest(dl *deviceLogs, s *logStream, ev *streamEvent) {
	if ev.EventType != "logEvent" {
		return
	}
	t, err := time.Parse(streamTimeLayout, ev.Timestamp)
	if err != nil {
		return
	}
	dl.mu.Lock()
	defer dl.mu.Unlock()
	if dl.stream != s {
		return // a stream that was replaced or stopped
	}
	dl.lines++
	if dl.firstLine.IsZero() {
		dl.firstLine = t
		close(dl.firstSeen)
		dl.firstSeen = nil
		if !dl.backfill {
			dl.backfill = true
			from := dl.started.Add(-backfillLead)
			b.bgWG.Add(1)
			go func() {
				defer b.bgWG.Done()
				b.runBackfill(dl, s, from, t)
			}()
		}
	}
	rec := dl.recordLocked(ev, t)
	if isPastedLine(ev, rec.bundle) {
		dl.paste.add(t, ev.EventMessage)
	}
	dl.seq++
	rec.seq = dl.seq
	if strings.HasPrefix(rec.bundle, "user/") {
		if _, ok := dl.bundles[rec.bundle]; !ok {
			dl.unresolved[rec.bundle] = true
		}
		dl.user.add(rec)
	} else {
		dl.rest.add(rec)
	}
	dl.observeLocked(ev, &rec, t)
}

// recordLocked converts a stream event to a record (seq not set).
func (dl *deviceLogs) recordLocked(ev *streamEvent, t time.Time) logRecord {
	return logRecord{
		bundle: dl.internLocked(bundleKey(ev.ProcessImagePath)),
		entry: api.LogEntry{
			Time:    t,
			Level:   logLevel(ev.MessageType),
			Pid:     ev.ProcessID,
			Tid:     int(ev.ThreadID),
			Process: dl.internLocked(path.Base(ev.ProcessImagePath)),
			Tag:     dl.internLocked(logTag(ev)),
			Message: api.CutText(ev.EventMessage, api.MaxLogMessage),
		},
	}
}

// backfillTimeLayout is what `log show --start/--end` accept (whole seconds, local time).
const backfillTimeLayout = "2006-01-02 15:04:05"

// backfillPredicate limits the backfill to what it is for: user apps' processes (an app
// launched while the stream was starting) and runningboardd's exit lines (how one ended).
const backfillPredicate = `processImagePath CONTAINS "/Containers/Bundle/Application/" OR ` +
	`(process == "runningboardd" AND eventMessage CONTAINS "exited with context")`

// backfillBytes bounds what the backfill keeps (by message size plus 200 per line): the rings
// hold 2 MiB each, so older lines would be dropped anyway.
const backfillBytes = 4 << 20

// runBackfill reads, once per capture, what the unified log stored between from and the
// stream's first line (until) for user apps: the lines of an app launched while the stream was
// starting. The store keeps default, error and fault lines; info lines logged before the stream
// was up are lost. The lines go in front of the rings with sequence numbers below the
// capture's.
func (b *Backend) runBackfill(dl *deviceLogs, s *logStream, from, until time.Time) {
	ctx, cancel := context.WithTimeout(b.bg, time.Minute)
	defer cancel()
	start := time.Now()
	cmd := simctlCommand(ctx, "spawn", dl.udid, "log", "show", "--style", "ndjson", "--info",
		"--start", from.Local().Format(backfillTimeLayout), "--end", until.Add(time.Second).Local().Format(backfillTimeLayout),
		"--predicate", backfillPredicate)
	cmd.Stderr = io.Discard
	out, err := cmd.StdoutPipe()
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		log.Printf("ios logs %s: backfill: %v", dl.udid, err)
		return
	}
	var evs []streamEvent
	size := 0
	br := bufio.NewReaderSize(out, maxStreamLine)
	for {
		line, rerr := br.ReadBytes('\n')
		if len(line) > 1 && line[0] == '{' && len(line) < maxStreamLine {
			var ev streamEvent
			if json.Unmarshal(line, &ev) == nil && ev.EventType == "logEvent" {
				evs = append(evs, ev)
				size += len(ev.EventMessage) + 200
				// `log show` prints oldest first: keep the newest.
				for size > backfillBytes && len(evs) > 1 {
					size -= len(evs[0].EventMessage) + 200
					evs = evs[1:]
				}
			}
		}
		if rerr != nil {
			break
		}
	}
	_ = cmd.Wait()
	dl.mu.Lock()
	defer dl.mu.Unlock()
	if dl.stream != s {
		return
	}
	n := dl.addBackfillLocked(evs, from, until)
	log.Printf("ios logs %s: backfill read %d stored lines from %s to the stream's first line in %.0f ms",
		dl.udid, n, from.Format("15:04:05"), ms(time.Since(start)))
}

// addBackfillLocked puts the stored lines logged from from until until (the stream's first
// line) in front of the rings, numbered below the capture's first sequence number, oldest
// first, and returns how many it added. A line the stream delivered too (it can deliver lines
// a few milliseconds out of order) is not added twice.
func (dl *deviceLogs) addBackfillLocked(evs []streamEvent, from, until time.Time) int {
	type lineID struct {
		pid int
		t   time.Time
		msg string
	}
	streamed := map[lineID]bool{}
	for _, r := range []*logRing{&dl.user, &dl.rest} {
		for _, rec := range r.live() {
			if rec.entry.Time.Before(until.Add(time.Second)) {
				streamed[lineID{rec.entry.Pid, rec.entry.Time, rec.entry.Message}] = true
			}
		}
	}
	var user, rest []logRecord
	for i := range evs {
		t, err := time.Parse(streamTimeLayout, evs[i].Timestamp)
		if err != nil || t.Before(from) || !t.Before(until) {
			continue
		}
		rec := dl.recordLocked(&evs[i], t)
		if streamed[lineID{rec.entry.Pid, t, rec.entry.Message}] {
			continue
		}
		if strings.HasPrefix(rec.bundle, "user/") {
			if _, ok := dl.bundles[rec.bundle]; !ok {
				dl.unresolved[rec.bundle] = true
			}
			user = append(user, rec)
		} else {
			rest = append(rest, rec)
		}
		dl.observeLocked(&evs[i], &rec, t)
	}
	sortRecs := func(rs []logRecord) {
		sort.SliceStable(rs, func(i, j int) bool { return rs[i].entry.Time.Before(rs[j].entry.Time) })
	}
	sortRecs(user)
	sortRecs(rest)
	// Number them below the capture's first line, oldest first, across both rings.
	all := append(append([]*logRecord{}, ptrs(user)...), ptrs(rest)...)
	sort.SliceStable(all, func(i, j int) bool { return all[i].entry.Time.Before(all[j].entry.Time) })
	base := dl.startSeq - uint64(len(all)) - 1
	for i, r := range all {
		r.seq = base + uint64(i)
	}
	dl.user.prepend(user)
	dl.rest.prepend(rest)
	return len(all)
}

func ptrs(rs []logRecord) []*logRecord {
	out := make([]*logRecord, len(rs))
	for i := range rs {
		out[i] = &rs[i]
	}
	return out
}

// stopCapture stops the device's stream and forgets what it captured (shutdown, Close).
func (b *Backend) stopCapture(udid string) {
	b.mu.Lock()
	dl := b.logs[udid]
	delete(b.logs, udid)
	b.mu.Unlock()
	if dl == nil {
		return
	}
	dl.mu.Lock()
	s := dl.stream
	dl.stream = nil
	dl.notifyLocked()
	dl.mu.Unlock()
	if s != nil {
		stopStream(udid, s)
	}
}

// clear empties the device's rings and crash records and keeps the stream running (reset:
// nothing from before it is returned).
func (dl *deviceLogs) clear() {
	dl.mu.Lock()
	defer dl.mu.Unlock()
	dl.user = logRing{max: ringBytes}
	dl.rest = logRing{max: ringBytes}
	dl.unresolved = map[string]bool{}
	for k, v := range dl.bundles {
		if strings.HasPrefix(k, "user/") || v == "" {
			delete(dl.bundles, k)
		}
	}
	dl.procs = map[int]*appProc{}
	dl.gapAfter = time.Time{}
	if dl.stream != nil {
		dl.startSeq = dl.seq + 1
	}
}

// Logs returns the captured lines of q.App's processes (every process when q.App is ""),
// starting the capture if it is not running. The first call after the capture started waits
// up to 3 s for the stream's first line.
func (b *Backend) Logs(ctx context.Context, id string, q api.LogQuery) (api.LogBatch, error) {
	if !logCaptureOn() {
		return api.LogBatch{}, fmt.Errorf("log capture is off on this daemon (AGEMU_IOS_LOG_CAPTURE=0)")
	}
	d, err := b.target(ctx, id)
	if err != nil {
		return api.LogBatch{}, err
	}
	if d.StateCode != stateBooted {
		return api.LogBatch{}, fmt.Errorf("simulator %s is not booted (%s)", d.Name, d.State)
	}
	dl := b.startCapture(d)
	dl.mu.Lock()
	first := dl.firstSeen
	waiting := dl.firstLine.IsZero()
	dl.mu.Unlock()
	if waiting && first != nil {
		wctx, cancel := context.WithTimeout(ctx, firstLineWait)
		select {
		case <-first:
		case <-wctx.Done():
		}
		cancel()
	}
	var keys map[string]bool
	if q.App != "" {
		if keys, err = b.appKeys(ctx, dl, q.App); err != nil {
			return api.LogBatch{}, err
		}
	}
	dl.mu.Lock()
	defer dl.mu.Unlock()
	return dl.queryLocked(q, keys), nil
}

// queryLocked selects the records matching q (keys: the bundles of q.App; nil: every process),
// oldest first, and the window's accounting (see api.LogBatch).
func (dl *deviceLogs) queryLocked(q api.LogQuery, keys map[string]bool) api.LogBatch {
	out := api.LogBatch{Cursor: dl.seq}
	if out.Cursor == 0 {
		out.Cursor = dl.startSeq
	}
	minRank := q.Level.Rank()
	match := func(r *logRecord) bool {
		if r.seq <= q.After || (!q.Since.IsZero() && r.entry.Time.Before(q.Since)) {
			return false
		}
		if keys != nil && !keys[r.bundle] {
			return false
		}
		return minRank == 0 || r.entry.Level.Rank() >= minRank
	}
	rings := []*logRing{&dl.user, &dl.rest}
	if keys != nil && !hasSystemKey(keys) {
		rings = rings[:1]
	}
	var lists [][]logRecord
	for _, r := range rings {
		live := r.live()
		// Records are in seq order: skip those at or before the cursor with a binary search.
		i := sort.Search(len(live), func(i int) bool { return live[i].seq > q.After })
		var sel []logRecord
		for j := i; j < len(live); j++ {
			if match(&live[j]) {
				sel = append(sel, live[j])
			}
		}
		lists = append(lists, sel)
	}
	for _, rec := range mergeBySeq(lists) {
		out.Entries = append(out.Entries, rec.entry)
	}

	// What the capture holds whole for these processes: from the stream's first line (the
	// backfill before it lacks info lines), the newest line its rings dropped, or the last
	// stream restart.
	from := dl.firstLine
	var dropSeq uint64
	for _, r := range rings {
		if r.dropTime.After(from) {
			from = r.dropTime
		}
		dropSeq = max(dropSeq, r.dropSeq)
	}
	if dl.gapAfter.After(from) {
		from = dl.gapAfter
	}
	out.CapturedFrom = from
	switch {
	case q.After > 0:
		// A cursor of an earlier capture (another boot, daemon or stream), or lines after it
		// dropped since.
		out.Partial = q.After+1 < dl.startSeq || dropSeq > q.After
	case from.IsZero():
		out.Partial = true // nothing captured yet
	default:
		out.Partial = q.Since.IsZero() || q.Since.Before(from)
	}
	return out
}

func hasSystemKey(keys map[string]bool) bool {
	for k := range keys {
		if strings.HasPrefix(k, "system/") {
			return true
		}
	}
	return false
}

// mergeBySeq merges lists sorted by seq.
func mergeBySeq(lists [][]logRecord) []logRecord {
	n := 0
	for _, l := range lists {
		n += len(l)
	}
	out := make([]logRecord, 0, n)
	idx := make([]int, len(lists))
	for len(out) < n {
		best := -1
		for i, l := range lists {
			if idx[i] < len(l) && (best < 0 || l[idx[i]].seq < lists[best][idx[best]].seq) {
				best = i
			}
		}
		out = append(out, lists[best][idx[best]])
		idx[best]++
	}
	return out
}

// userAppPath is the helper's "userappinfo" entry with the bundle's path.
type userAppPath struct {
	ID   string `json:"id"`
	Path string `json:"path"`
}

// appKeys returns the bundle keys of app's processes: every install of it seen during the
// capture (a reinstall moves it to a new container) and its current one.
func (b *Backend) appKeys(ctx context.Context, dl *deviceLogs, app string) (map[string]bool, error) {
	dl.mu.Lock()
	keys := keysOf(dl.bundles, app)
	unresolved := len(dl.unresolved) > 0
	dl.mu.Unlock()
	if len(keys) > 0 && !unresolved {
		return keys, nil
	}
	// User apps: one read of the device's installed bundles names every key seen so far.
	var apps []userAppPath
	if err := b.call(ctx, "userappinfo", map[string]any{"udid": dl.udid}, &apps); err != nil {
		return nil, err
	}
	dl.mu.Lock()
	for _, a := range apps {
		if k := bundleKey(a.Path); k != "" {
			dl.bundles[k] = a.ID
		}
	}
	for k := range dl.unresolved {
		if _, ok := dl.bundles[k]; !ok {
			dl.bundles[k] = "" // no longer installed: it cannot be named now
		}
	}
	dl.unresolved = map[string]bool{}
	keys = keysOf(dl.bundles, app)
	dl.mu.Unlock()
	if len(keys) > 0 {
		return keys, nil
	}
	// A built-in app: its bundle in the runtime.
	info, err := b.appInfo(ctx, dl.udid, app)
	if err != nil {
		return map[string]bool{}, nil // not installed now and never seen: no lines
	}
	k := bundleKey(info.Path)
	dl.mu.Lock()
	if k != "" {
		dl.bundles[k] = app
	}
	keys = keysOf(dl.bundles, app)
	dl.mu.Unlock()
	return keys, nil
}

func keysOf(bundles map[string]string, app string) map[string]bool {
	keys := map[string]bool{}
	for k, v := range bundles {
		if v == app {
			keys[k] = true
		}
	}
	return keys
}
