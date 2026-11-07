package android

// Logs and crashes (docs/CAPABILITIES.md, "Logs and crashes"; backend.LogReader, CrashReader
// and AppWatcher). One `logcat -B` per device, opened through the adb server's exec service
// (no adb process), streams logd's main, system, crash and events buffers into the daemon:
//
//   - log lines go into two ring buffers, user apps' lines and everything else's, so system
//     chatter cannot push an app's lines out;
//   - ActivityManager's events (am_proc_start, am_proc_died, am_kill, am_crash, am_anr) and the
//     crash buffer (AndroidRuntime's FATAL EXCEPTION, libc's "Fatal signal", crash_dump's
//     tombstone) become crash records and tell the watcher which app processes live
//     (crashes.go).
//
// Capture only reads logd, as `adb logcat` does: no buffer size, log level or property is
// changed. The guest's clock is not the Mac's (after a snapshot load it runs about a second
// behind), so lines and crash records keep their guest times and every result converts them
// with the current estimate of the offset (guestClock), which never places an event before it
// happened.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
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
	// readyWait bounds how long a logs or crashes call waits for a capture that is starting
	// (clock offset, process names, the backfill from logd's buffer).
	readyWait = 10 * time.Second
	// psEvery spaces the process list reads that name processes the stream has not named.
	psEvery = 2 * time.Second
)

// logsEnabled: AGEMU_ANDROID_LOGS=0 turns capture off (logs and crashes then answer that it
// is off; calls carry no crash notice).
func logsEnabled() bool { return env("AGEMU_ANDROID_LOGS", "1") != "0" }

// clockWindow: the offset estimate is the tightest bound of the last one to two windows, so it
// follows a slow drift between the two clocks.
const clockWindow = time.Minute

// guestClock estimates the Mac's clock minus the guest's as an upper bound, so a converted time
// is never earlier than the event it stamps: a guest timestamp seen at Mac time t was taken at
// or before t, so the offset is at most t minus it. Every `date` reply and every record as it
// arrives gives such a bound; a record that comes through the stream at once gives a bound
// within the stream's latency of the true offset (a `date` round trip over adb is about 20 ms).
// A midpoint estimate, ±half a round trip, put a crash 3 ms before the launch that caused it,
// which hides the crash from the notice.
type guestClock struct {
	cur, prev       time.Duration // the smallest bound in the current and the previous window
	curSet, prevSet bool
	start           time.Time // the current window's start (Mac time)
}

// observe takes a guest timestamp seen at Mac time now.
func (g *guestClock) observe(now, guest time.Time) {
	b := now.Sub(guest)
	if !g.curSet || now.Sub(g.start) > clockWindow {
		g.prev, g.prevSet = g.cur, g.curSet
		g.cur, g.curSet, g.start = b, true, now
		return
	}
	if b < g.cur {
		g.cur = b
	}
}

// offset is the current estimate (0 before any observation).
func (g *guestClock) offset() time.Duration {
	if g.prevSet && g.prev < g.cur {
		return g.prev
	}
	return g.cur
}

// logRec is one captured line (its time is the guest's) and the capture's sequence number for
// it (the cursor).
type logRec struct {
	seq uint64
	uid int
	e   api.LogEntry
}

// logRing keeps the newest lines within max bytes (api.LogEntry.Size), oldest first.
type logRing struct {
	max     int
	bytes   int
	recs    []logRec
	dropSeq uint64    // seq of the newest line dropped; 0: none
	dropAt  time.Time // and its guest time
}

func (r *logRing) push(rec logRec) {
	r.recs = append(r.recs, rec)
	r.bytes += rec.e.Size()
	for r.bytes > r.max && len(r.recs) > 1 {
		old := r.recs[0]
		r.bytes -= old.e.Size()
		r.dropSeq, r.dropAt = old.seq, old.e.Time
		r.recs[0] = logRec{} // release its strings now, not when the array is reallocated
		r.recs = r.recs[1:]
	}
}

// procInfo is a live process of an app (uid >= 10000), from the process list at capture start
// and ActivityManager's process start events.
type procInfo struct {
	uid  int
	name string
}

// recKey identifies a record across a reopened stream, which restarts at the newest record's
// millisecond and so sends some records again.
type recKey struct {
	sec, nsec uint32
	pid, tid  int
	lid       uint32
	n         int
}

// recentKeys remembers the keys of the last records taken. logd sends the buffers' records in
// arrival order, not strictly by time, so a reopened stream's repeats are found by key.
type recentKeys struct {
	keys [512]recKey
	next int
	set  map[recKey]bool
}

// seen reports whether k was taken already, and remembers it.
func (r *recentKeys) seen(k recKey) bool {
	if r.set == nil {
		r.set = map[recKey]bool{}
	}
	if r.set[k] {
		return true
	}
	if old := r.keys[r.next]; old != (recKey{}) {
		delete(r.set, old)
	}
	r.keys[r.next] = k
	r.next = (r.next + 1) % len(r.keys)
	r.set[k] = true
	return false
}

// logCapture is one device's capture: from its start (the first launch, logs or crashes call)
// to a reset, standby, shutdown or the daemon's exit.
type logCapture struct {
	name   string
	serial string
	floor  time.Time // Mac time the backfill starts at (the last snapshot load); zero: all of logd's buffer
	ctx    context.Context
	cancel context.CancelFunc
	ready  chan struct{} // closed once the backfill is in, or the start failed (err)
	done   chan struct{} // closed when the capture's goroutine has ended

	// shell runs a guest command (adbShellOK; replaced in tests).
	shell func(ctx context.Context, cmd string) (string, error)
	// open opens a stream of a guest command's stdout (adb exec; replaced in tests).
	open func(ctx context.Context, cmd string) (io.ReadCloser, error)
	// tick is the busy meter's clock (time.Now; replaced in tests).
	tick func() time.Time

	mu      sync.Mutex
	err     error          // why the capture did not start
	clock   guestClock     // Mac clock minus guest clock
	loc     *time.Location // the guest's time zone (dropbox prints local times)
	seq     uint64         // the newest assigned sequence number
	first   uint64         // the first sequence number of this capture (its cursors start above it)
	apps    logRing
	other   logRing
	from    time.Time // the oldest time held when nothing was dropped (the backfill's start)
	names   map[int]string
	pkgUID  map[string]int
	sysApp  map[int]bool // app ids (uid % 100000) of system packages
	tags    map[string]string
	unnamed bool // a line came from a process without a name since the last process list
	lastPS  time.Time
	psBusy  bool
	last    time.Time // guest time of the newest record taken
	recent  recentKeys
	w       watch // crash records and live app processes (crashes.go)
}

func newLogCapture(name, serial string, floor time.Time, firstSeq uint64) *logCapture {
	ctx, cancel := context.WithCancel(context.Background())
	c := &logCapture{
		name: name, serial: serial, floor: floor, ctx: ctx, cancel: cancel,
		ready: make(chan struct{}), done: make(chan struct{}),
		loc:  time.UTC,
		apps: logRing{max: ringBytes}, other: logRing{max: ringBytes},
		names: map[int]string{}, pkgUID: map[string]int{}, sysApp: map[int]bool{}, tags: map[string]string{},
		seq: firstSeq, first: firstSeq,
		w:    newWatch(),
		tick: time.Now,
	}
	c.shell = func(ctx context.Context, cmd string) (string, error) { return adbShellOK(ctx, c.serial, cmd) }
	c.open = func(ctx context.Context, cmd string) (io.ReadCloser, error) {
		// exec runs the command without a pty, so stdout is the binary stream; stderr is dropped.
		return adbOpen(ctx, c.serial, "exec:"+cmd+" 2>/dev/null")
	}
	return c
}

// ------------------------------------------------------------------ lifecycle

// startCapture starts the device's capture unless one runs (or capture is off). It does not
// wait: the capture backfills what it missed from logd's buffer.
func (d *device) startCapture() *logCapture {
	if !logsEnabled() {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if c := d.logs; c != nil {
		select {
		case <-c.done: // ended (the device went away, or the start failed): start anew
		default:
			return c
		}
	}
	if d.run.PID == 0 {
		return nil
	}
	first := uint64(time.Now().UnixMicro())
	if first <= d.lastSeq {
		first = d.lastSeq + 1
	}
	c := newLogCapture(d.name, d.run.serial(), d.captureFloor(), first)
	d.logs = c
	go c.run()
	return c
}

// stopCapture ends the device's capture (reset, standby, shutdown): its lines and crash
// records go with it. The next capture's cursors continue above this one's.
func (d *device) stopCapture() {
	d.mu.Lock()
	c := d.logs
	d.logs = nil
	if c != nil {
		c.mu.Lock()
		if c.seq > d.lastSeq {
			d.lastSeq = c.seq
		}
		c.mu.Unlock()
	}
	d.mu.Unlock()
	if c != nil {
		c.cancel()
	}
}

// capture returns the device's running capture, starting one if needed, once it is ready.
func (d *device) capture(ctx context.Context) (*logCapture, error) {
	if !logsEnabled() {
		return nil, errors.New("log capture is off (AGEMU_ANDROID_LOGS=0)")
	}
	if _, _, err := d.connRaw(ctx); err != nil {
		return nil, err
	}
	d.mu.Lock()
	running := d.logs != nil
	d.mu.Unlock()
	// Standby stopped the capture; the paused guest would not answer a new one.
	if !running && d.inStandby(ctx) {
		return nil, fmt.Errorf("device android:%s is shut down (in standby); agemu boot android:%s", d.name, d.name)
	}
	c := d.startCapture()
	if c == nil {
		return nil, fmt.Errorf("device android:%s is not running", d.name)
	}
	wctx, cancel := context.WithTimeout(ctx, readyWait)
	defer cancel()
	select {
	case <-c.ready:
	case <-wctx.Done():
		return nil, fmt.Errorf("the log capture of android:%s did not start in %v", d.name, readyWait)
	}
	c.mu.Lock()
	err := c.err
	c.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("the log capture of android:%s could not start: %w", d.name, err)
	}
	return c, nil
}

// loadFile records the start of the last snapshot load into the running emulator, so a
// restarted daemon backfills from it too (the restored logd holds the baseline's old lines).
type loadRecord struct {
	PID   int   `json:"pid"`
	Start int64 `json:"start"` // unix nanoseconds
}

func loadFile(name string) string { return filepath.Join(logDir(), name+".load.json") }

// noteLoad remembers that a snapshot load into the running emulator started at t.
func (d *device) noteLoad(t time.Time) {
	d.mu.Lock()
	d.loadStart = t
	pid := d.run.PID
	d.mu.Unlock()
	if b, err := json.Marshal(loadRecord{PID: pid, Start: t.UnixNano()}); err == nil {
		_ = os.WriteFile(loadFile(d.name), b, 0o644)
	}
}

// captureFloor is where a new capture's backfill starts: the start of the last snapshot load
// into this emulator process (this daemon's, or one recorded by an earlier daemon), else the
// baseline snapshot's save time (older lines are the baseline's), else nothing (all of logd's
// buffer). The caller holds d.mu.
func (d *device) captureFloor() time.Time {
	if !d.loadStart.IsZero() {
		return d.loadStart
	}
	if b, err := os.ReadFile(loadFile(d.name)); err == nil {
		var r loadRecord
		if json.Unmarshal(b, &r) == nil && r.PID != 0 && r.PID == d.run.PID {
			return time.Unix(0, r.Start)
		}
	}
	if a, err := loadAVD(d.name); err == nil {
		if st, err := os.Stat(filepath.Join(a.Dir, "snapshots", baselineSnapshot, "snapshot.pb")); err == nil {
			return st.ModTime()
		}
	}
	return time.Time{}
}

func (c *logCapture) fail(err error) {
	c.mu.Lock()
	c.err = err
	c.mu.Unlock()
	logf("%s: log capture: %v", c.name, err)
}

// run starts the capture (clock offset, names, backfill) and then follows logd until the
// capture is stopped or the device goes away.
func (c *logCapture) run() {
	defer close(c.done)
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if err = c.setup(); err == nil {
			err = c.backfill()
		}
		if err == nil || c.ctx.Err() != nil {
			break
		}
		select {
		case <-c.ctx.Done():
		case <-time.After(500 * time.Millisecond):
		}
	}
	if err == nil && c.ctx.Err() != nil {
		err = c.ctx.Err()
	}
	if err != nil {
		c.fail(err)
		close(c.ready)
		return
	}
	close(c.ready)
	c.follow()
}

// setup measures the guest clock's offset and reads the time zone, the system packages and the
// process list.
func (c *logCapture) setup() error {
	ctx, cancel := context.WithTimeout(c.ctx, 15*time.Second)
	defer cancel()
	if err := c.measureOffset(ctx); err != nil {
		return fmt.Errorf("guest clock: %w", err)
	}
	out, err := c.shell(ctx, "getprop persist.sys.timezone; echo --; cmd package list packages -U -s; echo --; ps -A -o PID,UID,NAME")
	if err != nil {
		return err
	}
	parts := strings.SplitN(out, "--\n", 3)
	if len(parts) != 3 {
		return fmt.Errorf("unexpected setup output %q", firstLine(out))
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	off := c.clock.offset()
	if tz := strings.TrimSpace(parts[0]); tz != "" {
		if loc, err := time.LoadLocation(tz); err == nil {
			c.loc = loc
		}
	}
	for pkg, uid := range parsePackages(parts[1]) {
		c.pkgUID[pkg] = uid
		c.sysApp[uid%100000] = true
	}
	for pid, p := range parsePS(parts[2]) {
		c.setName(pid, p.name)
		if isAppUID(p.uid) {
			c.w.procs[pid] = p
			if !strings.Contains(p.name, ":") {
				c.pkgUID[p.name] = p.uid // an app's main process is named after its package
			}
		}
	}
	c.lastPS = time.Now()
	logf("%s: log capture: guest clock at most %v behind the Mac's, %d system packages, %d processes",
		c.name, off.Round(time.Millisecond), len(c.sysApp), len(c.names))
	return nil
}

// measureOffset gives the clock estimate its first bounds: three `date` round trips, each
// bounding the offset by the reply's arrival minus the guest time it carries.
func (c *logCapture) measureOffset(ctx context.Context) error {
	var lastErr error
	ok := false
	for i := 0; i < 3; i++ {
		out, err := c.shell(ctx, "date +%s%N")
		t1 := time.Now()
		if err != nil {
			lastErr = err
			continue
		}
		ns, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
		if err != nil {
			lastErr = fmt.Errorf("date: %q", strings.TrimSpace(out))
			continue
		}
		c.mu.Lock()
		c.clock.observe(t1, time.Unix(0, ns))
		c.mu.Unlock()
		ok = true
	}
	if !ok {
		return lastErr
	}
	return nil
}

// toMac converts a guest time with the current offset estimate (the caller holds c.mu).
func (c *logCapture) toMac(t time.Time) time.Time { return t.Add(c.clock.offset()) }

// toGuest converts a Mac time to the guest's clock (the caller holds c.mu).
func (c *logCapture) toGuest(t time.Time) time.Time { return t.Add(-c.clock.offset()) }

// A live stream costs the emulator per record: logcat writes each record on its own, and each
// write crosses out of the VM as a packet. With the probe app logging 500 lines/s, the stream
// took the emulator from 15% to 59% of a core, while a dump every 250 ms (each one burst) took
// it to 20%. So the capture streams while the device is quiet, for crash notices within
// milliseconds, and dumps while it is busy, which delays them by up to pollEvery. Busy means a
// sustained rate: a cold launch alone logs over busyRate records in its first second, and a
// switch then delayed the notice of a crash right after a launch (the most common case) from
// about 10 ms to 50-250 ms.
const (
	busyRate  = 100                    // records in one second that count the second as busy
	busySecs  = 3                      // consecutive busy seconds that switch to dumps
	pollEvery = 250 * time.Millisecond // the dump period while busy
	quietDump = 10                     // a dump with fewer new records switches back to the stream
)

// errBusy ends a live stream that brought more than busyRate records in each of busySecs
// consecutive seconds.
var errBusy = errors.New("more than the stream's record rate")

// maxBackfill bounds how far back a capture reads logd's buffer when it starts. logd keeps its
// buffers compressed (2 MiB each here, some 7 MiB of lines each), so a device that ran for an
// hour since its last load holds over 100,000 records: read through a busy adb server, such a
// backfill took over 20 s, and the first logs call waits for it.
const maxBackfill = 10 * time.Minute

// backfillFrom is the Mac time a capture starting at now backfills from: the last snapshot
// load (floor), but no more than maxBackfill back. Older lines count as not captured
// (partial).
func backfillFrom(floor, now time.Time) time.Time {
	if lim := now.Add(-maxBackfill); floor.Before(lim) {
		return lim
	}
	return floor
}

// backfill takes the lines logd holds from backfillFrom on (they predate the stream), then
// marks the capture ready.
func (c *logCapture) backfill() error {
	ctx, cancel := context.WithTimeout(c.ctx, 30*time.Second)
	defer cancel()
	c.mu.Lock()
	c.from = backfillFrom(c.floor, time.Now())
	cmd := "logcat -B -d -b " + logcatBuffers + " -T " + guestTimeArg(c.toGuest(c.from))
	c.mu.Unlock()
	rc, err := c.open(ctx, cmd)
	if err != nil {
		return err
	}
	defer rc.Close()
	n, err := c.consume(ctx, rc, false)
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("backfill: %w", err)
	}
	if trace {
		logf("%s: log capture: backfilled %d records", c.name, n)
	}
	return nil
}

// follow takes new records until the capture is stopped: a live stream while the device logs
// fewer than busyRate records a second, dumps every pollEvery while it logs more (back to the
// stream when a dump brings fewer than quietDump). A broken stream (adb restarted, the device
// went offline) is reopened from the newest record taken; the capture ends when the device is
// no longer running.
func (c *logCapture) follow() {
	backoff := 100 * time.Millisecond
	polling := false
	for c.ctx.Err() == nil {
		c.mu.Lock()
		since := c.last
		if since.IsZero() {
			since = c.toGuest(c.from)
		}
		c.mu.Unlock()
		cmd := "logcat -B -b " + logcatBuffers
		if polling {
			cmd = "logcat -B -d -b " + logcatBuffers
		}
		if !since.IsZero() {
			cmd += " -T " + guestTimeArg(since)
		}
		t0 := time.Now()
		rc, err := c.open(c.ctx, cmd)
		n := 0
		if err == nil {
			n, err = c.consume(c.ctx, rc, !polling)
			rc.Close()
		}
		if c.ctx.Err() != nil {
			return
		}
		switch {
		case !polling && errors.Is(err, errBusy):
			polling = true
			if trace {
				logf("%s: log capture: over %d records/s, reading logd in dumps every %v", c.name, busyRate, pollEvery)
			}
			continue
		case polling && (err == nil || errors.Is(err, io.EOF)): // a dump ends there
			if n < quietDump {
				polling = false
				if trace {
					logf("%s: log capture: quiet again, streaming", c.name)
				}
				continue
			}
			select {
			case <-c.ctx.Done():
				return
			case <-time.After(pollEvery):
			}
			backoff = 100 * time.Millisecond
			continue
		}
		if _, ok := findRunning(c.name); !ok {
			logf("%s: log capture ended: the device is not running", c.name)
			return
		}
		if time.Since(t0) > 10*time.Second {
			backoff = 100 * time.Millisecond
		}
		logf("%s: log stream broke (%v); reopening in %v", c.name, err, backoff)
		select {
		case <-c.ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, 2*time.Second)
	}
}

// busyMeter tells a sustained record rate: more than busyRate records in each of busySecs
// consecutive one-second windows (a window starts at the first record after the previous one).
type busyMeter struct {
	window time.Time
	in     int // records in the current window
	busy   int // consecutive busy windows just before the current one
}

// add counts a record that arrived at now and reports whether the rate is sustained.
func (m *busyMeter) add(now time.Time) bool {
	if d := now.Sub(m.window); d >= time.Second {
		if m.in > busyRate && d < 2*time.Second {
			m.busy++
		} else {
			m.busy = 0
		}
		m.window, m.in = now, 0
	}
	m.in++
	return m.in > busyRate && m.busy >= busySecs-1
}

// consume reads records until the stream ends, closing it when the capture stops, and returns
// how many were new. With limit set (a live stream), it stops with errBusy once the new records
// keep up a busy rate (busyMeter).
func (c *logCapture) consume(ctx context.Context, rc io.ReadCloser, limit bool) (int, error) {
	stop := context.AfterFunc(ctx, func() { rc.Close() })
	defer stop()
	r := bufio.NewReaderSize(rc, 64<<10)
	n := 0
	var meter busyMeter
	for {
		rec, err := readLogRecord(r)
		if err != nil {
			if ctx.Err() != nil {
				return n, ctx.Err()
			}
			return n, err
		}
		now := time.Now()
		if !c.add(rec, now) {
			continue
		}
		n++
		if !limit {
			continue
		}
		if meter.add(c.tick()) {
			return n, errBusy
		}
	}
}

// ------------------------------------------------------------------ taking records

// add takes one record that arrived at Mac time arrival (zero: unknown): it bounds the clock
// offset, is deduped against a reopened stream or a dump, feeds the watcher, and a text line
// goes into its ring. Times stay the guest's; results convert them. It reports whether the
// record was new.
func (c *logCapture) add(rec logRecord, arrival time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !arrival.IsZero() {
		c.clock.observe(arrival, rec.time)
	}
	key := recKey{sec: uint32(rec.time.Unix()), nsec: uint32(rec.time.Nanosecond()), pid: rec.pid, tid: rec.tid, lid: rec.lid, n: len(rec.msg) + len(rec.tag)}
	if c.recent.seen(key) {
		return false // a reopened stream or a dump sends the records of its first millisecond again
	}
	if rec.time.After(c.last) {
		c.last = rec.time
	}
	t := rec.time
	if rec.lid == lidEvents {
		c.event(rec, t)
		return true
	}
	c.text(rec, t)
	name, ok := c.names[rec.pid]
	if !ok {
		c.unnamed = true
	}
	e := api.LogEntry{
		Time: t, Level: logLevel(rec.prio), Pid: rec.pid, Tid: rec.tid, Process: name,
		Tag: c.intern(rec.tag), Message: api.CutText(rec.msg, api.MaxLogMessage),
	}
	c.seq++
	r := logRec{seq: c.seq, uid: rec.uid, e: e}
	if c.userApp(rec.uid) {
		c.apps.push(r)
	} else {
		c.other.push(r)
	}
	if c.unnamed && !c.psBusy && time.Since(c.lastPS) > psEvery {
		c.psBusy = true
		go c.refreshNames()
	}
	return true
}

// maxNames bounds the pid -> process name map: every shell command adds a process, and pids
// are only reused after many of them. When full it is emptied; names come back from the next
// process list and start events, and lines keep the names they were taken with.
const maxNames = 20000

// setName records a process's name (the caller holds c.mu).
func (c *logCapture) setName(pid int, name string) {
	if len(c.names) >= maxNames {
		clear(c.names)
	}
	c.names[pid] = name
}

// intern shares tag strings between lines (a few hundred tags log most lines).
func (c *logCapture) intern(tag string) string {
	if s, ok := c.tags[tag]; ok {
		return s
	}
	if len(c.tags) > 4096 {
		clear(c.tags)
	}
	c.tags[tag] = tag
	return tag
}

// refreshNames reads the process list for processes the stream has not named (native daemons
// and shell commands started after the capture's start; apps are named by their start event).
func (c *logCapture) refreshNames() {
	ctx, cancel := context.WithTimeout(c.ctx, 5*time.Second)
	defer cancel()
	out, err := c.shell(ctx, "ps -A -o PID,UID,NAME")
	c.mu.Lock()
	defer c.mu.Unlock()
	c.psBusy, c.lastPS, c.unnamed = false, time.Now(), false
	if err != nil {
		return
	}
	for pid, p := range parsePS(out) {
		if _, ok := c.names[pid]; !ok {
			c.setName(pid, p.name)
		}
	}
}

// isAppUID: an app's uid (app ids 10000-19999 of any user); isolated and SDK sandbox uids are not.
func isAppUID(uid int) bool { id := uid % 100000; return uid >= 0 && id >= 10000 && id <= 19999 }

// userApp: the uid of an app that is not a system package (installed by the user or agemu).
func (c *logCapture) userApp(uid int) bool { return isAppUID(uid) && !c.sysApp[uid%100000] }

// parsePackages reads `cmd package list packages -U` ("package:com.x uid:10123", shared uids
// as "uid:10123,10124" are not expected for apps).
func parsePackages(out string) map[string]int {
	m := map[string]int{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || !strings.HasPrefix(f[0], "package:") || !strings.HasPrefix(f[1], "uid:") {
			continue
		}
		u, _, _ := strings.Cut(strings.TrimPrefix(f[1], "uid:"), ",")
		if uid, err := strconv.Atoi(u); err == nil {
			m[strings.TrimPrefix(f[0], "package:")] = uid
		}
	}
	return m
}

// parsePS reads `ps -A -o PID,UID,NAME`. UID is a number or a name (u0_a229, system, root).
func parsePS(out string) map[int]procInfo {
	m := map[int]procInfo{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		pid, err := strconv.Atoi(f[0])
		if err != nil {
			continue
		}
		m[pid] = procInfo{uid: parseUID(f[1]), name: strings.Join(f[2:], " ")}
	}
	return m
}

// parseUID reads a uid as ps prints it: a number, or u<user>_a<n> (app), u<user>_i<n>
// (isolated), or a system name (-1: not an app).
func parseUID(s string) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	var user, n int
	if _, err := fmt.Sscanf(s, "u%d_a%d", &user, &n); err == nil {
		return user*100000 + 10000 + n
	}
	if _, err := fmt.Sscanf(s, "u%d_i%d", &user, &n); err == nil {
		return user*100000 + 90000 + n
	}
	return -1
}

// pkgOf is the package of a process name ("com.x:remote" -> "com.x").
func pkgOf(process string) string {
	p, _, _ := strings.Cut(process, ":")
	return p
}

func firstLine(s string) string { l, _, _ := strings.Cut(s, "\n"); return l }

// ------------------------------------------------------------------ queries

// packageName is what an Android package name can hold (it goes into a shell command).
var packageName = regexp.MustCompile(`^[A-Za-z0-9_.]+$`)

// appUID resolves a package to its uid: system packages and apps whose process started are
// known; others are asked from the package manager once (the caller holds c.mu, which this
// releases around the call).
func (c *logCapture) appUID(ctx context.Context, pkg string) (int, bool) {
	if uid, ok := c.pkgUID[pkg]; ok {
		return uid, uid >= 0
	}
	if !packageName.MatchString(pkg) {
		return 0, false // not a package name: never put into the guest's shell
	}
	c.mu.Unlock()
	qctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	out, err := c.shell(qctx, "cmd package list packages -U "+pkg)
	cancel()
	c.mu.Lock()
	if err != nil {
		return 0, false
	}
	uid, ok := parsePackages(out)[pkg]
	if !ok {
		c.pkgUID[pkg] = -1 // not installed: match by process name
		return 0, false
	}
	c.pkgUID[pkg] = uid
	return uid, true
}

// belongs reports whether a process (uid, name) is one of pkg's: the package's app uid when
// known (all its processes), else its process names (pkg, pkg:*). A package that shares a
// system uid (Settings runs as system, like system_server) is told apart by name too.
func belongs(pkg string, uid int, haveUID bool, procUID int, name string) bool {
	if haveUID && isAppUID(uid) {
		return procUID == uid
	}
	return name == pkg || strings.HasPrefix(name, pkg+":")
}

// Logs returns the captured lines of q.App's processes (all of them: the package's uid), or of
// every process, from q.Since and after the cursor q.After, oldest first.
func (b *Backend) Logs(ctx context.Context, id string, q api.LogQuery) (api.LogBatch, error) {
	d, err := b.devFor(id)
	if err != nil {
		return api.LogBatch{}, err
	}
	c, err := d.capture(ctx)
	if err != nil {
		return api.LogBatch{}, err
	}
	return c.query(ctx, q), nil
}

func (c *logCapture) query(ctx context.Context, q api.LogQuery) api.LogBatch {
	c.mu.Lock()
	defer c.mu.Unlock()
	var uid int
	var haveUID bool
	if q.App != "" {
		uid, haveUID = c.appUID(ctx, q.App)
	}
	minRank := q.Level.Rank()
	var since time.Time // in the guest's time, as the lines are kept
	if !q.Since.IsZero() {
		since = c.toGuest(q.Since)
	}
	keep := func(r *logRec) bool {
		if r.seq <= q.After || (!since.IsZero() && r.e.Time.Before(since)) {
			return false
		}
		if minRank > 0 && r.e.Level.Rank() < minRank {
			return false
		}
		if q.App != "" {
			name := r.e.Process
			if name == "" {
				name = c.names[r.e.Pid]
			}
			if !belongs(q.App, uid, haveUID, r.uid, name) {
				return false
			}
		}
		return true
	}
	// Both rings are in sequence order: merge them.
	a, o := c.apps.recs, c.other.recs
	var out []api.LogEntry
	for len(a) > 0 || len(o) > 0 {
		var r *logRec
		if len(o) == 0 || (len(a) > 0 && a[0].seq < o[0].seq) {
			r, a = &a[0], a[1:]
		} else {
			r, o = &o[0], o[1:]
		}
		if keep(r) {
			e := r.e
			e.Time = c.toMac(e.Time)
			if e.Process == "" {
				e.Process = c.names[e.Pid]
			}
			out = append(out, e)
		}
	}
	// What the capture holds for these lines: an app's lines are all in the apps ring, except a
	// system app's; the rest are in the other ring.
	dropSeq, dropAt := c.other.dropSeq, c.other.dropAt
	if q.App != "" && haveUID && c.userApp(uid) {
		dropSeq, dropAt = c.apps.dropSeq, c.apps.dropAt
	} else if c.apps.dropSeq > dropSeq {
		dropSeq, dropAt = c.apps.dropSeq, c.apps.dropAt
	}
	from := c.from
	if dropSeq > 0 {
		from = c.toMac(dropAt)
	}
	batch := api.LogBatch{Entries: out, Cursor: c.seq, CapturedFrom: from}
	switch {
	case q.After > 0:
		// A cursor of an earlier capture (before a reset or a daemon restart) or one whose next
		// lines were dropped.
		batch.Partial = q.After < c.first || dropSeq > q.After
	case !q.Since.IsZero():
		batch.Partial = from.IsZero() || q.Since.Before(from)
	default:
		batch.Partial = dropSeq > 0
	}
	return batch
}
