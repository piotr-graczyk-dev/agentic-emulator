package api

// Logs and crashes (docs/CAPABILITIES.md, "Logs and crashes"): the platform-neutral shapes of
// a log line, a crash report and the crash notice every call on a device can carry.

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// LogLevel is a platform-neutral log priority. The levels, lowest first:
//
//	level    Android               iOS (unified log type)
//	verbose  V                     -
//	debug    D                     debug (not captured: streaming it may turn it on in the app)
//	info     I                     info, default (os_log default, Logger.notice, NSLog)
//	warn     W                     - (Logger.warning logs as error)
//	error    E                     error
//	fatal    F, A (Log.wtf)        fault (Logger.critical)
type LogLevel string

const (
	LogVerbose LogLevel = "verbose"
	LogDebug   LogLevel = "debug"
	LogInfo    LogLevel = "info"
	LogWarn    LogLevel = "warn"
	LogError   LogLevel = "error"
	LogFatal   LogLevel = "fatal"
)

// LogLevels lists the levels, lowest first.
var LogLevels = []LogLevel{LogVerbose, LogDebug, LogInfo, LogWarn, LogError, LogFatal}

// Rank orders levels: 1 for verbose up to 6 for fatal, 0 for an unknown level.
func (l LogLevel) Rank() int {
	for i, k := range LogLevels {
		if l == k {
			return i + 1
		}
	}
	return 0
}

// Letter is the level's one-letter form, as logcat prints it (V D I W E F).
func (l LogLevel) Letter() string {
	if l.Rank() == 0 {
		return "?"
	}
	return strings.ToUpper(string(l)[:1])
}

// ParseLogLevel accepts a level name, its letter (v d i w e f), "warning", "err" and "fault"
// (iOS), in any case.
func ParseLogLevel(s string) (LogLevel, error) {
	v := strings.ToLower(strings.TrimSpace(s))
	switch v {
	case "warning":
		return LogWarn, nil
	case "err":
		return LogError, nil
	case "fault", "assert", "a":
		return LogFatal, nil
	}
	for _, l := range LogLevels {
		if v == string(l) || (len(v) == 1 && v == string(l)[:1]) {
			return l, nil
		}
	}
	names := make([]string, len(LogLevels))
	for i, l := range LogLevels {
		names[i] = string(l)
	}
	return "", fmt.Errorf("unknown log level %q: use %s (or v, d, i, w, e, f)", s, strings.Join(names, ", "))
}

// LogEntry is one log line, the same shape on both platforms. Time is the host's wall clock
// (backends whose device clock differs correct it). A message can hold several lines (a stack
// trace logged in one call stays one entry).
type LogEntry struct {
	Time  time.Time `json:"time"`
	Level LogLevel  `json:"level"`
	Pid   int       `json:"pid"`
	Tid   int       `json:"tid,omitempty"`
	// Process is the process's name: Android its process name ("com.example.app",
	// "com.example.app:remote", "system_server"); iOS its executable ("AgemuTestApp", "SpringBoard").
	Process string `json:"process,omitempty"`
	// Tag is Android's log tag; on iOS "subsystem:category" (or the subsystem alone), else the
	// name of the image that logged it (an NSLog line: "Foundation").
	Tag     string `json:"tag,omitempty"`
	Message string `json:"message"`
}

// Size estimates the entry's size in a JSON or text result, for the byte bound of a logs call.
func (e LogEntry) Size() int { return len(e.Message) + len(e.Tag) + len(e.Process) + 64 }

// Line renders the entry as one text line (continuation lines of a message are indented):
//
//	10:00:00.123 E com.example.app[1234] ReactNativeJS: TypeError: undefined is not a function
func (e LogEntry) Line(loc *time.Location) string {
	var b strings.Builder
	b.WriteString(e.Time.In(loc).Format("15:04:05.000"))
	b.WriteByte(' ')
	b.WriteString(e.Level.Letter())
	b.WriteByte(' ')
	proc := e.Process
	if proc == "" {
		proc = "?"
	}
	b.WriteString(proc)
	b.WriteString("[" + strconv.Itoa(e.Pid) + "]")
	if e.Tag != "" {
		b.WriteString(" " + e.Tag)
	}
	b.WriteString(": ")
	b.WriteString(strings.ReplaceAll(strings.TrimRight(e.Message, "\n"), "\n", "\n    "))
	return b.String()
}

// Bounds of a logs call. The defaults keep a result small enough for an agent's context.
const (
	DefaultLogLimit = 100      // entries
	MaxLogLimit     = 10000    // entries
	DefaultLogBytes = 16 << 10 // bytes (LogEntry.Size)
	MinLogBytes     = 1 << 10
	MaxLogBytes     = 1 << 20
	// MaxLogMessage bounds one message; capture cuts longer ones (CutText).
	MaxLogMessage = 16 << 10
	// MaxLogPattern bounds a regex's length.
	MaxLogPattern = 1000
)

// LogQuery is what a logs call asks a backend for (backend.LogReader). The daemon has checked
// and normalized it: Level is a known level or "", Contains is lowercased, Regex compiles.
type LogQuery struct {
	App   string    // package name / bundle id: only the app's processes; "" for every process
	Since time.Time // entries at or after this time; zero: no bound
	After uint64    // a cursor: entries captured after it; 0: no bound
	// The filters below are hints: the daemon applies them again, and a backend may ignore them.
	Level    LogLevel // minimum level
	Contains string   // lowercased substring of the tag or the message
	Regex    string   // RE2 pattern, matched against the tag or the message
}

// LogBatch is a backend's answer to a LogQuery.
type LogBatch struct {
	// Entries match App, Since and After, oldest first. The daemon applies the bounds (limit,
	// bytes), so a backend returns every matching entry it holds.
	Entries []LogEntry
	// Cursor marks the newest entry the capture holds, of any process (or the capture's start
	// when it holds none). A later query with After = Cursor returns only newer entries.
	Cursor uint64
	// CapturedFrom is the oldest time the capture still holds for this query's processes
	// (the capture's start, or the oldest entry the ring buffer kept).
	CapturedFrom time.Time
	// Partial is set when the query's window (Since or After) reaches back before what the
	// capture holds, so some entries in it are missing.
	Partial bool
}

// Logs is the result of a logs call.
type Logs struct {
	Entries []LogEntry `json:"entries"` // oldest first; the newest that fit the bounds
	// Cursor: pass it as since to get only newer lines ("c" + a number).
	Cursor       string     `json:"cursor,omitempty"`
	Omitted      int        `json:"omitted,omitempty"` // older matching entries left out by the bounds
	Partial      bool       `json:"partial,omitempty"` // lines in the asked window were lost or not captured
	CapturedFrom *time.Time `json:"capturedFrom,omitempty"`
}

// FormatLogs renders a logs result as text lines, with "-- " lines at the end for what an agent
// needs next (omitted lines, a partial window, the cursor).
func FormatLogs(l Logs, loc *time.Location) string {
	var b strings.Builder
	for _, e := range l.Entries {
		b.WriteString(e.Line(loc))
		b.WriteByte('\n')
	}
	if len(l.Entries) == 0 {
		b.WriteString("no log lines match\n")
	}
	if l.Omitted > 0 {
		fmt.Fprintf(&b, "-- %d older matching lines omitted (raise limit or max bytes, or narrow the filter)\n", l.Omitted)
	}
	if l.Partial {
		from := "the capture's start"
		if l.CapturedFrom != nil {
			from = l.CapturedFrom.In(loc).Format("15:04:05.000")
		}
		fmt.Fprintf(&b, "-- partial: lines before %s were not captured or were dropped\n", from)
	}
	if l.Cursor != "" {
		fmt.Fprintf(&b, "-- cursor %s (pass it as since to get only newer lines)\n", l.Cursor)
	}
	return b.String()
}

// LogCursor formats a backend cursor for a logs result.
func LogCursor(n uint64) string { return "c" + strconv.FormatUint(n, 10) }

// Since is a parsed since argument of logs and crashes.
type Since struct {
	Time   time.Time // zero: no bound
	Cursor uint64    // logs only: entries after this cursor
	Launch bool      // the app's last launch through agemu (the daemon resolves it)
}

// SinceHelp lists the accepted forms of since.
const SinceHelp = `use a time (2026-11-07T10:00:00Z), a duration back from now (30s, 5m), "launch" (the app's last launch through agemu) or a cursor from a previous logs result (c123...)`

// ParseSince accepts "", an RFC 3339 time, a positive duration back from now ("90s", "5m"),
// "launch", or a logs cursor ("c" + digits).
func ParseSince(s string, now time.Time) (Since, error) {
	v := strings.TrimSpace(s)
	switch {
	case v == "":
		return Since{}, nil
	case strings.EqualFold(v, "launch"):
		return Since{Launch: true}, nil
	case len(v) > 1 && v[0] == 'c' && v[1] >= '0' && v[1] <= '9':
		n, err := strconv.ParseUint(v[1:], 10, 64)
		if err != nil || n == 0 {
			return Since{}, fmt.Errorf("since %q: not a cursor; %s", s, SinceHelp)
		}
		return Since{Cursor: n}, nil
	}
	if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
		return Since{Time: t}, nil
	}
	if d, err := time.ParseDuration(v); err == nil {
		if d <= 0 {
			return Since{}, fmt.Errorf("since %q: a duration must be positive (how far back from now)", s)
		}
		return Since{Time: now.Add(-d)}, nil
	}
	return Since{}, fmt.Errorf("since %q: %s", s, SinceHelp)
}

// CompileLogRegex checks and compiles a logs regex (RE2 syntax).
func CompileLogRegex(pattern string) (*regexp.Regexp, error) {
	if len(pattern) > MaxLogPattern {
		return nil, fmt.Errorf("regex is %d bytes, more than %d", len(pattern), MaxLogPattern)
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("bad regex: %v", err)
	}
	return re, nil
}

// CutText shortens s to at most max bytes (on a rune boundary), noting how much was cut.
func CutText(s string, max int) string {
	if len(s) <= max {
		return s
	}
	note := fmt.Sprintf("...[%d bytes cut]", len(s)-max)
	keep := max - len(note)
	if keep < 0 {
		keep = 0
	}
	for keep > 0 && !utf8.RuneStart(s[keep]) {
		keep--
	}
	return s[:keep] + note
}

// CrashKind says how an app's process ended.
type CrashKind string

const (
	// CrashKindCrash: the process died of its own fault: an uncaught exception (Java/Kotlin,
	// NSException, a Swift runtime error), a fatal signal (native crash) or abort.
	CrashKindCrash CrashKind = "crash"
	// CrashKindANR: Android found the app not responding (its main thread blocked); the
	// system shows its dialog or kills the app.
	CrashKindANR CrashKind = "anr"
	// CrashKindKilled: the system killed the process for a reason of its own (iOS watchdog or
	// jetsam; Android low memory or excessive resource use). Stops agemu asked for (terminate,
	// launch --relaunch, uninstall, clear_app_data, permission revoke or reset, reset) are never
	// reported.
	CrashKindKilled CrashKind = "killed"
	// CrashKindExited: the process ended without a crash report and without agemu asking: the
	// app called exit(), or the platform has not written its report yet (iOS writes .ips files
	// a moment after the process ends; crashes shows the crash once it has).
	CrashKindExited CrashKind = "exited"
)

// Bounds of crash reports.
const (
	DefaultCrashLimit = 10
	MaxCrashLimit     = 100
	MaxCrashFrames    = 20      // frames of the crashing thread in a report
	MaxCrashText      = 8 << 10 // exception and reason, each
	NoticeFrames      = 3       // frames in the notice an action result carries
	noticeReason      = 300     // bytes of the reason in a notice's summary
)

// Crash is one crash, ANR or system kill of an app's process.
type Crash struct {
	Time    time.Time `json:"time"`              // when it happened (host wall clock)
	App     string    `json:"app"`               // package name / bundle id
	Process string    `json:"process,omitempty"` // the process's name (see LogEntry.Process)
	Pid     int       `json:"pid,omitempty"`
	Kind    CrashKind `json:"kind"`
	// Exception is the most specific type known: the language exception's class or name
	// (java.lang.IllegalStateException, NSInvalidArgumentException), else the signal or Mach
	// exception (SIGSEGV (SEGV_MAPERR), EXC_BAD_ACCESS (SIGSEGV)); for kills, the mechanism.
	Exception string `json:"exception,omitempty"`
	// Reason is the exception's message, the termination reason, or the ANR's subject.
	Reason string `json:"reason,omitempty"`
	Thread string `json:"thread,omitempty"` // the crashing thread ("main", "Thread 3")
	// Frames is the top of the crashing thread's stack (iOS, for an uncaught NSException: the
	// exception's backtrace), innermost first, as the platform's report prints each frame.
	Frames []string `json:"frames,omitempty"`
	// Report locates the full report: a host path (iOS .ips) or the device's record (Android
	// dropbox tag and time, or a tombstone path). Empty while the platform is still writing it.
	Report string `json:"report,omitempty"`
}

// Clamp bounds a crash's text fields and frames to at most frames frames.
func (c Crash) Clamp(frames int) Crash {
	c.Exception = CutText(c.Exception, MaxCrashText)
	c.Reason = CutText(c.Reason, MaxCrashText)
	if len(c.Frames) > frames {
		c.Frames = append([]string(nil), c.Frames[:frames]...)
	}
	return c
}

// CrashQuery is what a crashes call asks a backend for (backend.CrashReader).
type CrashQuery struct {
	App   string    // only this app's reports; "" for every app
	Since time.Time // reports at or after this time; zero: no bound
}

// Crashes is the result of a crashes call.
type Crashes struct {
	Crashes []Crash `json:"crashes"`           // oldest first; the newest that fit the limit
	Omitted int     `json:"omitted,omitempty"` // older reports left out by the limit
}

// FormatCrashes renders a crashes result as text.
func FormatCrashes(cs Crashes, loc *time.Location) string {
	var b strings.Builder
	for _, c := range cs.Crashes {
		fmt.Fprintf(&b, "%s %s %s", c.Time.In(loc).Format("2006-01-02 15:04:05.000"), c.Kind, c.App)
		if c.Process != "" && c.Process != c.App {
			b.WriteString(" (" + c.Process + ")")
		}
		if c.Pid > 0 {
			fmt.Fprintf(&b, " pid %d", c.Pid)
		}
		b.WriteByte('\n')
		if head := c.headline(); head != "" {
			b.WriteString("  " + strings.ReplaceAll(head, "\n", "\n  ") + "\n")
		}
		if c.Thread != "" {
			b.WriteString("  thread " + c.Thread + "\n")
		}
		for _, f := range c.Frames {
			b.WriteString("    " + f + "\n")
		}
		if c.Report != "" {
			b.WriteString("  report " + c.Report + "\n")
		}
	}
	if len(cs.Crashes) == 0 {
		b.WriteString("no crashes\n")
	}
	if cs.Omitted > 0 {
		fmt.Fprintf(&b, "-- %d older reports omitted (raise limit or pass since)\n", cs.Omitted)
	}
	return b.String()
}

// headline is "exception: reason", or either alone.
func (c Crash) headline() string {
	switch {
	case c.Exception != "" && c.Reason != "":
		return c.Exception + ": " + c.Reason
	case c.Exception != "":
		return c.Exception
	}
	return c.Reason
}

// AppState is what a backend's watcher knows about one app (backend.AppWatcher).
type AppState struct {
	Running bool   // the app has a live process now
	Crash   *Crash // the newest crash, ANR or system kill after the asked time; nil when none
}

// AppCrash is the notice a call's result carries when the app the agent launched through
// agemu crashed, stopped responding, was killed by the system or exited on its own (see
// CrashKind).
type AppCrash struct {
	Crash
	Running bool `json:"running"` // whether the app has a live process again (or still, for an ANR)
}

// Summary says what happened in one line, for an agent reading a call's result or error.
func (c AppCrash) Summary() string {
	var what string
	switch c.Kind {
	case CrashKindANR:
		what = "stopped responding (ANR)"
	case CrashKindKilled:
		what = "was killed by the system"
	case CrashKindExited:
		what = "exited unexpectedly"
	default:
		what = "crashed"
	}
	s := fmt.Sprintf("%s %s at %s", c.App, what, c.Time.Local().Format("15:04:05.000"))
	if head := c.headline(); head != "" {
		s += ": " + CutText(strings.Join(strings.Fields(head), " "), noticeReason)
	}
	if c.Running {
		s += "; it is running"
	} else {
		s += "; it is not running"
	}
	if c.Kind == CrashKindExited {
		return s + "; crashes shows a report if one follows"
	}
	return s + "; crashes has the report"
}

// ErrAppCrashed matches (errors.Is) the error of a call that failed while the app the agent
// launched was down after a crash, an ANR, a system kill or an unexpected exit, also after a
// round trip through the daemon socket (code "app_crashed").
var ErrAppCrashed = errors.New("the app crashed")

// AppCrashedError is a call's own error with the crash that explains it.
type AppCrashedError struct {
	Crash AppCrash
	Err   error
}

func (e *AppCrashedError) Error() string { return e.Err.Error() + " (" + e.Crash.Summary() + ")" }

func (e *AppCrashedError) Unwrap() []error { return []error{e.Err, ErrAppCrashed} }
