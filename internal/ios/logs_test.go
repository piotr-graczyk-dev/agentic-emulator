package ios

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/api"
)

const (
	testUDID = "151A1D97-95B5-4D1D-A495-E08DDE41A661"
	testData = "/Users/me/Library/Developer/CoreSimulator/Devices/" + testUDID + "/data"
	testApp  = testData + "/Containers/Bundle/Application/BEDADDA3-F548-4858-969F-EF8C2C30CE28/AgemuTestApp.app"
	testRT   = "/Library/Developer/CoreSimulator/Volumes/iOS_23F77/Library/Developer/CoreSimulator/Profiles/Runtimes/iOS 26.5.simruntime/Contents/Resources/RuntimeRoot"
)

func TestBundleKey(t *testing.T) {
	cases := map[string]string{
		testApp + "/AgemuTestApp":                "user/BEDADDA3-F548-4858-969F-EF8C2C30CE28/AgemuTestApp.app",
		testApp:                                  "user/BEDADDA3-F548-4858-969F-EF8C2C30CE28/AgemuTestApp.app",
		testApp + "/PlugIns/Widget.appex/Widget": "user/BEDADDA3-F548-4858-969F-EF8C2C30CE28/AgemuTestApp.app",
		testRT + "/Applications/MobileSafari.app/MobileSafari":                                             "system/Applications/MobileSafari.app",
		testRT + "/Applications/MobileSafari.app":                                                          "system/Applications/MobileSafari.app",
		testRT + "/Applications/MobileSafari.app/Extensions/SafariLinkExtension.appex/SafariLinkExtension": "system/Applications/MobileSafari.app",
		testRT + "/System/Library/CoreServices/SpringBoard.app/SpringBoard":                                "system/System/Library/CoreServices/SpringBoard.app",
		testRT + "/usr/libexec/runningboardd":                                                              "",
		// A runtime inside Xcode.app: Xcode's own bundle is not the process's.
		"/Applications/Xcode.app/Contents/Developer/Platforms/iPhoneOS.platform/Library/Developer/CoreSimulator/Profiles/Runtimes/iOS.simruntime/Contents/Resources/RuntimeRoot/usr/libexec/locationd": "",
		testData + "/Containers/Bundle/Application/X/NotAnApp/run": "",
	}
	for p, want := range cases {
		if got := bundleKey(p); got != want {
			t.Errorf("bundleKey(%q) = %q, want %q", p, got, want)
		}
	}
}

func TestLevelAndTag(t *testing.T) {
	levels := map[string]api.LogLevel{"Default": api.LogInfo, "Info": api.LogInfo, "Debug": api.LogDebug,
		"Error": api.LogError, "Fault": api.LogFatal, "": ""}
	for in, want := range levels {
		if got := logLevel(in); got != want {
			t.Errorf("logLevel(%q) = %q, want %q", in, got, want)
		}
	}
	tags := []struct {
		ev   streamEvent
		want string
	}{
		{streamEvent{Subsystem: "dev.agemu.testapp", Category: "probe"}, "dev.agemu.testapp:probe"},
		{streamEvent{Subsystem: "com.example"}, "com.example"},
		{streamEvent{SenderImagePath: testRT + "/System/Library/Frameworks/Foundation.framework/Foundation"}, "Foundation"},
	}
	for _, c := range tags {
		if got := logTag(&c.ev); got != c.want {
			t.Errorf("logTag(%+v) = %q, want %q", c.ev, got, c.want)
		}
	}
}

// line builds a `log stream --style ndjson` line.
func line(t time.Time, typ string, pid int, image, sender, subsystem, category, msg string) []byte {
	b, _ := json.Marshal(map[string]any{
		"eventType": "logEvent", "messageType": typ, "timestamp": t.Format(streamTimeLayout),
		"processID": pid, "threadID": 7, "processImagePath": image, "senderImagePath": sender,
		"subsystem": subsystem, "category": category, "eventMessage": msg,
		"backtrace": map[string]any{"frames": []any{map[string]any{"imageOffset": 1}}},
	})
	return b
}

// testCapture is a device capture with a running (fake) stream, fed by ingestLine.
func testCapture(t *testing.T) (*Backend, *deviceLogs, *logStream) {
	t.Helper()
	b := &Backend{logs: map[string]*deviceLogs{}, reports: &reportDir{seen: map[string]reportEntry{}}}
	b.bg, b.stopBg = context.WithCancel(context.Background())
	t.Cleanup(b.stopBg)
	dl := b.devLogs(testUDID)
	s := &logStream{done: make(chan struct{})}
	dl.stream, dl.backfill, dl.firstSeen = s, true, make(chan struct{})
	dl.seq = 1_000_000
	dl.startSeq = dl.seq + 1
	return b, dl, s
}

func ingestLine(t *testing.T, b *Backend, dl *deviceLogs, s *logStream, raw []byte) {
	t.Helper()
	var ev streamEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		t.Fatal(err)
	}
	b.ingest(dl, s, &ev)
}

var t0 = time.Date(2026, 11, 7, 6, 27, 0, 0, time.FixedZone("CET", 3600))

func TestIngestSplitsUserAppsFromTheRest(t *testing.T) {
	b, dl, s := testCapture(t)
	app := testApp + "/AgemuTestApp"
	sb := testRT + "/System/Library/CoreServices/SpringBoard.app/SpringBoard"
	ingestLine(t, b, dl, s, line(t0, "Default", 42, app, app, "dev.agemu.testapp", "probe", "probe launched"))
	ingestLine(t, b, dl, s, line(t0.Add(time.Millisecond), "Error", 9, sb, sb, "com.apple.SpringBoard", "Common", "oops"))
	ingestLine(t, b, dl, s, line(t0.Add(2*time.Millisecond), "Fault", 42, app, app, "", "", strings.Repeat("x", api.MaxLogMessage+100)))
	// Not log lines: an activity, and a line that does not parse.
	activity, _ := json.Marshal(map[string]any{"eventType": "activityCreateEvent", "timestamp": t0.Format(streamTimeLayout), "processID": 42})
	ingestLine(t, b, dl, s, activity)
	b.ingest(dl, s, &streamEvent{EventType: "logEvent", Timestamp: "yesterday"})

	if n := len(dl.user.live()); n != 2 {
		t.Fatalf("user ring holds %d lines, want 2", n)
	}
	if n := len(dl.rest.live()); n != 1 {
		t.Fatalf("rest ring holds %d lines, want 1", n)
	}
	first := dl.user.live()[0]
	want := api.LogEntry{Time: t0, Level: api.LogInfo, Pid: 42, Tid: 7, Process: "AgemuTestApp", Tag: "dev.agemu.testapp:probe", Message: "probe launched"}
	if !first.entry.Time.Equal(want.Time) || first.entry.Level != want.Level || first.entry.Pid != want.Pid || first.entry.Tid != want.Tid ||
		first.entry.Process != want.Process || first.entry.Tag != want.Tag || first.entry.Message != want.Message {
		t.Errorf("first entry = %+v, want %+v", first.entry, want)
	}
	if first.seq != dl.startSeq || dl.rest.live()[0].seq != dl.startSeq+1 || dl.user.live()[1].seq != dl.startSeq+2 {
		t.Errorf("sequence numbers do not follow capture order")
	}
	if long := dl.user.live()[1].entry; len(long.Message) > api.MaxLogMessage || long.Level != api.LogFatal || long.Tag != "AgemuTestApp" {
		t.Errorf("long fault line: %d bytes, level %s, tag %q", len(long.Message), long.Level, long.Tag)
	}
	if !dl.firstLine.Equal(t0) || !dl.unresolved["user/BEDADDA3-F548-4858-969F-EF8C2C30CE28/AgemuTestApp.app"] {
		t.Errorf("firstLine %v, unresolved %v", dl.firstLine, dl.unresolved)
	}
	// A replaced stream's lines are ignored.
	ingestLine(t, b, dl, &logStream{}, line(t0, "Default", 42, app, app, "", "", "late"))
	if len(dl.user.live()) != 2 {
		t.Error("a line of a replaced stream was kept")
	}
}

func TestRingDropsOldestWithinBytes(t *testing.T) {
	r := logRing{max: 1000}
	for i := 1; i <= 50; i++ {
		r.add(logRecord{seq: uint64(i), entry: api.LogEntry{Message: strings.Repeat("m", 36)}}) // 100 bytes each
	}
	live := r.live()
	if len(live) != 10 || live[0].seq != 41 || live[9].seq != 50 || r.dropSeq != 40 || r.bytes != 1000 {
		t.Fatalf("kept %d (first %d, last %d), dropped up to %d, bytes %d; want the newest 10", len(live), live[0].seq, live[len(live)-1].seq, r.dropSeq, r.bytes)
	}
	// One record bigger than the bound is still kept (the newest always is).
	r.add(logRecord{seq: 51, entry: api.LogEntry{Message: strings.Repeat("m", 2000)}})
	if live := r.live(); len(live) != 1 || live[0].seq != 51 {
		t.Fatalf("after a huge record: %d kept", len(live))
	}
	// Compaction keeps order across many drops.
	big := logRing{max: 100 * 64}
	for i := 1; i <= 20000; i++ {
		big.add(logRecord{seq: uint64(i)})
	}
	live = big.live()
	if len(live) != 100 || live[0].seq != 19901 || len(big.recs) > 2*4096+200 {
		t.Fatalf("kept %d from %d, backing array %d", len(live), live[0].seq, len(big.recs))
	}
	// prepend puts older records in front and keeps the bound.
	p := logRing{max: 300}
	p.add(logRecord{seq: 10})
	p.add(logRecord{seq: 11})
	p.prepend([]logRecord{{seq: 1}, {seq: 2}, {seq: 3}})
	if live := p.live(); len(live) != 4 || live[0].seq != 2 || live[3].seq != 11 || p.dropSeq != 1 {
		t.Fatalf("prepend: %+v", live)
	}
}

func TestQuery(t *testing.T) {
	b, dl, s := testCapture(t)
	app := testApp + "/AgemuTestApp"
	other := testData + "/Containers/Bundle/Application/0000/Other.app/Other"
	sb := testRT + "/System/Library/CoreServices/SpringBoard.app/SpringBoard"
	ingestLine(t, b, dl, s, line(t0, "Default", 42, app, app, "dev.agemu.testapp", "probe", "a1"))
	ingestLine(t, b, dl, s, line(t0.Add(1*time.Second), "Default", 9, sb, sb, "", "", "s1"))
	ingestLine(t, b, dl, s, line(t0.Add(2*time.Second), "Error", 42, app, app, "dev.agemu.testapp", "probe", "a2"))
	ingestLine(t, b, dl, s, line(t0.Add(3*time.Second), "Default", 77, other, other, "", "", "o1"))
	ingestLine(t, b, dl, s, line(t0.Add(4*time.Second), "Default", 9, sb, sb, "", "", "s2"))
	key := "user/BEDADDA3-F548-4858-969F-EF8C2C30CE28/AgemuTestApp.app"
	appKeys := map[string]bool{key: true}
	msgs := func(bt api.LogBatch) string {
		var m []string
		for _, e := range bt.Entries {
			m = append(m, e.Message)
		}
		return strings.Join(m, ",")
	}

	all := dl.queryLocked(api.LogQuery{}, nil)
	if got := msgs(all); got != "a1,s1,a2,o1,s2" {
		t.Errorf("every process: %s", got)
	}
	if all.Cursor != dl.startSeq+4 || !all.CapturedFrom.Equal(t0) || !all.Partial {
		t.Errorf("cursor %d (start %d), capturedFrom %v, partial %v (no since: the window reaches before the capture)", all.Cursor, dl.startSeq, all.CapturedFrom, all.Partial)
	}
	if got := msgs(dl.queryLocked(api.LogQuery{}, appKeys)); got != "a1,a2" {
		t.Errorf("the app's lines: %s", got)
	}
	if got := msgs(dl.queryLocked(api.LogQuery{}, map[string]bool{})); got != "" {
		t.Errorf("an app never seen: %s", got)
	}
	since := dl.queryLocked(api.LogQuery{Since: t0.Add(1500 * time.Millisecond)}, nil)
	if got := msgs(since); got != "a2,o1,s2" || since.Partial {
		t.Errorf("since: %s, partial %v", got, since.Partial)
	}
	if got := msgs(dl.queryLocked(api.LogQuery{Level: api.LogError}, nil)); got != "a2" {
		t.Errorf("level error: %s", got)
	}
	cur := dl.queryLocked(api.LogQuery{After: dl.startSeq + 2}, nil)
	if got := msgs(cur); got != "o1,s2" || cur.Partial {
		t.Errorf("after the cursor: %s, partial %v", got, cur.Partial)
	}
	if got := msgs(dl.queryLocked(api.LogQuery{After: all.Cursor}, nil)); got != "" {
		t.Errorf("after the newest cursor: %s", got)
	}
	// A cursor from before this capture (an earlier daemon or boot) reaches back before it.
	if old := dl.queryLocked(api.LogQuery{After: 5}, nil); !old.Partial || len(old.Entries) != 5 {
		t.Errorf("old cursor: %d entries, partial %v", len(old.Entries), old.Partial)
	}
	// Once the ring dropped lines, what it holds whole starts at the newest line it dropped.
	dl.rest.max = 1
	ingestLine(t, b, dl, s, line(t0.Add(5*time.Second), "Default", 9, sb, sb, "", "", "s3"))
	q := dl.queryLocked(api.LogQuery{Since: t0.Add(2 * time.Second)}, nil)
	if !q.Partial || !q.CapturedFrom.Equal(t0.Add(4*time.Second)) {
		t.Errorf("after drops: capturedFrom %v, partial %v", q.CapturedFrom, q.Partial)
	}
	// A cursor taken before the dropped lines is partial; one after them is not.
	if q := dl.queryLocked(api.LogQuery{After: dl.startSeq + 1}, nil); !q.Partial {
		t.Error("a cursor before dropped lines is not partial")
	}
	if q := dl.queryLocked(api.LogQuery{After: dl.startSeq + 4}, nil); q.Partial || len(q.Entries) != 1 {
		t.Errorf("a cursor after the dropped lines: partial %v, %d entries", q.Partial, len(q.Entries))
	}
	// The app's own ring did not drop anything: its window is whole.
	if q := dl.queryLocked(api.LogQuery{Since: t0}, appKeys); q.Partial || !q.CapturedFrom.Equal(t0) {
		t.Errorf("app window: capturedFrom %v, partial %v", q.CapturedFrom, q.Partial)
	}
	// A stream restart leaves a gap.
	dl.gapAfter = t0.Add(10 * time.Second)
	if q := dl.queryLocked(api.LogQuery{Since: t0}, appKeys); !q.Partial || !q.CapturedFrom.Equal(dl.gapAfter) {
		t.Errorf("after a gap: capturedFrom %v, partial %v", q.CapturedFrom, q.Partial)
	}
}

func TestQueryBeforeTheFirstLine(t *testing.T) {
	_, dl, _ := testCapture(t)
	q := dl.queryLocked(api.LogQuery{Since: t0}, nil)
	if !q.Partial || !q.CapturedFrom.IsZero() || len(q.Entries) != 0 || q.Cursor != dl.startSeq-1 {
		t.Errorf("empty capture: %+v", q)
	}
}

func TestBackfillGoesInFront(t *testing.T) {
	b, dl, s := testCapture(t)
	app := testApp + "/AgemuTestApp"
	sb := testRT + "/System/Library/CoreServices/SpringBoard.app/SpringBoard"
	// The stream's first lines; the second came out of order and is also in the store.
	ingestLine(t, b, dl, s, line(t0.Add(10*time.Second), "Default", 42, app, app, "", "", "streamed"))
	ingestLine(t, b, dl, s, line(t0.Add(9*time.Second+990*time.Millisecond), "Default", 9, sb, sb, "", "", "late"))
	var evs []streamEvent
	for _, raw := range [][]byte{
		line(t0.Add(-200*time.Second), "Default", 42, app, app, "", "", "too old"),
		line(t0.Add(5*time.Second), "Error", 9, sb, sb, "", "", "stored 2"),
		line(t0.Add(3*time.Second), "Default", 42, app, app, "", "", "stored 1"),
		line(t0.Add(9*time.Second+990*time.Millisecond), "Default", 9, sb, sb, "", "", "late"),
		line(t0.Add(10*time.Second), "Default", 42, app, app, "", "", "streamed"),
	} {
		var ev streamEvent
		_ = json.Unmarshal(raw, &ev)
		evs = append(evs, ev)
	}
	if n := dl.addBackfillLocked(evs, t0.Add(-100*time.Second), t0.Add(10*time.Second)); n != 2 {
		t.Fatalf("backfill added %d, want 2", n)
	}
	q := dl.queryLocked(api.LogQuery{}, nil)
	var got []string
	for _, e := range q.Entries {
		got = append(got, e.Message)
	}
	if strings.Join(got, ",") != "stored 1,stored 2,streamed,late" {
		t.Fatalf("order: %v", got)
	}
	// Backfilled lines are numbered below the capture, so a cursor taken before they arrived
	// does not return them as new.
	if q := dl.queryLocked(api.LogQuery{After: dl.startSeq - 1}, nil); len(q.Entries) != 2 {
		t.Errorf("after the start cursor: %d entries", len(q.Entries))
	}
}

func TestClearKeepsTheStream(t *testing.T) {
	b, dl, s := testCapture(t)
	app := testApp + "/AgemuTestApp"
	ingestLine(t, b, dl, s, line(t0, "Default", 42, app, app, "", "", "before reset"))
	dl.bundles["user/X/A.app"] = "a"
	dl.bundles["system/Applications/M.app"] = "m"
	dl.procs[42] = &appProc{pid: 42, app: "a"}
	dl.clear()
	if len(dl.user.live())+len(dl.rest.live()) != 0 || len(dl.procs) != 0 || dl.stream != s {
		t.Fatal("clear left lines or records, or stopped the stream")
	}
	if _, ok := dl.bundles["user/X/A.app"]; ok || dl.bundles["system/Applications/M.app"] != "m" {
		t.Errorf("bundles after clear: %v (user apps are uninstalled by reset; built-in ones stay)", dl.bundles)
	}
	ingestLine(t, b, dl, s, line(t0.Add(time.Second), "Default", 42, app, app, "", "", "after reset"))
	if q := dl.queryLocked(api.LogQuery{}, nil); len(q.Entries) != 1 || q.Entries[0].Message != "after reset" {
		t.Errorf("after clear: %+v", q.Entries)
	}
}

func TestStrayStreams(t *testing.T) {
	const other = "18962024-0BC7-40CD-9598-F9C4C8DD49E9"
	args := streamArgs(testUDID, 500)
	if got := strings.Join(args[2:], " "); got != "log stream --style ndjson --level info --timeout 1000500m" {
		t.Fatalf("stream args: %q", got)
	}
	ps := fmt.Sprintf(`  100     1 launchd_sim /Users/me/Library/Developer/CoreSimulator/Devices/%[1]s/data/var/run/launchd_bootstrap.plist
  200     1 launchd_sim /Users/me/Library/Developer/CoreSimulator/Devices/%[2]s/data/var/run/launchd_bootstrap.plist
  500     1 /Users/me/agemu/bin/agemud
  600     1 /Users/me/other/bin/agemud
  501   500 /Library/Developer/PrivateFrameworks/CoreSimulator.framework/Versions/A/Resources/bin/simctl spawn %[1]s log stream --style ndjson --level info --timeout 1000500m
  101   100 log stream --style ndjson --level info --timeout 1000500m
  102   100 log stream --style ndjson --level info --timeout 1000600m
  103   100 log stream --style ndjson --level info --timeout 1000700m
  104   100 log stream --style ndjson --level info
  105   100 log stream --level debug
  201   200 log stream --style ndjson --level info --timeout 1000700m
`, testUDID, other)
	// Daemon 500 has no stream on the device: its own (101) and the one of a daemon that is
	// gone (103) are strays; another live daemon's (102), other tools' streams (104, 105) and
	// other devices' (201) are not.
	if got := fmt.Sprint(strayStreams(ps, testUDID, 500)); got != "[101 103]" {
		t.Errorf("strays for daemon 500: %s", got)
	}
	if got := fmt.Sprint(strayStreams(ps, testUDID, 600)); got != "[102 103]" {
		t.Errorf("strays for daemon 600: %s", got)
	}
	if got := fmt.Sprint(strayStreams(ps, "00000000-0000-0000-0000-000000000000", 500)); got != "[]" {
		t.Errorf("no such device: %s", got)
	}
}

func BenchmarkIngest(b *testing.B) {
	be := &Backend{logs: map[string]*deviceLogs{}}
	dl := be.devLogs(testUDID)
	s := &logStream{done: make(chan struct{})}
	dl.stream, dl.backfill, dl.firstSeen = s, true, make(chan struct{})
	// A real-sized line (~1.3 KB of ndjson, as the stream prints them).
	raw := line(t0, "Default", 48658, testRT+"/System/Library/CoreServices/SpringBoard.app/SpringBoard",
		testRT+"/System/Library/PrivateFrameworks/FrontBoard.framework/FrontBoard", "com.apple.FrontBoard", "Process",
		"[app<dev.agemu.testapp>:12263] Workspace connection invalidated."+strings.Repeat(" padding", 80))
	b.SetBytes(int64(len(raw)))
	for i := 0; i < b.N; i++ {
		var ev streamEvent
		if err := json.Unmarshal(raw, &ev); err != nil {
			b.Fatal(err)
		}
		be.ingest(dl, s, &ev)
	}
}

func BenchmarkQueryApp(b *testing.B) {
	be := &Backend{logs: map[string]*deviceLogs{}}
	dl := be.devLogs(testUDID)
	s := &logStream{done: make(chan struct{})}
	dl.stream, dl.backfill, dl.firstSeen = s, true, make(chan struct{})
	app := testApp + "/AgemuTestApp"
	sb := testRT + "/System/Library/CoreServices/SpringBoard.app/SpringBoard"
	for i := 0; i < 20000; i++ {
		img := sb
		if i%2 == 0 {
			img = app
		}
		var ev streamEvent
		_ = json.Unmarshal(line(t0.Add(time.Duration(i)*time.Millisecond), "Default", 42, img, img, "s", "c", fmt.Sprintf("line %d with some text", i)), &ev)
		be.ingest(dl, s, &ev)
	}
	keys := map[string]bool{bundleKey(app): true}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dl.mu.Lock()
		q := dl.queryLocked(api.LogQuery{Since: t0.Add(15 * time.Second)}, keys)
		dl.mu.Unlock()
		if len(q.Entries) == 0 {
			b.Fatal("no entries")
		}
	}
}
