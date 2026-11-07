package android

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/api"
)

// Times of the tests: the guest's clock runs a second behind the Mac's, as after a snapshot
// load on emulator 37.1 (measured 1.00-1.04 s).
var (
	guest0      = time.Unix(1794034991, 0)
	guestOffset = time.Second
)

const (
	probeUID = 10229
	gmsUID   = 10147 // a system package's app uid (Finsky on the device)
)

// testCapture is a ready capture with known packages and processes, fed records directly. Any
// guest command fails the test unless the test replaces shell.
func testCapture(t *testing.T) *logCapture {
	t.Helper()
	c := newLogCapture("agemu-test-none", "emulator-0", time.Time{}, 1000)
	c.clock.observe(guest0.Add(guestOffset), guest0) // records below are added without an arrival time
	c.sysApp[gmsUID] = true
	c.pkgUID["com.android.vending"] = gmsUID
	c.pkgUID["dev.agemu.probe"] = probeUID
	c.pkgUID["com.android.settings"] = 1000
	c.names = map[int]string{702: "system_server", 5267: "dev.agemu.probe", 5494: "dev.agemu.probe:remote", 4477: "com.android.vending", 1500: "com.android.settings"}
	c.w.procs[5267] = procInfo{uid: probeUID, name: "dev.agemu.probe"}
	c.w.procs[5494] = procInfo{uid: probeUID, name: "dev.agemu.probe:remote"}
	c.lastPS = time.Now().Add(time.Hour) // no process list reads
	c.shell = func(ctx context.Context, cmd string) (string, error) {
		t.Errorf("unexpected guest command %q", cmd)
		return "", fmt.Errorf("no device")
	}
	close(c.ready)
	t.Cleanup(c.cancel)
	return c
}

// line is a text record at guest0 + ms.
func line(lid uint32, pid, uid int, ms int, prio byte, tag, msg string) logRecord {
	return logRecord{lid: lid, pid: pid, tid: pid, uid: uid, time: guest0.Add(time.Duration(ms) * time.Millisecond), prio: prio, tag: tag, msg: msg}
}

// event is an events record from system_server at guest0 + ms.
func event(ms int, tag int32, vals ...any) logRecord {
	var b bytes.Buffer
	encEventValue(&b, vals)
	v, _ := parseEventValue(b.Bytes())
	return logRecord{lid: lidEvents, pid: 702, tid: 702, uid: 1000, time: guest0.Add(time.Duration(ms) * time.Millisecond), event: tag, data: v}
}

func messages(es []api.LogEntry) []string {
	var out []string
	for _, e := range es {
		out = append(out, e.Message)
	}
	return out
}

func TestCaptureCorrectsTheGuestClock(t *testing.T) {
	c := testCapture(t)
	c.add(line(lidMain, 5267, probeUID, 500, 4, "AgemuLab", "hello"), time.Time{})
	b := c.query(context.Background(), api.LogQuery{})
	if len(b.Entries) != 1 {
		t.Fatalf("entries %v", b.Entries)
	}
	want := guest0.Add(500*time.Millisecond + guestOffset)
	if e := b.Entries[0]; !e.Time.Equal(want) || e.Level != api.LogInfo || e.Process != "dev.agemu.probe" || e.Tag != "AgemuLab" || e.Pid != 5267 || e.Tid != 5267 {
		t.Fatalf("entry %+v, want time %v", e, want)
	}
	// A query's since is the Mac's time: a line 0.5 s after guest0 is 1.5 s after it on the Mac.
	if got := c.query(context.Background(), api.LogQuery{Since: guest0.Add(1400 * time.Millisecond)}); len(got.Entries) != 1 {
		t.Errorf("since before the corrected time: %d entries", len(got.Entries))
	}
	if got := c.query(context.Background(), api.LogQuery{Since: guest0.Add(1600 * time.Millisecond)}); len(got.Entries) != 0 {
		t.Errorf("since after the corrected time: %d entries", len(got.Entries))
	}
}

func TestCaptureAppFilter(t *testing.T) {
	c := testCapture(t)
	c.add(line(lidMain, 5267, probeUID, 1, 4, "AgemuLab", "main process"), time.Time{})
	c.add(line(lidSystem, 702, 1000, 2, 4, "ActivityManager", "system_server"), time.Time{})
	c.add(line(lidMain, 5494, probeUID, 3, 4, "AgemuLabRemote", "remote process"), time.Time{})
	c.add(line(lidMain, 4477, gmsUID, 4, 4, "Finsky", "a system app"), time.Time{})
	c.add(line(lidMain, 1500, 1000, 5, 4, "Settings", "settings runs as system"), time.Time{})
	c.add(line(lidCrash, 5267, probeUID, 6, 6, "AndroidRuntime", "crash buffer"), time.Time{})
	ctx := context.Background()
	cases := map[string][]string{
		"dev.agemu.probe":      {"main process", "remote process", "crash buffer"},
		"com.android.vending":  {"a system app"},
		"com.android.settings": {"settings runs as system"}, // a shared system uid: by process name
		"":                     {"main process", "system_server", "remote process", "a system app", "settings runs as system", "crash buffer"},
	}
	for app, want := range cases {
		got := messages(c.query(ctx, api.LogQuery{App: app}).Entries)
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("app %q: %q, want %q", app, got, want)
		}
	}
}

func TestCaptureAsksForUnknownPackagesOnce(t *testing.T) {
	c := testCapture(t)
	asked := 0
	c.shell = func(ctx context.Context, cmd string) (string, error) {
		asked++
		if cmd != "cmd package list packages -U com.example.new" {
			t.Errorf("command %q", cmd)
		}
		return "package:com.example.new uid:10300\npackage:com.example.newer uid:10301\n", nil
	}
	c.add(line(lidMain, 6000, 10300, 1, 4, "New", "new app"), time.Time{})
	c.add(line(lidMain, 6001, 10301, 2, 4, "Newer", "another app"), time.Time{})
	for i := 0; i < 2; i++ {
		if got := messages(c.query(context.Background(), api.LogQuery{App: "com.example.new"}).Entries); len(got) != 1 || got[0] != "new app" {
			t.Fatalf("query %d: %q", i, got)
		}
	}
	if asked != 1 {
		t.Errorf("asked the package manager %d times", asked)
	}
	// A package that is not installed: its processes by name, without asking again.
	asked = 0
	c.shell = func(ctx context.Context, cmd string) (string, error) { asked++; return "", nil }
	c.names[6002] = "com.example.gone:svc"
	c.add(line(lidMain, 6002, 10302, 3, 4, "Gone", "uninstalled app"), time.Time{})
	for i := 0; i < 2; i++ {
		if got := messages(c.query(context.Background(), api.LogQuery{App: "com.example.gone"}).Entries); len(got) != 1 {
			t.Fatalf("query %d: %q", i, got)
		}
	}
	if asked != 1 {
		t.Errorf("asked %d times for a missing package", asked)
	}
}

func TestCaptureRingsKeepAppLines(t *testing.T) {
	c := testCapture(t)
	c.other.max = 4 << 10
	c.add(line(lidMain, 5267, probeUID, 0, 4, "AgemuLab", "app line"), time.Time{})
	for i := 0; i < 200; i++ {
		c.add(line(lidMain, 702, 1000, 1+i, 4, "Chatty", fmt.Sprintf("system line %d", i)), time.Time{})
	}
	if c.other.bytes > c.other.max {
		t.Fatalf("other ring holds %d bytes, more than %d", c.other.bytes, c.other.max)
	}
	ctx := context.Background()
	app := c.query(ctx, api.LogQuery{App: "dev.agemu.probe"})
	if got := messages(app.Entries); len(got) != 1 || got[0] != "app line" {
		t.Fatalf("app lines %q", got)
	}
	if app.Partial {
		t.Error("app query partial although no app line was dropped")
	}
	all := c.query(ctx, api.LogQuery{})
	got := messages(all.Entries)
	if got[0] != "app line" || got[len(got)-1] != "system line 199" {
		t.Errorf("whole device: first %q last %q", got[0], got[len(got)-1])
	}
	// The oldest system lines went first.
	if got[1] == "system line 0" {
		t.Error("the oldest system line was kept")
	}
	if !all.Partial || !all.CapturedFrom.After(guest0.Add(guestOffset)) {
		t.Errorf("whole device: partial %v, captured from %v", all.Partial, all.CapturedFrom)
	}
	if !c.query(ctx, api.LogQuery{Since: guest0}).Partial {
		t.Error("a since before the dropped lines is not partial")
	}
}

func TestLogRingDropsOldestFirst(t *testing.T) {
	r := logRing{max: 3 * (64 + 1)}
	for i := 1; i <= 5; i++ {
		r.push(logRec{seq: uint64(i), e: api.LogEntry{Message: "x", Time: guest0.Add(time.Duration(i))}})
	}
	if len(r.recs) != 3 || r.recs[0].seq != 3 || r.dropSeq != 2 || !r.dropAt.Equal(guest0.Add(2)) {
		t.Fatalf("ring %+v", r)
	}
	// One line larger than the ring stays (the newest is never dropped).
	r.push(logRec{seq: 6, e: api.LogEntry{Message: strings.Repeat("y", 1000)}})
	if len(r.recs) != 1 || r.recs[0].seq != 6 {
		t.Fatalf("after a big line: %+v", r.recs)
	}
}

func TestCaptureCursor(t *testing.T) {
	c := testCapture(t)
	ctx := context.Background()
	empty := c.query(ctx, api.LogQuery{})
	if empty.Cursor != 1000 {
		t.Fatalf("cursor of an empty capture %d, want its start", empty.Cursor)
	}
	c.add(line(lidMain, 5267, probeUID, 1, 4, "A", "one"), time.Time{})
	c.add(line(lidMain, 702, 1000, 2, 4, "B", "two"), time.Time{})
	cur := c.query(ctx, api.LogQuery{}).Cursor
	c.add(line(lidMain, 5267, probeUID, 3, 4, "A", "three"), time.Time{})
	b := c.query(ctx, api.LogQuery{After: cur})
	if got := messages(b.Entries); len(got) != 1 || got[0] != "three" || b.Partial {
		t.Fatalf("after the cursor: %q partial %v", got, b.Partial)
	}
	if b.Cursor != cur+1 {
		t.Errorf("cursor %d, want %d", b.Cursor, cur+1)
	}
	// A cursor of an earlier capture: everything held, but the gap before it is unknown.
	old := c.query(ctx, api.LogQuery{After: 500})
	if len(old.Entries) != 3 || !old.Partial {
		t.Errorf("earlier capture's cursor: %d entries, partial %v", len(old.Entries), old.Partial)
	}
	// Lines after the cursor that the ring dropped make the answer partial.
	c.other.max = 200
	for i := 0; i < 5; i++ {
		c.add(line(lidMain, 702, 1000, 10+i, 4, "B", "more"), time.Time{})
	}
	if !c.query(ctx, api.LogQuery{After: cur}).Partial {
		t.Error("dropped lines after the cursor: not partial")
	}
}

func TestCaptureLevelAndMessageBounds(t *testing.T) {
	c := testCapture(t)
	c.add(line(lidMain, 5267, probeUID, 1, 3, "A", "debug"), time.Time{})
	c.add(line(lidMain, 5267, probeUID, 2, 5, "A", "warn"), time.Time{})
	c.add(line(lidMain, 5267, probeUID, 3, 1, "A", "default priority"), time.Time{})
	c.add(line(lidMain, 5267, probeUID, 4, 6, "A", strings.Repeat("e", api.MaxLogMessage+100)), time.Time{})
	b := c.query(context.Background(), api.LogQuery{Level: api.LogWarn})
	if len(b.Entries) != 2 || b.Entries[0].Message != "warn" {
		t.Fatalf("level warn: %q", messages(b.Entries))
	}
	if m := b.Entries[1].Message; len(m) != api.MaxLogMessage || !strings.HasSuffix(m, "bytes cut]") {
		t.Errorf("long message: %d bytes, ends %q", len(m), m[len(m)-20:])
	}
	if got := c.query(context.Background(), api.LogQuery{}); len(got.Entries) != 4 {
		t.Errorf("without a level: %d entries", len(got.Entries))
	}
}

func TestCaptureSkipsRepeatedRecords(t *testing.T) {
	c := testCapture(t)
	a := line(lidMain, 5267, probeUID, 1, 4, "A", "a")
	b := line(lidMain, 5267, probeUID, 1, 4, "A", "bb") // same time, another record
	c.add(a, time.Time{})
	c.add(b, time.Time{})
	c.add(a, time.Time{}) // a reopened stream starts at the newest millisecond again
	c.add(b, time.Time{})
	c.add(line(lidMain, 5267, probeUID, 2, 4, "A", "c"), time.Time{})
	if got := messages(c.query(context.Background(), api.LogQuery{}).Entries); strings.Join(got, ",") != "a,bb,c" {
		t.Fatalf("got %q", got)
	}
}

func TestCaptureNamesProcesses(t *testing.T) {
	c := testCapture(t)
	ps := make(chan struct{})
	c.lastPS = time.Time{}
	c.shell = func(ctx context.Context, cmd string) (string, error) {
		defer close(ps)
		if cmd != "ps -A -o PID,UID,NAME" {
			t.Errorf("command %q", cmd)
		}
		return "PID UID NAME\n9000 shell sh\n9001 u0_a229 dev.agemu.probe:other\n", nil
	}
	c.add(line(lidMain, 9000, 2000, 1, 4, "sh", "unnamed for now"), time.Time{})
	select {
	case <-ps:
	case <-time.After(5 * time.Second):
		t.Fatal("no process list read for an unnamed process")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if e := c.query(context.Background(), api.LogQuery{}).Entries; len(e) == 1 && e[0].Process == "sh" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the line's process was not named after the process list")
		}
		time.Sleep(time.Millisecond)
	}
	// An app process is named by its start event.
	c.add(event(5, evAmProcStart, 0, 9100, probeUID, "dev.agemu.probe:sync", "service", "{x}"), time.Time{})
	c.add(line(lidMain, 9100, probeUID, 6, 4, "Sync", "named"), time.Time{})
	if e := c.query(context.Background(), api.LogQuery{App: "dev.agemu.probe"}).Entries; len(e) != 1 || e[0].Process != "dev.agemu.probe:sync" {
		t.Fatalf("entries %+v", e)
	}
}

func TestParsePSAndPackages(t *testing.T) {
	ps := parsePS("PID UID NAME\n1 root init\n702 system system_server\n5267 u0_a229 dev.agemu.probe\n5300 u10_i5 com.android.chrome:sandboxed_process0\nbad line\n")
	if ps[1].uid != -1 || ps[702].uid != -1 || ps[5267] != (procInfo{uid: 10229, name: "dev.agemu.probe"}) || ps[5300].uid != 1090005 {
		t.Fatalf("ps %+v", ps)
	}
	if len(ps) != 4 {
		t.Errorf("%d processes", len(ps))
	}
	if parseUID("10229") != 10229 || parseUID("nobody") != -1 {
		t.Error("parseUID")
	}
	pk := parsePackages("package:com.android.settings uid:1000\npackage:dev.agemu.probe uid:10229\npackage:shared uid:10300,10301\ngarbage\n")
	if pk["com.android.settings"] != 1000 || pk["dev.agemu.probe"] != 10229 || pk["shared"] != 10300 || len(pk) != 3 {
		t.Fatalf("packages %v", pk)
	}
	if !isAppUID(10229) || !isAppUID(1010229) || isAppUID(1000) || isAppUID(90005) || isAppUID(-1) {
		t.Error("isAppUID")
	}
}

func TestMeasureOffsetIsAnUpperBound(t *testing.T) {
	c := newLogCapture("agemu-test-none", "emulator-0", time.Time{}, 1)
	defer c.cancel()
	call := 0
	c.shell = func(ctx context.Context, cmd string) (string, error) {
		call++
		// The guest's clock is a second behind; the reply takes `after` to come back.
		after := 40 * time.Millisecond
		if call == 2 {
			after = 2 * time.Millisecond
		}
		g := time.Now().Add(-time.Second)
		time.Sleep(after)
		return fmt.Sprintf("%d\n", g.UnixNano()), nil
	}
	if err := c.measureOffset(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Never below the true offset (a converted time is never early), and as tight as the
	// quickest reply.
	if off := c.clock.offset(); off < time.Second || off > time.Second+15*time.Millisecond {
		t.Fatalf("offset %v, want 1s to 1.015s", off)
	}
}

func TestGuestClock(t *testing.T) {
	var g guestClock
	if g.offset() != 0 {
		t.Fatal("offset before any observation")
	}
	mac := time.Unix(1794034991, 0)
	g.observe(mac, mac.Add(-1020*time.Millisecond)) // a date reply: 20 ms of round trip in it
	g.observe(mac.Add(time.Second), mac.Add(time.Second-1002*time.Millisecond))
	g.observe(mac.Add(2*time.Second), mac.Add(-5*time.Second)) // a backfilled record: no news
	if g.offset() != 1002*time.Millisecond {
		t.Fatalf("offset %v, want the tightest bound 1.002s", g.offset())
	}
	// The guest's clock falls 30 ms further behind (drift): after two windows the old bound is
	// gone and the estimate is a bound again.
	for i := 1; i <= 3; i++ {
		at := mac.Add(time.Duration(i) * (clockWindow + time.Second))
		g.observe(at, at.Add(-1033*time.Millisecond))
	}
	if g.offset() != 1033*time.Millisecond {
		t.Fatalf("after drift: %v", g.offset())
	}
}

func TestCaptureTightensTheOffsetFromLiveRecords(t *testing.T) {
	c := testCapture(t)
	c.clock = guestClock{}
	now := time.Now()
	c.clock.observe(now, now.Add(-1020*time.Millisecond)) // the date replies' bound
	// A record that came through the stream 2 ms after the guest stamped it.
	rec := line(lidMain, 5267, probeUID, 0, 4, "AgemuLab", "live")
	rec.time = now.Add(time.Second - 1002*time.Millisecond)
	c.add(rec, now.Add(time.Second))
	if off := c.clock.offset(); off != 1002*time.Millisecond {
		t.Fatalf("offset %v", off)
	}
	e := c.query(context.Background(), api.LogQuery{}).Entries
	if len(e) != 1 || !e[0].Time.Equal(rec.time.Add(1002*time.Millisecond)) {
		t.Fatalf("entry time %v", e)
	}
}

// fakeDevice answers a capture's guest commands and streams as a device would.
type fakeDevice struct {
	t        *testing.T
	guestNow func() time.Time
	mu       sync.Mutex
	opened   []string
	streams  [][]byte // returned in turn by open; then open fails
}

func (f *fakeDevice) shell(ctx context.Context, cmd string) (string, error) {
	switch {
	case cmd == "date +%s%N":
		return fmt.Sprintf("%d\n", f.guestNow().UnixNano()), nil
	case strings.HasPrefix(cmd, "getprop persist.sys.timezone"):
		return fmt.Sprintf("Europe/Warsaw\n--\npackage:com.android.vending uid:%d\npackage:android uid:1000\n--\nPID UID NAME\n702 system system_server\n5267 u0_a229 dev.agemu.probe\n", gmsUID), nil
	}
	f.t.Errorf("unexpected guest command %q", cmd)
	return "", fmt.Errorf("unexpected")
}

func (f *fakeDevice) open(ctx context.Context, cmd string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opened = append(f.opened, cmd)
	if len(f.streams) == 0 {
		return nil, fmt.Errorf("device offline")
	}
	s := f.streams[0]
	f.streams = f.streams[1:]
	return io.NopCloser(bytes.NewReader(s)), nil
}

func TestCaptureRunBackfillsAndFollows(t *testing.T) {
	floor := time.Now().Add(-time.Minute) // the last snapshot load, on the Mac's clock
	g := floor.Add(-guestOffset).Add(time.Second)
	rec := func(ms int, pid, uid int, tag, msg string) []byte {
		return encRecord(lidMain, pid, pid, uid, g.Add(time.Duration(ms)*time.Millisecond), textPayload(4, tag, msg))
	}
	backfill := append(rec(0, 5267, probeUID, "AgemuLab", "launch line"), rec(1, 702, 1000, "AM", "system")...)
	// The reopened stream repeats the newest record, then goes on.
	stream := append(rec(1, 702, 1000, "AM", "system"), rec(2, 5267, probeUID, "AgemuLab", "live line")...)
	f := &fakeDevice{t: t, guestNow: func() time.Time { return time.Now().Add(-guestOffset) }, streams: [][]byte{backfill, stream}}
	c := newLogCapture("agemu-test-none", "emulator-0", floor, 42)
	c.shell, c.open = f.shell, f.open
	go c.run()
	select {
	case <-c.done: // the second stream ends, the device is not running: the capture ends
	case <-time.After(10 * time.Second):
		c.cancel()
		t.Fatal("the capture did not end")
	}
	if c.err != nil {
		t.Fatal(c.err)
	}
	wantT := "logcat -B -d -b main,system,crash,events -T " + guestTimeArg(floor.Add(-c.clock.offset()))
	if len(f.opened) < 2 || f.opened[0] != wantT || !strings.HasPrefix(f.opened[1], "logcat -B -b main,system,crash,events -T ") {
		t.Fatalf("opened %q, want first %q", f.opened, wantT)
	}
	if off := c.clock.offset(); off < guestOffset || off > guestOffset+20*time.Millisecond {
		t.Errorf("offset %v", off)
	}
	if c.loc.String() != "Europe/Warsaw" || !c.sysApp[gmsUID] || c.names[5267] != "dev.agemu.probe" || c.w.procs[5267].uid != probeUID {
		t.Errorf("setup: loc %v sys %v names %v procs %v", c.loc, c.sysApp, c.names, c.w.procs)
	}
	b := c.query(context.Background(), api.LogQuery{})
	if got := messages(b.Entries); strings.Join(got, ",") != "launch line,system,live line" {
		t.Fatalf("entries %q", got)
	}
	if b.Cursor != 45 || !b.CapturedFrom.Equal(floor) || b.Partial {
		t.Errorf("cursor %d captured from %v (want %v) partial %v", b.Cursor, b.CapturedFrom, floor, b.Partial)
	}
	if app := c.query(context.Background(), api.LogQuery{App: "dev.agemu.probe"}); len(app.Entries) != 2 {
		t.Errorf("app entries %d", len(app.Entries))
	}
}

func TestCaptureSwitchesToDumpsWhileBusy(t *testing.T) {
	floor := time.Now().Add(-time.Minute)
	g := floor.Add(-guestOffset).Add(time.Second)
	recs := func(from, n int) []byte {
		var b []byte
		for i := from; i < from+n; i++ {
			b = append(b, encRecord(lidMain, 5267, 5267, probeUID, g.Add(time.Duration(i)*time.Millisecond), textPayload(4, "AgemuLab", fmt.Sprintf("spam %d", i)))...)
		}
		return b
	}
	// The stream's records arrive 4 ms apart (250 a second): the third busy second passes
	// busyRate at its 101st record, the 601st of the stream.
	const cut = 2*250 + busyRate + 1
	f := &fakeDevice{t: t, guestNow: func() time.Time { return time.Now().Add(-guestOffset) }, streams: [][]byte{
		nil,              // the backfill: nothing
		recs(0, cut+100), // the live stream: a sustained busy rate
		recs(cut-1, 60),  // a dump: still busy (it repeats what the stream had taken)
		recs(cut+49, 15), // a dump with 5 new records after 10 it had sent: quiet again
		nil,              // the live stream again; it ends, the device is not running
	}}
	c := newLogCapture("agemu-test-none", "emulator-0", floor, 1)
	c.shell, c.open = f.shell, f.open
	fake := time.Now()
	c.tick = func() time.Time { fake = fake.Add(4 * time.Millisecond); return fake }
	t0 := time.Now()
	go c.run()
	select {
	case <-c.done:
	case <-time.After(10 * time.Second):
		c.cancel()
		t.Fatal("the capture did not end")
	}
	var modes []string
	for _, cmd := range f.opened {
		if strings.Contains(cmd, " -d ") {
			modes = append(modes, "dump")
		} else {
			modes = append(modes, "stream")
		}
	}
	if strings.Join(modes, ",") != "dump,stream,dump,dump,stream" {
		t.Fatalf("modes %v (%q)", modes, f.opened)
	}
	// One dump period between the two dumps.
	if d := time.Since(t0); d < pollEvery {
		t.Errorf("took %v, less than one dump period", d)
	}
	if got, want := len(c.query(context.Background(), api.LogQuery{}).Entries), cut+64; got != want {
		t.Errorf("%d lines, want %d (each once)", got, want)
	}
}

// A burst (a cold launch logs over busyRate records in its first second) keeps the stream; only
// busySecs busy seconds in a row switch to dumps.
func TestBusyMeterNeedsASustainedRate(t *testing.T) {
	t0 := time.Now()
	run := func(perSec []int) (switchedIn int) {
		var m busyMeter
		for s, n := range perSec {
			for i := 0; i < n; i++ {
				if m.add(t0.Add(time.Duration(s)*time.Second + time.Duration(i)*time.Second/time.Duration(n+1))) {
					return s + 1
				}
			}
		}
		return 0
	}
	cases := []struct {
		name   string
		perSec []int
		want   int
	}{
		{"a launch burst", []int{400, 30, 10, 5}, 0},
		{"two busy seconds", []int{300, 300, 20, 300, 300, 20}, 0},
		{"just under the rate", []int{busyRate, busyRate, busyRate, busyRate}, 0},
		{"a sustained rate", []int{500, 500, 500, 500}, busySecs},
		{"busy after a quiet start", []int{5, 200, 200, 200}, 1 + busySecs},
	}
	for _, c := range cases {
		if got := run(c.perSec); got != c.want {
			t.Errorf("%s: switched in second %d, want %d (0: never)", c.name, got, c.want)
		}
	}
}

func TestBackfillFrom(t *testing.T) {
	now := time.Unix(1794034991, 0)
	recent := now.Add(-time.Minute)
	if got := backfillFrom(recent, now); !got.Equal(recent) {
		t.Errorf("a load a minute ago: %v", got)
	}
	for _, floor := range []time.Time{{}, now.Add(-time.Hour)} {
		if got := backfillFrom(floor, now); !got.Equal(now.Add(-maxBackfill)) {
			t.Errorf("floor %v: %v, want at most %v back", floor, got, maxBackfill)
		}
	}
}

func TestCaptureFloor(t *testing.T) {
	t.Setenv("AGEMU_HOME", t.TempDir())
	t.Setenv("ANDROID_AVD_HOME", t.TempDir())
	if err := os.MkdirAll(logDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	d := &device{name: "agemu-floor", run: runningEmu{PID: 4321}}
	if !d.captureFloor().IsZero() {
		t.Fatal("a floor without a load or a baseline")
	}
	at := time.Unix(1794034000, 5)
	d.noteLoad(at)
	if got := d.captureFloor(); !got.Equal(at) {
		t.Fatalf("after a load: %v", got)
	}
	// Another daemon: the load recorded for this emulator process.
	d2 := &device{name: "agemu-floor", run: runningEmu{PID: 4321}}
	if got := d2.captureFloor(); !got.Equal(at) {
		t.Fatalf("recorded load: %v", got)
	}
	// Another emulator process: not its load; the baseline snapshot's time instead.
	d3 := &device{name: "agemu-floor", run: runningEmu{PID: 999}}
	snap := filepath.Join(os.Getenv("ANDROID_AVD_HOME"), "agemu-floor.avd", "snapshots", baselineSnapshot)
	if err := os.MkdirAll(snap, 0o755); err != nil {
		t.Fatal(err)
	}
	avd := filepath.Dir(filepath.Dir(snap))
	os.WriteFile(filepath.Join(os.Getenv("ANDROID_AVD_HOME"), "agemu-floor.ini"), []byte("path="+avd+"\n"), 0o644)
	os.WriteFile(filepath.Join(avd, "config.ini"), []byte("hw.ramSize=2048\n"), 0o644)
	os.WriteFile(filepath.Join(snap, "snapshot.pb"), nil, 0o644)
	saved := time.Unix(1794000000, 0)
	os.Chtimes(filepath.Join(snap, "snapshot.pb"), saved, saved)
	if got := d3.captureFloor(); !got.Equal(saved) {
		t.Fatalf("baseline floor %v, want %v", got, saved)
	}
}

func TestStopCaptureKeepsCursorsGrowing(t *testing.T) {
	// The new capture's goroutine must not reach a real adb server or emulator.
	t.Setenv("ANDROID_ADB_SERVER_PORT", "1")
	d := &device{name: "agemu-test-none", run: runningEmu{PID: os.Getpid(), Console: 1}}
	t.Setenv("AGEMU_HOME", t.TempDir())
	c := newLogCapture(d.name, "emulator-0", time.Time{}, uint64(time.Now().Add(time.Hour).UnixMicro()))
	c.seq += 10
	d.logs = c
	d.stopCapture()
	if d.logs != nil || c.ctx.Err() == nil {
		t.Fatal("capture not stopped")
	}
	if d.lastSeq != c.seq {
		t.Fatalf("last seq %d, want %d", d.lastSeq, c.seq)
	}
	// The next capture starts above it even though the clock is behind that cursor.
	t.Setenv("AGEMU_ANDROID_LOGS", "1")
	n := d.startCapture()
	defer n.cancel()
	if n.first <= c.seq {
		t.Fatalf("new capture starts at %d, not above %d", n.first, c.seq)
	}
}
