package api

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestParseLogLevel(t *testing.T) {
	for in, want := range map[string]LogLevel{
		"verbose": LogVerbose, "V": LogVerbose, "debug": LogDebug, "d": LogDebug, " Info ": LogInfo,
		"i": LogInfo, "warn": LogWarn, "WARNING": LogWarn, "w": LogWarn, "error": LogError, "err": LogError,
		"E": LogError, "fatal": LogFatal, "F": LogFatal, "fault": LogFatal, "assert": LogFatal, "a": LogFatal,
	} {
		got, err := ParseLogLevel(in)
		if err != nil || got != want {
			t.Fatalf("%q: %v %v, want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "loud", "x", "warnings"} {
		if _, err := ParseLogLevel(in); err == nil || !strings.Contains(err.Error(), "verbose, debug, info, warn, error, fatal") {
			t.Fatalf("%q: err = %v", in, err)
		}
	}
	for i := 1; i < len(LogLevels); i++ {
		if LogLevels[i].Rank() <= LogLevels[i-1].Rank() {
			t.Fatalf("%s does not rank above %s", LogLevels[i], LogLevels[i-1])
		}
	}
	if LogLevel("loud").Rank() != 0 || LogLevel("loud").Letter() != "?" || LogWarn.Letter() != "W" {
		t.Fatal("rank or letter of an unknown level")
	}
}

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		in   string
		want Since
	}{
		{"", Since{}},
		{"  ", Since{}},
		{"launch", Since{Launch: true}},
		{"Launch", Since{Launch: true}},
		{"c42", Since{Cursor: 42}},
		{"c1730970000123456", Since{Cursor: 1730970000123456}},
		{"2026-11-07T10:00:00Z", Since{Time: time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)}},
		{"2026-11-07T11:00:00.5+01:00", Since{Time: time.Date(2026, 11, 7, 10, 0, 0, 5e8, time.UTC)}},
		{"90s", Since{Time: now.Add(-90 * time.Second)}},
		{"1h30m", Since{Time: now.Add(-90 * time.Minute)}},
	}
	for _, c := range cases {
		got, err := ParseSince(c.in, now)
		if err != nil || !got.Time.Equal(c.want.Time) || got.Cursor != c.want.Cursor || got.Launch != c.want.Launch {
			t.Fatalf("%q: %+v %v, want %+v", c.in, got, err, c.want)
		}
	}
	for _, in := range []string{"yesterday", "c", "c0", "c12x", "-5m", "0s", "2026-11-07", "10:00:00", "5"} {
		if _, err := ParseSince(in, now); err == nil {
			t.Fatalf("%q: want an error", in)
		}
	}
}

func TestLogLines(t *testing.T) {
	at := time.Date(2026, 11, 7, 10, 0, 0, 123e6, time.UTC)
	e := LogEntry{Time: at, Level: LogError, Pid: 1234, Process: "com.example.app", Tag: "ReactNativeJS", Message: "TypeError: x\n    at f (index.js:1)\n"}
	want := "10:00:00.123 E com.example.app[1234] ReactNativeJS: TypeError: x\n        at f (index.js:1)"
	if got := e.Line(time.UTC); got != want {
		t.Fatalf("line\n got %q\nwant %q", got, want)
	}
	e = LogEntry{Time: at, Level: LogInfo, Pid: 7, Message: "hello"}
	if got := e.Line(time.UTC); got != "10:00:00.123 I ?[7]: hello" {
		t.Fatalf("line without process and tag: %q", got)
	}

	from := at.Add(-time.Minute)
	out := FormatLogs(Logs{Entries: []LogEntry{e}, Cursor: "c9", Omitted: 3, Partial: true, CapturedFrom: &from}, time.UTC)
	for _, s := range []string{"I ?[7]: hello\n", "-- 3 older matching lines omitted", "-- partial: lines before 09:59:00.123", "-- cursor c9 (pass it as since"} {
		if !strings.Contains(out, s) {
			t.Fatalf("FormatLogs lacks %q:\n%s", s, out)
		}
	}
	if out := FormatLogs(Logs{Entries: []LogEntry{}}, time.UTC); out != "no log lines match\n" {
		t.Fatalf("empty: %q", out)
	}
}

func TestFormatCrashes(t *testing.T) {
	at := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	out := FormatCrashes(Crashes{Crashes: []Crash{{
		Time: at, App: "com.app", Process: "com.app:remote", Pid: 42, Kind: CrashKindCrash,
		Exception: "java.lang.IllegalStateException", Reason: "boom", Thread: "main",
		Frames: []string{"at com.app.Main.onClick(Main.kt:42)"}, Report: "dropbox data_app_crash 1730973600000",
	}}, Omitted: 2}, time.UTC)
	want := "2026-11-07 10:00:00.000 crash com.app (com.app:remote) pid 42\n" +
		"  java.lang.IllegalStateException: boom\n" +
		"  thread main\n" +
		"    at com.app.Main.onClick(Main.kt:42)\n" +
		"  report dropbox data_app_crash 1730973600000\n" +
		"-- 2 older reports omitted (raise limit or pass since)\n"
	if out != want {
		t.Fatalf("got\n%s\nwant\n%s", out, want)
	}
	if out := FormatCrashes(Crashes{Crashes: []Crash{}}, time.UTC); out != "no crashes\n" {
		t.Fatalf("empty: %q", out)
	}
}

func TestCutText(t *testing.T) {
	if CutText("short", 10) != "short" {
		t.Fatal("cut a short text")
	}
	s := strings.Repeat("ż", 100) // 2 bytes each
	for _, max := range []int{0, 1, 20, 21, 57, 199} {
		got := CutText(s, max)
		if !utf8.ValidString(got) || !strings.Contains(got, "bytes cut]") {
			t.Fatalf("max %d: %q", max, got)
		}
		if max >= 20 && len(got) > max {
			t.Fatalf("max %d: %d bytes", max, len(got))
		}
	}
}

func TestCrashNoticeSummaryAndError(t *testing.T) {
	at := time.Date(2026, 11, 7, 10, 0, 0, 0, time.Local)
	cases := []struct {
		c    AppCrash
		want []string
	}{
		{AppCrash{Crash: Crash{Time: at, App: "com.app", Kind: CrashKindCrash, Exception: "java.lang.IllegalStateException", Reason: "boom\n  more"}},
			[]string{"com.app crashed at 10:00:00.000: java.lang.IllegalStateException: boom more; it is not running; crashes has the report"}},
		{AppCrash{Crash: Crash{Time: at, App: "com.app", Kind: CrashKindANR, Reason: "Input dispatching timed out"}, Running: true},
			[]string{"com.app stopped responding (ANR) at 10:00:00.000: Input dispatching timed out; it is running"}},
		{AppCrash{Crash: Crash{Time: at, App: "pl.app", Kind: CrashKindKilled, Exception: "jetsam", Reason: "per-process-limit"}},
			[]string{"pl.app was killed by the system at 10:00:00.000: jetsam: per-process-limit"}},
		{AppCrash{Crash: Crash{Time: at, App: "pl.app", Kind: CrashKindCrash, Reason: strings.Repeat("r", 5000)}},
			[]string{"bytes cut]"}},
		{AppCrash{Crash: Crash{Time: at, App: "pl.app", Kind: CrashKindExited}},
			[]string{"pl.app exited unexpectedly at 10:00:00.000; it is not running; crashes shows a report if one follows"}},
	}
	for _, c := range cases {
		s := c.c.Summary()
		for _, w := range c.want {
			if !strings.Contains(s, w) {
				t.Fatalf("summary %q lacks %q", s, w)
			}
		}
		if len(s) > 500 {
			t.Fatalf("summary of %d bytes", len(s))
		}
	}
	own := errors.New("no element matches {\"text\":\"Pay\"}")
	err := fmt.Errorf("tap done, but: %w", &AppCrashedError{Crash: cases[0].c, Err: own})
	if !errors.Is(err, ErrAppCrashed) || !errors.Is(err, own) || errors.Is(err, ErrNotImplemented) {
		t.Fatalf("errors.Is on %v", err)
	}
	if !strings.Contains(err.Error(), `no element matches {"text":"Pay"} (com.app crashed at`) {
		t.Fatalf("message %q", err)
	}
	c := Crash{Exception: strings.Repeat("e", 2*MaxCrashText), Frames: make([]string, 30)}.Clamp(NoticeFrames)
	if len(c.Exception) > MaxCrashText || len(c.Frames) != NoticeFrames {
		t.Fatalf("clamp: %d bytes, %d frames", len(c.Exception), len(c.Frames))
	}
}

func TestCompileLogRegex(t *testing.T) {
	if _, err := CompileLogRegex(`^ReactNativeJS$|TypeError`); err != nil {
		t.Fatal(err)
	}
	if _, err := CompileLogRegex(`(`); err == nil || !strings.Contains(err.Error(), "bad regex") {
		t.Fatalf("err = %v", err)
	}
	if _, err := CompileLogRegex(strings.Repeat("a", MaxLogPattern+1)); err == nil {
		t.Fatal("a pattern over the bound compiled")
	}
}
