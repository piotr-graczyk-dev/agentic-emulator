package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/client"
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/daemon"
)

// runningDaemon pings the daemon that runs now, without starting one. ok is false when none
// answers.
func runningDaemon() (p daemon.Ping, ok bool) {
	cl, ok := client.DialRunning()
	if !ok {
		return p, false
	}
	defer cl.Close()
	if _, err := cl.Call("ping", nil, &p); err != nil {
		return p, false
	}
	return p, true
}

// staleReason says why the running daemon is not the agemud this agemu would start now, or ""
// when it is. A daemon keeps running the binary it started from, so after a rebuild or an
// upgrade it serves old code until it restarts.
func staleReason(p daemon.Ping) string {
	r, _ := staleness(p)
	return r
}

// staleness is staleReason, and whether restarting the daemon is safe without asking: it is
// older than this agemu, so no session can be relying on something newer. A development build
// from another directory is reported but not restarted automatically, since two checkouts
// sharing one daemon would otherwise take turns restarting it.
func staleness(p daemon.Ping) (reason string, autoRestart bool) {
	if p.Started.IsZero() {
		return "it predates version reporting, so it is an older build", true
	}
	want, err := client.DaemonPath()
	if err != nil {
		return "", false
	}
	if r, err := filepath.EvalSymlinks(want); err == nil {
		want = r
	}
	if p.Exe == want {
		if st, err := os.Stat(want); err == nil && st.ModTime().After(p.Started) {
			return "agemud was rebuilt or reinstalled after the daemon started", true
		}
		return "", false
	}
	if versionOlder(p.Version, daemon.Version) {
		return fmt.Sprintf("it is version %s, this agemu is %s", p.Version, daemon.Version), true
	}
	_, releaseD := parseVersion(p.Version)
	_, releaseA := parseVersion(daemon.Version)
	if !releaseD || !releaseA { // development builds: no version order to go by
		return fmt.Sprintf("it runs %s, this agemu uses %s", p.Exe, want), false
	}
	return "", false // the same or a newer release, installed elsewhere
}

// daemonCommand runs `agemu daemon [status|start|stop|restart]`; with no subcommand it runs the
// daemon in the foreground.
func daemonCommand(args []string) error {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "":
		path, err := client.DaemonPath()
		if err != nil {
			return err
		}
		// Replace this process so signals reach the daemon directly.
		return syscall.Exec(path, []string{path}, os.Environ())
	case "status":
		p, ok := runningDaemon()
		if !ok {
			fmt.Println("agemud is not running (the next agemu command starts it)")
			return nil
		}
		fmt.Printf("agemud %s running, pid %d, since %s\n", p.Version, p.PID, p.Started.Local().Format(time.DateTime))
		if p.Exe != "" {
			fmt.Println("  binary:", p.Exe)
		}
		fmt.Println("  log:   ", client.LogPath())
		if r := staleReason(p); r != "" {
			fmt.Printf("  out of date: %s; run `agemu daemon restart`\n", r)
		}
		return nil
	case "start":
		return startDaemon()
	case "stop":
		stopped, err := stopDaemon()
		if err != nil {
			return err
		}
		if stopped {
			fmt.Println("agemud stopped (devices keep running)")
		} else {
			fmt.Println("agemud is not running")
		}
		return nil
	case "restart":
		// --if-running: only restart a daemon that runs (make install uses it).
		if parse(args[1:]).flags["if-running"] == "true" {
			if _, ok := runningDaemon(); !ok {
				return nil
			}
		}
		if _, err := stopDaemon(); err != nil {
			return err
		}
		return startDaemon()
	}
	return fmt.Errorf("usage: daemon [status|start|stop|restart]")
}

func startDaemon() error {
	cl, err := client.Dial()
	if err != nil {
		return err
	}
	defer cl.Close()
	var p daemon.Ping
	if _, err := cl.Call("ping", nil, &p); err != nil {
		return err
	}
	fmt.Printf("agemud %s running, pid %d\n", p.Version, p.PID)
	return nil
}

// stopDaemon stops the running daemon and waits until its socket is gone. Devices are not
// touched: emulators and simulators keep running and the next daemon finds them again.
func stopDaemon() (bool, error) {
	p, ok := runningDaemon()
	if !ok {
		return false, nil
	}
	if err := syscall.Kill(p.PID, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return false, fmt.Errorf("stopping agemud (pid %d): %w", p.PID, err)
	}
	gone := func(d time.Duration) bool {
		for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
			cl, ok := client.DialRunning()
			if !ok {
				return true
			}
			cl.Close()
		}
		return false
	}
	if gone(20 * time.Second) {
		return true, nil
	}
	_ = syscall.Kill(p.PID, syscall.SIGKILL)
	if gone(3 * time.Second) {
		return true, nil
	}
	return false, fmt.Errorf("agemud (pid %d) did not stop", p.PID)
}

// versionCommand prints agemu's version and the running daemon's.
func versionCommand() {
	fmt.Printf("agemu  %s\n", daemon.Version)
	if p, ok := runningDaemon(); ok {
		v, note := p.Version, ""
		if v == "" {
			v = "(older build)"
		}
		if r := staleReason(p); r != "" {
			note = " - out of date, run `agemu daemon restart`"
		}
		fmt.Printf("agemud %s (running, pid %d)%s\n", v, p.PID, note)
	} else {
		fmt.Println("agemud not running")
	}
}

// restartIfOutdated stops a running daemon that is older than this agemu, so the next call
// starts the current one. `agemu mcp` calls it once per session: after an upgrade, `npx` runs the
// new version from another directory while the old daemon still serves. A daemon that is newer,
// or the same version from another directory, is left alone, since another session may use it.
// Clients reconnect on their next call; devices keep running.
func restartIfOutdated() {
	p, ok := runningDaemon()
	if !ok {
		return
	}
	if _, restart := staleness(p); !restart {
		return
	}
	fmt.Fprintf(os.Stderr, "agemu: restarting agemud %s (pid %d) on %s\n", p.Version, p.PID, daemon.Version)
	if _, err := stopDaemon(); err != nil {
		fmt.Fprintln(os.Stderr, "agemu:", err)
	}
}

// versionOlder reports whether release version a is older than b ("v0.1.0", "0.2.0" or git
// describe's "v0.1.0-3-gabc1234"). Anything else (dev builds, bare commits) compares as not older.
func versionOlder(a, b string) bool {
	pa, oka := parseVersion(a)
	pb, okb := parseVersion(b)
	if !oka || !okb {
		return false
	}
	for i := range pa {
		if pa[i] != pb[i] {
			return pa[i] < pb[i]
		}
	}
	return false
}

func parseVersion(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimPrefix(v, "v")
	v, _, _ = strings.Cut(v, "-")
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, s := range parts {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}
