package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/daemon"
)

func TestShellQuote(t *testing.T) {
	for in, want := range map[string]string{
		"sdkmanager": "sdkmanager",
		"system-images;android-37.1;google_apis;arm64-v8a": "'system-images;android-37.1;google_apis;arm64-v8a'",
		"/Applications/Android Studio.app":                 "'/Applications/Android Studio.app'",
		"it's":                                             `'it'\''s'`,
		"":                                                 "''",
	} {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestNewestIOSRuntime(t *testing.T) {
	raw := `{"runtimes":[
		{"name":"iOS 18.6","version":"18.6","platform":"iOS","identifier":"com.apple.CoreSimulator.SimRuntime.iOS-18-6","isAvailable":true},
		{"name":"iOS 26.5","version":"26.5","platform":"iOS","identifier":"com.apple.CoreSimulator.SimRuntime.iOS-26-5","isAvailable":true},
		{"name":"iOS 27.0","version":"27.0","platform":"iOS","identifier":"com.apple.CoreSimulator.SimRuntime.iOS-27-0","isAvailable":false},
		{"name":"watchOS 26.5","version":"26.5","platform":"watchOS","identifier":"com.apple.CoreSimulator.SimRuntime.watchOS-26-5","isAvailable":true}
	]}`
	if got := newestIOSRuntime(raw); got != "iOS 26.5" {
		t.Errorf("newest = %q, want iOS 26.5", got)
	}
	if got := newestIOSRuntime(`{"runtimes":[]}`); got != "" {
		t.Errorf("no runtimes: %q", got)
	}
}

func TestStaleReason(t *testing.T) {
	dir := t.TempDir()
	agemud := filepath.Join(dir, "agemud")
	if err := os.WriteFile(agemud, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGEMU_DAEMON", agemud)
	real, _ := filepath.EvalSymlinks(agemud)
	built := time.Now().Add(-time.Hour)
	_ = os.Chtimes(agemud, built, built)

	current := daemon.Ping{PID: 1, Exe: real, Started: time.Now(), Version: daemon.Version}
	if r := staleReason(current); r != "" {
		t.Errorf("current daemon reported stale: %s", r)
	}
	if r := staleReason(daemon.Ping{PID: 1}); r == "" {
		t.Error("a daemon without a start time (older build) is not reported stale")
	}
	if r := staleReason(daemon.Ping{PID: 1, Exe: "/elsewhere/agemud", Started: time.Now()}); r == "" {
		t.Error("a daemon running another binary is not reported stale")
	}
	old := current
	old.Started = built.Add(-time.Minute)
	if r := staleReason(old); r == "" {
		t.Error("a daemon started before agemud was rebuilt is not reported stale")
	}
}

func TestVersionOlder(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"v0.1.0", "v0.2.0", true},
		{"0.1.9", "v0.1.10", true},
		{"v0.2.0", "v0.1.0", false},
		{"v0.1.0", "v0.1.0", false},
		{"v0.1.0-3-gabc1234", "v0.1.1", true},
		{"31d48a9-dirty", "v0.1.0", false},
		{"dev", "v0.1.0", false},
	} {
		if got := versionOlder(c.a, c.b); got != c.want {
			t.Errorf("versionOlder(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestStalenessAcrossInstallations(t *testing.T) {
	dir := t.TempDir()
	agemud := filepath.Join(dir, "agemud")
	if err := os.WriteFile(agemud, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGEMU_DAEMON", agemud)
	old := daemon.Version
	defer func() { daemon.Version = old }()
	daemon.Version = "v0.2.0"
	elsewhere := daemon.Ping{PID: 1, Exe: "/npx/cache/agemud", Started: time.Now()}

	for _, c := range []struct {
		running        string
		stale, restart bool
	}{
		{"v0.1.0", true, true},         // an older release: restart it
		{"v0.2.0", false, false},       // the same release from another directory
		{"v0.3.0", false, false},       // a newer release another session started: leave it
		{"abc1234-dirty", true, false}, // a development build: report, do not restart
	} {
		p := elsewhere
		p.Version = c.running
		reason, restart := staleness(p)
		if (reason != "") != c.stale || restart != c.restart {
			t.Errorf("daemon %s: reason %q, restart %v; want stale %v, restart %v", c.running, reason, restart, c.stale, c.restart)
		}
	}
}
