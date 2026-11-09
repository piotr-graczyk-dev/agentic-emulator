package main

import (
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
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
		{"name":"iOS 26.9","version":"26.9","platform":"iOS","identifier":"com.apple.CoreSimulator.SimRuntime.iOS-26-9","isAvailable":true},
		{"name":"iOS 26.10","version":"26.10","platform":"iOS","identifier":"com.apple.CoreSimulator.SimRuntime.iOS-26-10","isAvailable":true},
		{"name":"iOS 27.0","version":"27.0","platform":"iOS","identifier":"com.apple.CoreSimulator.SimRuntime.iOS-27-0","isAvailable":false},
		{"name":"watchOS 26.5","version":"26.5","platform":"watchOS","identifier":"com.apple.CoreSimulator.SimRuntime.watchOS-26-5","isAvailable":true}
	]}`
	if got := newestIOSRuntime(raw); got != "iOS 26.10" {
		t.Errorf("newest = %q, want iOS 26.10", got)
	}
	if !iosRuntimeAvailable(raw, "com.apple.CoreSimulator.SimRuntime.iOS-26-5") ||
		iosRuntimeAvailable(raw, "com.apple.CoreSimulator.SimRuntime.iOS-27-0") ||
		iosRuntimeAvailable(raw, "com.apple.CoreSimulator.SimRuntime.iOS-18-0") {
		t.Error("iosRuntimeAvailable: want installed-and-available only")
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
		{"v0.1.0", "v0.1.0-3-gabc1234", true},  // commits after the tag are newer
		{"v0.1.0-3-gabc1234", "v0.1.0", false}, //
		{"v0.2.0-rc.1", "v0.2.0", true},        // a prerelease precedes its release
		{"v0.2.0", "v0.2.0-rc.1", false},       //
		{"v0.2.0-rc.1", "v0.2.0-rc.2", true},   //
		{"v0.2.0-rc.2", "v0.2.0-rc.10", true},  // numeric identifiers compare as numbers
		{"v0.2.0-alpha", "v0.2.0-beta", true},  //
		{"v0.2.0-rc.1", "v0.2.0-rc.1-2-gabc", true},
		{"v0.1.0-dirty", "v0.1.0", false},
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
	// npm upgrades in place and gives files the 1985 mtime it stores: only the version tells.
	real, _ := filepath.EvalSymlinks(agemud)
	npm := time.Date(1985, 10, 26, 8, 15, 0, 0, time.UTC)
	_ = os.Chtimes(agemud, npm, npm)
	inPlace := daemon.Ping{PID: 1, Exe: real, Started: time.Now().Add(-time.Hour), Version: "v0.1.0"}
	if reason, restart := staleness(inPlace); reason == "" || !restart {
		t.Errorf("in-place upgrade with an old mtime: reason %q, restart %v", reason, restart)
	}
	inPlace.Version = daemon.Version
	if reason, _ := staleness(inPlace); reason != "" {
		t.Errorf("same version in place reported stale: %q", reason)
	}
}

// fakeHungDaemon is what the test binary runs as when TestStopAHungDaemon starts it: a daemon
// that holds the lock, names itself in it and accepts connections, but never answers.
func fakeHungDaemon() {
	lock, err := os.OpenFile(daemon.LockPath(), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		os.Exit(3)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		os.Exit(4)
	}
	_, _ = lock.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	ln, err := net.Listen("unix", daemon.SocketPath())
	if err != nil {
		os.Exit(5)
	}
	signal.Ignore(syscall.SIGTERM) // stuck: only SIGKILL ends it
	for {
		c, err := ln.Accept()
		if err != nil {
			os.Exit(6)
		}
		defer c.Close() // read nothing, answer nothing
	}
}

func TestMain(m *testing.M) {
	if os.Getenv("AGEMU_TEST_FAKE_HUNG_DAEMON") == "1" {
		fakeHungDaemon()
	}
	os.Exit(m.Run())
}

func TestStopAHungDaemon(t *testing.T) {
	defer func(g time.Duration) { stopGrace = g }(stopGrace)
	stopGrace = 500 * time.Millisecond              // the fake ignores SIGTERM: it gets SIGKILL after this
	home, err := os.MkdirTemp("/tmp", "agemu-hung") // unix socket paths are limited to 104 bytes
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)
	t.Setenv("AGEMU_HOME", home)
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "AGEMU_TEST_FAKE_HUNG_DAEMON=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// No cmd.Wait: like an MCP session that started the daemon, the parent leaves it a zombie.
	for end := time.Now().Add(5 * time.Second); lockPID() != cmd.Process.Pid; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("fake daemon did not start")
		}
	}
	for end := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if c, err := net.Dial("unix", daemon.SocketPath()); err == nil {
			c.Close()
			break
		}
		if time.Now().After(end) {
			t.Fatal("fake daemon does not listen")
		}
	}

	start := time.Now()
	if _, state := probeDaemon(); state != daemonHung {
		t.Fatalf("state = %v, want daemonHung", state)
	}
	if d := time.Since(start); d > pingTimeout+time.Second {
		t.Fatalf("probe took %v", d)
	}
	stopped, err := stopDaemon()
	if err != nil || !stopped {
		t.Fatalf("stopDaemon = %v, %v", stopped, err)
	}
	if !daemonGone(cmd.Process.Pid) {
		t.Fatal("the daemon is not gone")
	}
	if _, state := probeDaemon(); state != daemonAbsent {
		t.Fatalf("after stop: state = %v", state)
	}
}

// fakePingDaemon answers ping with p on $AGEMU_HOME's socket until the test ends.
func fakePingDaemon(t *testing.T, p daemon.Ping) {
	t.Helper()
	ln, err := net.Listen("unix", daemon.SocketPath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				dec := json.NewDecoder(conn)
				for {
					var req daemon.Request
					if dec.Decode(&req) != nil {
						return
					}
					res, _ := json.Marshal(p)
					resp, _ := json.Marshal(daemon.Response{ID: req.ID, Result: res})
					if _, err := conn.Write(append(resp, '\n')); err != nil {
						return
					}
				}
			}()
		}
	}()
}

// An agemu mcp session does not restart an older daemon while it prepares a device: that would
// cancel minutes of work. Without a preparation it does.
func TestRestartIfOutdatedSkipsAPreparingDaemon(t *testing.T) {
	defer func(v string) { daemon.Version = v }(daemon.Version)
	daemon.Version = "v0.2.0"
	for _, preparing := range [][]string{{"android:agemu-pixel-9"}, nil} {
		home, err := os.MkdirTemp("/tmp", "agemu-prep") // unix socket paths are limited to 104 bytes
		if err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(home)
		t.Setenv("AGEMU_HOME", home)
		t.Setenv("AGEMU_DAEMON", filepath.Join(home, "agemud"))
		old := exec.Command("sleep", "30") // stands in for the old daemon's process
		if err := old.Start(); err != nil {
			t.Fatal(err)
		}
		exited := make(chan struct{})
		go func() { old.Wait(); close(exited) }()
		fakePingDaemon(t, daemon.Ping{OK: true, PID: old.Process.Pid, Version: "v0.1.0", Exe: "/elsewhere/agemud",
			Started: time.Now(), Preparing: preparing})

		restartIfOutdated()
		select {
		case <-exited:
			if preparing != nil {
				t.Fatalf("restarted a daemon preparing %v", preparing)
			}
		case <-time.After(300 * time.Millisecond):
			if preparing == nil {
				t.Fatal("did not restart an older daemon")
			}
			old.Process.Kill()
		}
	}
}
