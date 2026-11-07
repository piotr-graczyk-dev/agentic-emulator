package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/api"
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/backend"
)

// BenchmarkCallNotice measures what the crash notice adds to a call: "ui" on a backend without
// a watcher, and with one watching a launched app that has not crashed.
func BenchmarkCallNotice(b *testing.B) {
	for _, watched := range []bool{false, true} {
		b.Run(fmt.Sprintf("watcher=%v", watched), func(b *testing.B) {
			var be backend.Backend = &fakeBackend{}
			if watched {
				be = &obsBackend{}
			}
			svc := NewService(b.TempDir(), be)
			if watched {
				svc.Call(context.Background(), "launch", json.RawMessage(`{"device":"android:x","app":"com.app"}`))
			}
			raw := json.RawMessage(`{"device":"android:x"}`)
			for b.Loop() {
				if _, _, err := svc.Handle(context.Background(), "ui", raw); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkSelectLogs filters a full ring buffer's worth of lines (20,000) in the daemon.
func BenchmarkSelectLogs(b *testing.B) {
	entries := make([]api.LogEntry, 20000)
	for i := range entries {
		entries[i] = entry(i, api.LogLevels[i%len(api.LogLevels)], "com.app", "ReactNativeJS", fmt.Sprintf("line %d: some message of a typical length, with a few words in it", i))
	}
	re, _ := api.CompileLogRegex(`line \d+5:`)
	for _, c := range []struct {
		name string
		q    api.LogQuery
		re   *regexp.Regexp
	}{
		{"all", api.LogQuery{}, nil},
		{"level+contains", api.LogQuery{Level: api.LogWarn, Contains: "line 19"}, nil},
		{"regex", api.LogQuery{}, re},
	} {
		b.Run(c.name, func(b *testing.B) {
			for b.Loop() {
				selectLogs(entries, c.q, c.re, api.DefaultLogLimit, api.DefaultLogBytes)
			}
		})
	}
}

// obsBackend implements LogReader, CrashReader and AppWatcher with canned data, and the
// device calls the crash notice depends on (launch, terminate, reset). It records queries.
type obsBackend struct {
	fakeBackend
	mu       sync.Mutex
	entries  []api.LogEntry
	batch    api.LogBatch // Cursor, CapturedFrom, Partial
	reports  []api.Crash
	state    map[string]api.AppState // app -> its state (Crash filtered by since)
	slow     bool                    // AppState blocks until its context ends
	logQ     []api.LogQuery
	crashQ   []api.CrashQuery
	watchQ   []string
	launched []string
}

var (
	_ backend.LogReader   = (*obsBackend)(nil)
	_ backend.CrashReader = (*obsBackend)(nil)
	_ backend.AppWatcher  = (*obsBackend)(nil)
)

func (o *obsBackend) Logs(ctx context.Context, id string, q api.LogQuery) (api.LogBatch, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.logQ = append(o.logQ, q)
	b := o.batch
	for _, e := range o.entries {
		if q.App == "" || e.Process == q.App {
			b.Entries = append(b.Entries, e)
		}
	}
	return b, nil
}

func (o *obsBackend) Crashes(ctx context.Context, id string, q api.CrashQuery) ([]api.Crash, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.crashQ = append(o.crashQ, q)
	return append([]api.Crash(nil), o.reports...), nil // unfiltered: the daemon must filter
}

func (o *obsBackend) AppState(ctx context.Context, id, app string, since time.Time) (api.AppState, error) {
	if o.slow {
		<-ctx.Done()
		return api.AppState{}, ctx.Err()
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.watchQ = append(o.watchQ, app)
	st, ok := o.state[app]
	if !ok {
		return api.AppState{Running: true}, nil
	}
	if st.Crash != nil && st.Crash.Time.Before(since) {
		st.Crash = nil
	}
	return st, nil
}

func (o *obsBackend) setState(app string, st api.AppState) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.state == nil {
		o.state = map[string]api.AppState{}
	}
	o.state[app] = st
}

func (o *obsBackend) lastLogQuery() api.LogQuery {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.logQ[len(o.logQ)-1]
}

func (o *obsBackend) Launch(ctx context.Context, id, app string) error {
	o.mu.Lock()
	o.launched = append(o.launched, app)
	o.mu.Unlock()
	return nil
}
func (o *obsBackend) Terminate(ctx context.Context, id, app string) error { return nil }
func (o *obsBackend) Reset(ctx context.Context, id string) error          { return nil }

var t0 = time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)

func entry(sec int, level api.LogLevel, proc, tag, msg string) api.LogEntry {
	return api.LogEntry{Time: t0.Add(time.Duration(sec) * time.Second), Level: level, Pid: 100, Process: proc, Tag: tag, Message: msg}
}

func sampleEntries() []api.LogEntry {
	return []api.LogEntry{
		entry(0, api.LogDebug, "com.app", "Net", "GET /items"),
		entry(1, api.LogInfo, "com.app", "ReactNativeJS", "Rendering list"),
		entry(2, api.LogWarn, "system_server", "ActivityManager", "Slow operation"),
		entry(3, api.LogError, "com.app", "ReactNativeJS", "TypeError: undefined is not a function"),
		entry(4, api.LogFatal, "com.app", "AndroidRuntime", "FATAL EXCEPTION: main\njava.lang.IllegalStateException: boom"),
	}
}

func logsOf(t *testing.T, res any) api.Logs {
	t.Helper()
	l, ok := res.(api.Logs)
	if !ok {
		t.Fatalf("result %T, want api.Logs", res)
	}
	return l
}

func messages(l api.Logs) string {
	var m []string
	for _, e := range l.Entries {
		m = append(m, strings.SplitN(e.Message, "\n", 2)[0])
	}
	return strings.Join(m, " | ")
}

func TestLogsFilterAndBoundInTheDaemon(t *testing.T) {
	cases := []struct {
		params, want string
		omitted      int
	}{
		{`{}`, "GET /items | Rendering list | Slow operation | TypeError: undefined is not a function | FATAL EXCEPTION: main", 0},
		{`{"app":"com.app"}`, "GET /items | Rendering list | TypeError: undefined is not a function | FATAL EXCEPTION: main", 0},
		{`{"level":"W"}`, "Slow operation | TypeError: undefined is not a function | FATAL EXCEPTION: main", 0},
		{`{"level":"error","app":"com.app"}`, "TypeError: undefined is not a function | FATAL EXCEPTION: main", 0},
		{`{"contains":"reactnativejs"}`, "Rendering list | TypeError: undefined is not a function", 0}, // the tag
		{`{"contains":"ILLEGALSTATE"}`, "FATAL EXCEPTION: main", 0},                                    // a later line of the message
		{`{"regex":"^React"}`, "Rendering list | TypeError: undefined is not a function", 0},
		{`{"regex":"Type(Error|Exception)","contains":"undefined"}`, "TypeError: undefined is not a function", 0},
		{`{"limit":2}`, "TypeError: undefined is not a function | FATAL EXCEPTION: main", 3},
		{`{"since":"2026-11-06T10:00:03Z"}`, "TypeError: undefined is not a function | FATAL EXCEPTION: main", 0},
		{`{"since":"2026-11-06T11:00:02+01:00"}`, "Slow operation | TypeError: undefined is not a function | FATAL EXCEPTION: main", 0},
		{`{"contains":"nothing like it"}`, "", 0},
	}
	for _, c := range cases {
		ob := &obsBackend{entries: sampleEntries(), batch: api.LogBatch{Cursor: 1730970000000042}}
		svc := NewService(t.TempDir(), ob)
		res, err := call(t, svc, "logs", withDevice("android:x", c.params))
		if err != nil {
			t.Fatalf("%s: %v", c.params, err)
		}
		l := logsOf(t, res)
		if got := messages(l); got != c.want || l.Omitted != c.omitted {
			t.Fatalf("%s:\n got %q (omitted %d)\nwant %q (omitted %d)", c.params, got, l.Omitted, c.want, c.omitted)
		}
		if l.Cursor != "c1730970000000042" {
			t.Fatalf("%s: cursor %q", c.params, l.Cursor)
		}
		if l.Entries == nil {
			t.Fatalf("%s: entries must be [] when nothing matches, not null", c.params)
		}
	}
}

func TestLogsByteBound(t *testing.T) {
	big := strings.Repeat("x", 900)
	var entries []api.LogEntry
	for i := range 5 {
		entries = append(entries, entry(i, api.LogInfo, "com.app", "T", fmt.Sprintf("%d %s", i, big)))
	}
	ob := &obsBackend{entries: entries}
	svc := NewService(t.TempDir(), ob)
	// Each entry is about 1 KB: 2 KB holds the newest two.
	res, err := call(t, svc, "logs", `{"device":"android:x","maxBytes":2100}`)
	if err != nil {
		t.Fatal(err)
	}
	l := logsOf(t, res)
	if len(l.Entries) != 2 || !strings.HasPrefix(l.Entries[0].Message, "3 ") || !strings.HasPrefix(l.Entries[1].Message, "4 ") || l.Omitted != 3 {
		t.Fatalf("got %d entries (omitted %d), want the newest 2 (3 and 4)", len(l.Entries), l.Omitted)
	}
	// The newest entry alone is over the bound: it is kept, its message cut to fit.
	ob.entries = []api.LogEntry{entry(0, api.LogInfo, "com.app", "T", strings.Repeat("y", 5000))}
	res, err = call(t, svc, "logs", `{"device":"android:x","maxBytes":1024}`)
	if err != nil {
		t.Fatal(err)
	}
	l = logsOf(t, res)
	if len(l.Entries) != 1 || l.Entries[0].Size() > 1024 || !strings.Contains(l.Entries[0].Message, "bytes cut]") {
		t.Fatalf("got %d entries, size %d, message %q...", len(l.Entries), l.Entries[0].Size(), l.Entries[0].Message[:20])
	}
	// One message is never longer than api.MaxLogMessage.
	ob.entries = []api.LogEntry{entry(0, api.LogInfo, "com.app", "T", strings.Repeat("z", 3*api.MaxLogMessage))}
	res, _ = call(t, svc, "logs", `{"device":"android:x","maxBytes":1048576}`)
	if m := logsOf(t, res).Entries[0].Message; len(m) > api.MaxLogMessage {
		t.Fatalf("message of %d bytes", len(m))
	}
}

func TestLogsQueryReachesTheBackend(t *testing.T) {
	captured := t0.Add(-time.Hour).In(time.FixedZone("CET", 3600))
	ob := &obsBackend{batch: api.LogBatch{Cursor: 7, CapturedFrom: captured, Partial: true}}
	svc := NewService(t.TempDir(), ob)

	res, err := call(t, svc, "logs", `{"device":"android:x","app":" com.app ","level":"Warning","contains":"TypeError","regex":"a+b","since":"5m"}`)
	if err != nil {
		t.Fatal(err)
	}
	q := ob.lastLogQuery()
	if q.App != "com.app" || q.Level != api.LogWarn || q.Contains != "typeerror" || q.Regex != "a+b" || q.After != 0 {
		t.Fatalf("query %+v", q)
	}
	if ago := time.Since(q.Since); ago < 5*time.Minute || ago > 5*time.Minute+5*time.Second {
		t.Fatalf("since 5m reached the backend as %v (%v ago)", q.Since, ago)
	}
	l := logsOf(t, res)
	if !l.Partial || l.CapturedFrom == nil || !l.CapturedFrom.Equal(captured) || l.Cursor != "c7" {
		t.Fatalf("result %+v: want partial, capturedFrom and cursor c7 from the backend", l)
	}
	b, _ := json.Marshal(l)
	if !strings.Contains(string(b), `"entries":[]`) || !strings.Contains(string(b), `"partial":true`) || !strings.Contains(string(b), `"capturedFrom":"2026-11-06T09:00:00Z"`) {
		t.Fatalf("JSON %s (times in UTC)", b)
	}
	// Entries come back in UTC whatever zone the backend used.
	ob.entries = []api.LogEntry{entry(0, api.LogInfo, "com.app", "T", "m")}
	ob.entries[0].Time = ob.entries[0].Time.In(time.FixedZone("PST", -8*3600))
	res, _ = call(t, svc, "logs", `{"device":"android:x"}`)
	if e := logsOf(t, res).Entries[0]; e.Time.Location() != time.UTC || !e.Time.Equal(t0) {
		t.Fatalf("entry time %v", e.Time)
	}

	if _, err := call(t, svc, "logs", `{"device":"android:x","since":"c1730970000000042"}`); err != nil {
		t.Fatal(err)
	}
	if q := ob.lastLogQuery(); q.After != 1730970000000042 || !q.Since.IsZero() {
		t.Fatalf("a cursor reached the backend as %+v", q)
	}
}

func TestObserveArgumentsAreCheckedFirstAndTakeNoDeviceLock(t *testing.T) {
	bad := []struct{ method, params, want string }{
		{"logs", `{"level":"loud"}`, `unknown log level "loud"`},
		{"logs", `{"regex":"("}`, "bad regex"},
		{"logs", `{"regex":"` + strings.Repeat("a", api.MaxLogPattern+1) + `"}`, "more than 1000"},
		{"logs", `{"limit":-1}`, "limit must be between 1 and 10000"},
		{"logs", `{"limit":10001}`, "limit must be between 1 and 10000"},
		{"logs", `{"maxBytes":10}`, "maxBytes must be between 1024 and 1048576"},
		{"logs", `{"maxBytes":2000000}`, "maxBytes must be between"},
		{"logs", `{"since":"yesterday"}`, `since "yesterday"`},
		{"logs", `{"since":"-5m"}`, "must be positive"},
		{"logs", `{"since":"launch"}`, "no app was launched through agemu"},
		{"logs", `{"since":"launch","app":"com.app"}`, "com.app was not launched through agemu"},
		{"logs", `{"app":"com app"}`, "not a package name"},
		{"logs", `{"app":"com.app;reboot"}`, "not a package name"},
		{"crashes", `{"app":"$(id)"}`, "not a package name"},
		{"crashes", `{"since":"c123"}`, "not a logs cursor"},
		{"crashes", `{"level":"error"}`, "crashes takes app, since and limit"},
		{"crashes", `{"limit":101}`, "limit must be between 1 and 100"},
	}
	ob := &obsBackend{entries: sampleEntries()}
	svc := NewService(t.TempDir(), ob)
	// Another agent holds the device for a long action.
	l := svc.deviceLock("android:x")
	l.Lock()
	defer l.Unlock()
	run := func(method, params string) error {
		done := make(chan error, 1)
		go func() {
			_, err := call(t, svc, method, withDevice("android:x", params))
			done <- err
		}()
		select {
		case err := <-done:
			return err
		case <-time.After(2 * time.Second):
			t.Fatalf("%s %s: blocked on the device lock", method, params)
		}
		return nil
	}
	for _, c := range bad {
		err := run(c.method, c.params)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s %s: err = %v, want %q", c.method, c.params, err, c.want)
		}
		if errors.Is(err, api.ErrNotImplemented) || errors.Is(err, api.ErrAppCrashed) {
			t.Fatalf("%s %s: an argument error with a code: %v", c.method, c.params, err)
		}
	}
	if len(ob.logQ) != 0 || len(ob.crashQ) != 0 {
		t.Fatalf("the backend was asked: %v %v", ob.logQ, ob.crashQ)
	}
	// Valid reads do not wait for the device lock either.
	for _, m := range []string{"logs", "crashes"} {
		if err := run(m, `{}`); err != nil {
			t.Fatalf("%s: %v", m, err)
		}
	}
}

func TestSinceLaunch(t *testing.T) {
	ob := &obsBackend{}
	svc := NewService(t.TempDir(), ob)
	before := time.Now()
	if _, err := call(t, svc, "launch", `{"device":"android:x","app":"com.app/.Main"}`); err != nil {
		t.Fatal(err)
	}
	after := time.Now()
	for _, params := range []string{`{"since":"launch","app":"com.app"}`, `{"since":"LAUNCH"}`} {
		if _, err := call(t, svc, "logs", withDevice("android:x", params)); err != nil {
			t.Fatalf("%s: %v", params, err)
		}
		if s := ob.lastLogQuery().Since; s.Before(before) || s.After(after) {
			t.Fatalf("%s: since %v, want the launch's start (%v..%v)", params, s, before, after)
		}
	}
	if _, err := call(t, svc, "crashes", `{"device":"android:x","since":"launch","app":"com.app"}`); err != nil {
		t.Fatal(err)
	}
	if s := ob.crashQ[0].Since; s.Before(before) || s.After(after) {
		t.Fatalf("crashes since launch: %v", s)
	}
	// Another app's launch is not this app's.
	if _, err := call(t, svc, "logs", `{"device":"android:x","since":"launch","app":"com.other"}`); err == nil {
		t.Fatal("since launch of an app agemu did not launch: want an error")
	}
}

func TestResetStartsLogsAndCrashesAfterIt(t *testing.T) {
	ob := &obsBackend{
		entries: sampleEntries(), // all from t0, before the reset
		reports: []api.Crash{{Time: t0, App: "com.app", Kind: api.CrashKindCrash}},
	}
	svc := NewService(t.TempDir(), ob)
	if _, err := call(t, svc, "launch", `{"device":"android:x","app":"com.app"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, svc, "reset", `{"device":"android:x"}`); err != nil {
		t.Fatal(err)
	}
	resetEnd := time.Now()
	for _, params := range []string{`{}`, `{"since":"2026-11-06T10:00:00Z"}`, `{"since":"c5"}`} {
		res, err := call(t, svc, "logs", withDevice("android:x", params))
		if err != nil {
			t.Fatal(err)
		}
		if q := ob.lastLogQuery(); q.Since.IsZero() || q.Since.After(resetEnd) || time.Since(q.Since) > time.Minute {
			t.Fatalf("%s: since %v, want the reset's end", params, q.Since)
		}
		if l := logsOf(t, res); len(l.Entries) != 0 {
			t.Fatalf("%s: lines from before the reset: %q", params, messages(l))
		}
	}
	res, err := call(t, svc, "crashes", `{"device":"android:x"}`)
	if err != nil {
		t.Fatal(err)
	}
	if cs := res.(api.Crashes); len(cs.Crashes) != 0 {
		t.Fatalf("a crash from before the reset: %+v", cs)
	}
	// The launch was forgotten with the reset.
	if _, err := call(t, svc, "logs", `{"device":"android:x","since":"launch"}`); err == nil {
		t.Fatal("since launch after a reset: want an error")
	}
}

func TestCrashesAreSortedFilteredAndBounded(t *testing.T) {
	frames := make([]string, 30)
	for i := range frames {
		frames[i] = fmt.Sprintf("at f%d", i)
	}
	ob := &obsBackend{reports: []api.Crash{
		{Time: t0.Add(3 * time.Second), App: "com.app", Kind: api.CrashKindANR, Reason: "Input dispatching timed out"},
		{Time: t0.Add(1 * time.Second), App: "com.app", Kind: api.CrashKindCrash, Exception: "java.lang.IllegalStateException", Reason: strings.Repeat("r", 3*api.MaxCrashText), Frames: frames},
		{Time: t0.Add(2 * time.Second), App: "com.other", Kind: api.CrashKindCrash},
		{Time: t0.Add(4 * time.Second).In(time.FixedZone("CET", 3600)), App: "com.app", Kind: api.CrashKindKilled, Exception: "jetsam"},
	}}
	svc := NewService(t.TempDir(), ob)
	res, err := call(t, svc, "crashes", `{"device":"android:x","app":"com.app"}`)
	if err != nil {
		t.Fatal(err)
	}
	cs := res.(api.Crashes)
	var kinds []string
	for _, c := range cs.Crashes {
		kinds = append(kinds, string(c.Kind))
	}
	if fmt.Sprint(kinds) != "[crash anr killed]" || cs.Omitted != 0 {
		t.Fatalf("kinds %v (omitted %d), want [crash anr killed]: oldest first, com.other left out", kinds, cs.Omitted)
	}
	if c := cs.Crashes[0]; len(c.Frames) != api.MaxCrashFrames || len(c.Reason) > api.MaxCrashText {
		t.Fatalf("frames %d, reason %d bytes", len(c.Frames), len(c.Reason))
	}
	if loc := cs.Crashes[2].Time.Location(); loc != time.UTC {
		t.Fatalf("crash time in %v, want UTC", loc)
	}
	if q := ob.crashQ[0]; q.App != "com.app" || !q.Since.IsZero() {
		t.Fatalf("query %+v", q)
	}
	res, _ = call(t, svc, "crashes", `{"device":"android:x","limit":2}`)
	cs = res.(api.Crashes)
	if len(cs.Crashes) != 2 || cs.Omitted != 2 || cs.Crashes[1].Kind != api.CrashKindKilled {
		t.Fatalf("limit 2: %+v, want the newest two and 2 omitted", cs)
	}
}

func TestLogsAndCrashesAreNotImplementedWithoutTheirInterface(t *testing.T) {
	svc := NewService(t.TempDir(), &fakeBackend{}, &iosFake{})
	for _, method := range []string{"logs", "crashes"} {
		for _, dev := range []string{"android:x", "ios:y"} {
			plat, _, _ := strings.Cut(dev, ":")
			_, err := call(t, svc, method, withDevice(dev, `{"app":"com.app","since":"5m"}`))
			if !errors.Is(err, api.ErrNotImplemented) || err.Error() != method+" is not implemented on "+plat {
				t.Fatalf("%s on %s: err = %v", method, dev, err)
			}
			if errorCode(err) != CodeNotImplemented {
				t.Fatalf("%s on %s: code %q", method, dev, errorCode(err))
			}
		}
	}
}

func handleCall(t *testing.T, s *Service, method, params string) (any, *api.AppCrash, error) {
	t.Helper()
	return s.Handle(context.Background(), method, json.RawMessage(params))
}

func TestCrashNoticeTellsTheAgentOnce(t *testing.T) {
	ob := &obsBackend{}
	svc := NewService(t.TempDir(), ob)
	if _, c, err := handleCall(t, svc, "launch", `{"device":"android:x","app":"com.app"}`); err != nil || c != nil {
		t.Fatalf("launch: %v %v", c, err)
	}
	if _, c, _ := handleCall(t, svc, "ui", `{"device":"android:x"}`); c != nil {
		t.Fatalf("a notice without a crash: %+v", c)
	}
	// The app crashes between two calls.
	crash := &api.Crash{Time: time.Now(), App: "com.app", Pid: 4242, Kind: api.CrashKindCrash,
		Exception: "java.lang.IllegalStateException", Reason: "boom", Frames: []string{"at a", "at b", "at c", "at d"}}
	ob.setState("com.app", api.AppState{Running: false, Crash: crash})

	res, c, err := handleCall(t, svc, "find", `{"device":"android:x","selector":{"role":"button"}}`)
	if err != nil || c == nil {
		t.Fatalf("find after the crash: notice %v, err %v", c, err)
	}
	if els, _ := res.([]*api.Element); len(els) != 1 {
		t.Fatalf("the call's own result changed: %#v", res)
	}
	if c.App != "com.app" || c.Running || c.Exception != "java.lang.IllegalStateException" || len(c.Frames) != api.NoticeFrames {
		t.Fatalf("notice %+v", c)
	}
	if s := c.Summary(); !strings.Contains(s, "com.app crashed at") || !strings.Contains(s, "IllegalStateException: boom") || !strings.Contains(s, "not running") {
		t.Fatalf("summary %q", s)
	}
	// Once: the next successful call has no notice.
	if _, c, _ := handleCall(t, svc, "ui", `{"device":"android:x"}`); c != nil {
		t.Fatalf("the notice repeated: %+v", c)
	}
	// A failing call while the app is down says why, every time.
	for range 2 {
		_, c, err = handleCall(t, svc, "tap", `{"device":"android:x","selector":{"text":"Pay"}}`)
		if !errors.Is(err, api.ErrAppCrashed) || errorCode(err) != CodeAppCrashed || c == nil {
			t.Fatalf("err = %v (code %q), notice %v; want app_crashed", err, errorCode(err), c)
		}
		if !strings.Contains(err.Error(), "no element matches") || !strings.Contains(err.Error(), "com.app crashed") {
			t.Fatalf("err = %q, want the call's own error and the crash", err)
		}
	}
	// Relaunched: the old crash no longer explains anything.
	ob.setState("com.app", api.AppState{Running: true, Crash: crash})
	if _, _, err := handleCall(t, svc, "launch", `{"device":"android:x","app":"com.app"}`); err != nil {
		t.Fatal(err)
	}
	_, c, err = handleCall(t, svc, "tap", `{"device":"android:x","selector":{"text":"Pay"}}`)
	if errors.Is(err, api.ErrAppCrashed) || c != nil {
		t.Fatalf("after a relaunch: err = %v, notice %v", err, c)
	}
}

func TestCrashNoticeFollowsTheLaunchedApp(t *testing.T) {
	ob := &obsBackend{}
	svc := NewService(t.TempDir(), ob)
	other := &api.Crash{Time: time.Now().Add(time.Second), App: "com.other", Kind: api.CrashKindCrash}
	ob.setState("com.other", api.AppState{Crash: other})
	if _, c, _ := handleCall(t, svc, "ui", `{"device":"android:x"}`); c != nil {
		t.Fatalf("a notice before any launch: %+v", c)
	}
	handleCall(t, svc, "launch", `{"device":"android:x","app":"com.app"}`)
	if _, c, _ := handleCall(t, svc, "ui", `{"device":"android:x"}`); c != nil {
		t.Fatalf("another app's crash was reported: %+v", c)
	}
	// An exit agemu asked for is not watched.
	ob.setState("com.app", api.AppState{Crash: &api.Crash{Time: time.Now().Add(time.Second), App: "com.app", Kind: api.CrashKindCrash}})
	if _, c, err := handleCall(t, svc, "terminate", `{"device":"android:x","app":"com.app"}`); err != nil || c != nil {
		t.Fatalf("terminate: notice %v, err %v", c, err)
	}
	if _, c, _ := handleCall(t, svc, "ui", `{"device":"android:x"}`); c != nil {
		t.Fatalf("a notice after terminate: %+v", c)
	}
	if _, c, err := handleCall(t, svc, "tap", `{"device":"android:x","selector":{"text":"Pay"}}`); c != nil || errors.Is(err, api.ErrAppCrashed) {
		t.Fatalf("a failed tap after terminate: notice %v, err %v", c, err)
	}
	// Every device call asks the watcher about the launched app only.
	for _, app := range ob.watchQ {
		if app != "com.app" {
			t.Fatalf("watcher asked about %q", app)
		}
	}
}

func TestCrashesCallCountsAsTold(t *testing.T) {
	crash := api.Crash{Time: time.Now().Add(time.Second), App: "com.app", Kind: api.CrashKindANR, Reason: "Input dispatching timed out"}
	ob := &obsBackend{reports: []api.Crash{crash}}
	svc := NewService(t.TempDir(), ob)
	handleCall(t, svc, "launch", `{"device":"android:x","app":"com.app"}`)
	ob.setState("com.app", api.AppState{Running: true, Crash: &crash})
	if _, c, err := handleCall(t, svc, "crashes", `{"device":"android:x"}`); err != nil || c != nil {
		t.Fatalf("crashes: notice %v, err %v", c, err)
	}
	if _, c, _ := handleCall(t, svc, "ui", `{"device":"android:x"}`); c != nil {
		t.Fatalf("a crash the agent read in crashes was reported again: %+v", c)
	}
	// An ANR leaves the app running: a later error is the call's own.
	if _, _, err := handleCall(t, svc, "tap", `{"device":"android:x","selector":{"text":"Pay"}}`); errors.Is(err, api.ErrAppCrashed) {
		t.Fatalf("err = %v", err)
	}
}

func TestWaitStopsWhenTheAppCrashesDuringIt(t *testing.T) {
	ob := &obsBackend{}
	svc := NewService(t.TempDir(), ob)
	handleCall(t, svc, "launch", `{"device":"android:x","app":"com.app"}`)
	go func() {
		time.Sleep(200 * time.Millisecond)
		ob.setState("com.app", api.AppState{Crash: &api.Crash{Time: time.Now(), App: "com.app", Kind: api.CrashKindCrash, Exception: "SIGSEGV"}})
	}()
	start := time.Now()
	_, c, err := handleCall(t, svc, "wait", `{"device":"android:x","selector":{"text":"Thank you"},"timeoutMs":5000}`)
	if !errors.Is(err, api.ErrAppCrashed) || c == nil || !strings.Contains(err.Error(), `stopped waiting for {"text":"Thank you"} to appear`) {
		t.Fatalf("err = %v, notice %v", err, c)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("the wait ran %v after the crash; want it to stop at once", el)
	}
	// The usual case: the app crashes after a call returned (a tap, then the crash), and the
	// next call is a compound action. Its wait stops too, and the error keeps both parts.
	ob.setState("com.app", api.AppState{Running: true})
	handleCall(t, svc, "launch", `{"device":"android:x","app":"com.app"}`)
	ob.setState("com.app", api.AppState{Crash: &api.Crash{Time: time.Now(), App: "com.app", Kind: api.CrashKindCrash}})
	start = time.Now()
	_, _, err = handleCall(t, svc, "tap", `{"device":"android:x","x":1,"y":1,"waitFor":{"text":"Thank you"},"waitTimeoutMs":5000}`)
	if !errors.Is(err, api.ErrAppCrashed) || !strings.Contains(err.Error(), "tap done, but: stopped waiting") || strings.Count(err.Error(), "crashed at") != 1 {
		t.Fatalf("err = %v", err)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("the compound wait ran %v", el)
	}
	// Told once, the agent may wait for something else: the wait runs its course, and its
	// timeout still says the app is down.
	_, _, err = handleCall(t, svc, "wait", `{"device":"android:x","selector":{"text":"Thank you"},"timeoutMs":300}`)
	if !errors.Is(err, api.ErrAppCrashed) || !strings.Contains(err.Error(), "timeout after 300ms") {
		t.Fatalf("wait after the agent was told: err = %v", err)
	}
	// A wait for an element to go away is not cut short.
	handleCall(t, svc, "launch", `{"device":"android:x","app":"com.app"}`)
	ob.setState("com.app", api.AppState{Crash: &api.Crash{Time: time.Now(), App: "com.app", Kind: api.CrashKindCrash}})
	_, _, err = handleCall(t, svc, "wait", `{"device":"android:x","selector":{"text":"Log in"},"gone":true,"timeoutMs":300}`)
	if err == nil || !strings.Contains(err.Error(), "timeout after 300ms") {
		t.Fatalf("gone wait: err = %v", err)
	}
}

// stuckTreeBackend's UI tree reads block until their context ends once the app crashed (an
// Android read waits out the accessibility timeout on the dying app's window).
type stuckTreeBackend struct {
	obsBackend
	stuck atomic.Bool
}

func (s *stuckTreeBackend) UITree(ctx context.Context, id string) (*api.UITree, error) {
	if s.stuck.Load() {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return tree(), nil
}

func TestWaitAfterACrashDoesNotWaitForASlowRead(t *testing.T) {
	sb := &stuckTreeBackend{}
	svc := NewService(t.TempDir(), sb)
	handleCall(t, svc, "launch", `{"device":"android:x","app":"com.app"}`)
	sb.setState("com.app", api.AppState{Crash: &api.Crash{Time: time.Now(), App: "com.app", Kind: api.CrashKindCrash}})
	sb.stuck.Store(true)
	start := time.Now()
	_, _, err := handleCall(t, svc, "wait", `{"device":"android:x","selector":{"text":"Thank you"},"timeoutMs":10000}`)
	if !errors.Is(err, api.ErrAppCrashed) {
		t.Fatalf("err = %v", err)
	}
	if el := time.Since(start); el > crashedRead+time.Second {
		t.Fatalf("the wait took %v behind a read held by the dying app", el)
	}
	// The crash comes while a read runs (the wait began just before it): that read is cut short.
	handleCall(t, svc, "launch", `{"device":"android:x","app":"com.app"}`)
	sb.setState("com.app", api.AppState{Running: true})
	go func() {
		time.Sleep(100 * time.Millisecond)
		sb.setState("com.app", api.AppState{Crash: &api.Crash{Time: time.Now(), App: "com.app", Kind: api.CrashKindCrash}})
	}()
	start = time.Now()
	_, _, err = handleCall(t, svc, "wait", `{"device":"android:x","selector":{"text":"Thank you"},"timeoutMs":10000}`)
	if !errors.Is(err, api.ErrAppCrashed) {
		t.Fatalf("crash during the read: err = %v", err)
	}
	if el := time.Since(start); el > 100*time.Millisecond+crashedRead+time.Second {
		t.Fatalf("the read the crash came during held the wait %v", el)
	}
	// What the screen still shows (a system window) is found by that last read.
	sb.stuck.Store(false)
	handleCall(t, svc, "launch", `{"device":"android:x","app":"com.app"}`)
	sb.setState("com.app", api.AppState{Crash: &api.Crash{Time: time.Now(), App: "com.app", Kind: api.CrashKindCrash}})
	if res, _, err := handleCall(t, svc, "wait", `{"device":"android:x","selector":{"text":"Log in"},"timeoutMs":10000}`); err != nil || res == nil {
		t.Fatalf("an element on screen after the crash: %v %v", res, err)
	}
}

func TestASlowWatcherDoesNotHoldCalls(t *testing.T) {
	ob := &obsBackend{slow: true}
	svc := NewService(t.TempDir(), ob)
	handleCall(t, svc, "launch", `{"device":"android:x","app":"com.app"}`)
	start := time.Now()
	if _, c, err := handleCall(t, svc, "ui", `{"device":"android:x"}`); err != nil || c != nil {
		t.Fatalf("ui: %v %v", c, err)
	}
	if el := time.Since(start); el > watchTimeout+500*time.Millisecond {
		t.Fatalf("ui took %v behind a slow watcher", el)
	}
}

func TestErrorCodes(t *testing.T) {
	crashed := &api.AppCrashedError{Crash: api.AppCrash{Crash: api.Crash{App: "a", Kind: api.CrashKindCrash}}, Err: errors.New("no element matches")}
	if errorCode(crashed) != CodeAppCrashed || errorCode(fmt.Errorf("tap done, but: %w", crashed)) != CodeAppCrashed {
		t.Fatal("app crashed without its code")
	}
	both := &api.AppCrashedError{Err: api.NotImplemented(api.IOS, "logs", "")}
	if errorCode(both) != CodeNotImplemented {
		t.Fatal("not implemented must win over app crashed")
	}
}

func TestTheNoticeCrossesTheSocket(t *testing.T) {
	ob := &obsBackend{}
	svc := NewService(t.TempDir(), ob)
	handleCall(t, svc, "launch", `{"device":"android:x","app":"com.app"}`)
	ob.setState("com.app", api.AppState{Crash: &api.Crash{Time: time.Now().Add(time.Second), App: "com.app", Kind: api.CrashKindCrash, Exception: "SIGABRT"}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c1, c2 := net.Pipe()
	go ServeConn(ctx, svc, c2)
	defer c1.Close()
	rd := bufio.NewReader(c1)
	ask := func(method, params string) Response {
		fmt.Fprintf(c1, `{"id":1,"method":%q,"params":%s}`+"\n", method, params)
		line, err := rd.ReadBytes('\n')
		if err != nil {
			t.Fatal(err)
		}
		var r Response
		if err := json.Unmarshal(line, &r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	r := ask("ui", `{"device":"android:x"}`)
	if r.Error != "" || r.AppCrash == nil || r.AppCrash.Exception != "SIGABRT" {
		t.Fatalf("response %+v", r)
	}
	r = ask("tap", `{"device":"android:x","selector":{"text":"Pay"}}`)
	if r.Code != CodeAppCrashed || r.AppCrash == nil || !strings.Contains(r.Error, "no element matches") {
		t.Fatalf("response %+v", r)
	}
}
