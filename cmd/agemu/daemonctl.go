package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/client"
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/daemon"
)

type daemonState int

const (
	daemonAbsent daemonState = iota // nothing listens on the socket
	daemonUp                        // it answered ping
	daemonHung                      // it accepts connections but did not answer in pingTimeout
)

// pingTimeout bounds the health check, so doctor, version, stop and MCP start-up cannot hang
// on a daemon that stopped answering.
const pingTimeout = 3 * time.Second

// probeDaemon pings the daemon that runs now, without starting one.
func probeDaemon() (daemon.Ping, daemonState) {
	var p daemon.Ping
	cl, ok := client.DialRunning()
	if !ok {
		return p, daemonAbsent
	}
	defer cl.Close()
	_ = cl.SetDeadline(time.Now().Add(pingTimeout))
	if _, err := cl.Call("ping", nil, &p); err != nil {
		if errors.Is(err, os.ErrDeadlineExceeded) {
			return p, daemonHung
		}
		return p, daemonAbsent // it closed the connection: shutting down
	}
	return p, daemonUp
}

// runningDaemon is probeDaemon for callers that only need a daemon that answers.
func runningDaemon() (daemon.Ping, bool) {
	p, s := probeDaemon()
	return p, s == daemonUp
}

// hungMessage describes a daemon that does not answer.
func hungMessage() string {
	who := "agemud"
	if pid := lockPID(); pid != 0 {
		who = fmt.Sprintf("agemud (pid %d)", pid)
	}
	return fmt.Sprintf("%s accepts connections but did not answer within %v", who, pingTimeout)
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
		// An upgrade in place: a rebuild has a newer modification time, but npm and tar keep
		// the package's own (npm's is from 1985), so compare versions too.
		if versionOlder(p.Version, daemon.Version) {
			return fmt.Sprintf("it is version %s, this agemu is %s", p.Version, daemon.Version), true
		}
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
		p, state := probeDaemon()
		switch state {
		case daemonAbsent:
			fmt.Println("agemud is not running (the next agemu command starts it)")
			return nil
		case daemonHung:
			fmt.Printf("%s; run `agemu daemon restart`\n  log: %s\n", hungMessage(), client.LogPath())
			return nil
		}
		fmt.Printf("agemud %s running, pid %d, since %s\n", p.Version, p.PID, p.Started.Local().Format(time.DateTime))
		if p.Exe != "" {
			fmt.Println("  binary:", p.Exe)
		}
		fmt.Println("  log:   ", client.LogPath())
		if len(p.Preparing) > 0 {
			fmt.Printf("  preparing: %s (a stop or restart now cancels it; the next boot starts over)\n", strings.Join(p.Preparing, ", "))
		}
		if r := staleReason(p); r != "" {
			fmt.Printf("  out of date: %s; run `agemu daemon restart`\n", r)
		}
		return nil
	case "start":
		return startDaemon()
	case "stop":
		warnPreparing()
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
		// --if-running: only restart a daemon that runs, and not while it prepares a device (make
		// install and install.sh use it; an unasked restart must not cancel minutes of work).
		if parse(args[1:]).flags["if-running"] == "true" {
			p, state := probeDaemon()
			if state == daemonAbsent {
				return nil
			}
			if len(p.Preparing) > 0 {
				fmt.Printf("agemud not restarted: %s. Run `agemu daemon restart` once it is done (agemu mcp also restarts an older daemon then).\n", preparingText(p))
				return nil
			}
		} else {
			warnPreparing()
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

// stopDaemon stops the running daemon, also one that no longer answers, and waits until it has
// exited. Devices are not touched: emulators and simulators keep running and the next daemon
// finds them again.
func stopDaemon() (bool, error) {
	p, state := probeDaemon()
	switch state {
	case daemonAbsent:
		return false, nil
	case daemonHung:
		pid := lockPID()
		if pid == 0 {
			return false, fmt.Errorf("%s, and %s does not name its pid; stop it with `pkill -x agemud`", hungMessage(), daemon.LockPath())
		}
		return true, stopPID(pid)
	}
	return true, stopPID(p.PID)
}

// stopGrace is how long a daemon gets to exit after SIGTERM before it is killed. It exceeds the
// bound on resuming the devices the daemon auto-parked (30 s, internal/daemon/autopark.go), which
// it does on its way out.
var stopGrace = 35 * time.Second

// preparingText names the devices a running daemon is preparing.
func preparingText(p daemon.Ping) string {
	verb := "is"
	if len(p.Preparing) > 1 {
		verb = "are"
	}
	return fmt.Sprintf("%s %s preparing a baseline", strings.Join(p.Preparing, ", "), verb)
}

// warnPreparing says what an asked-for stop costs: it cancels the preparations in progress.
func warnPreparing() {
	if p, ok := runningDaemon(); ok && len(p.Preparing) > 0 {
		fmt.Fprintf(os.Stderr, "agemu: %s; stopping agemud cancels it and the next boot starts over\n", preparingText(p))
	}
}

// stopPID stops the daemon with this pid and no other: when another session has already
// replaced it, the lock file names the new daemon and there is nothing to do.
func stopPID(pid int) error {
	if pid <= 0 { // kill(0) or kill(-n) would signal process groups
		return fmt.Errorf("agemud reported an invalid pid %d", pid)
	}
	if lp := lockPID(); lp != 0 && lp != pid {
		return nil
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return fmt.Errorf("stopping agemud (pid %d): %w", pid, err)
	}
	wait := func(d time.Duration) bool {
		for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
			if daemonGone(pid) {
				return true
			}
		}
		return false
	}
	if wait(stopGrace) {
		return nil
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	if wait(3 * time.Second) {
		return nil
	}
	return fmt.Errorf("agemud (pid %d) did not stop", pid)
}

// daemonGone reports whether the daemon with this pid has exited. The pid alone does not say:
// a daemon started by an agemu that still runs (an MCP session) stays a zombie after it exits.
// Its flock is released at exit, though, and a new daemon writes its own pid to the lock file.
func daemonGone(pid int) bool {
	if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		return true
	}
	if lp := lockPID(); lp != 0 && lp != pid {
		return true
	}
	return !lockHeld()
}

// lockPID is the pid the running daemon wrote to its lock file, or 0 (no daemon, or one that
// predates writing it).
func lockPID() int {
	b, err := os.ReadFile(daemon.LockPath())
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if pid <= 0 { // kill(0) or kill(-n) would signal process groups
		return 0
	}
	return pid
}

// lockHeld reports whether a daemon holds the lock file's flock.
func lockHeld() bool {
	f, err := os.Open(daemon.LockPath())
	if err != nil {
		return false
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		return true
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}

// versionCommand prints agemu's version and the running daemon's.
func versionCommand() {
	fmt.Printf("agemu  %s\n", daemon.Version)
	p, state := probeDaemon()
	if state == daemonHung {
		fmt.Printf("agemud: %s; run `agemu daemon restart`\n", hungMessage())
		return
	}
	if state == daemonUp {
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
	if len(p.Preparing) > 0 {
		// A restart would cancel minutes of work; the next session restarts it instead.
		fmt.Fprintf(os.Stderr, "agemu: agemud %s (pid %d) is older than %s but not restarted: %s\n", p.Version, p.PID, daemon.Version, preparingText(p))
		return
	}
	fmt.Fprintf(os.Stderr, "agemu: restarting agemud %s (pid %d) on %s\n", p.Version, p.PID, daemon.Version)
	// Only the daemon checked above: another session may already have started its successor.
	if err := stopPID(p.PID); err != nil {
		fmt.Fprintln(os.Stderr, "agemu:", err)
	}
}

// versionOlder reports whether version a is older than b, in semver order: "v0.2.0-rc.1" <
// "v0.2.0" < "v0.2.0-3-gabc1234" (git describe: commits after the tag) < "v0.2.1". Versions
// that are not semver (dev builds, bare commits) compare as not older.
func versionOlder(a, b string) bool {
	va, oka := parseVersion(a)
	vb, okb := parseVersion(b)
	return oka && okb && compareVersions(va, vb) < 0
}

type version struct {
	core [3]int
	pre  []string // prerelease identifiers ("rc", "1")
	post bool     // git describe: commits after the tag, or a dirty tree
}

var describeSuffix = regexp.MustCompile(`-[0-9]+-g[0-9a-f]+(-dirty)?$|-dirty$`)

func parseVersion(s string) (version, bool) {
	var v version
	s = strings.TrimPrefix(s, "v")
	s, _, _ = strings.Cut(s, "+") // build metadata does not order
	if loc := describeSuffix.FindStringIndex(s); loc != nil {
		s, v.post = s[:loc[0]], true
	}
	core, pre, hasPre := strings.Cut(s, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return v, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return v, false
		}
		v.core[i] = n
	}
	if hasPre {
		if pre == "" {
			return v, false
		}
		v.pre = strings.Split(pre, ".")
	}
	return v, true
}

func compareVersions(a, b version) int {
	for i := range a.core {
		if a.core[i] != b.core[i] {
			return cmpInt(a.core[i], b.core[i])
		}
	}
	switch {
	case len(a.pre) == 0 && len(b.pre) > 0:
		return 1
	case len(a.pre) > 0 && len(b.pre) == 0:
		return -1
	}
	for i := 0; i < len(a.pre) && i < len(b.pre); i++ {
		if c := comparePre(a.pre[i], b.pre[i]); c != 0 {
			return c
		}
	}
	if c := cmpInt(len(a.pre), len(b.pre)); c != 0 {
		return c
	}
	switch {
	case a.post && !b.post:
		return 1
	case !a.post && b.post:
		return -1
	}
	return 0
}

// comparePre orders one prerelease identifier: numbers numerically and before words.
func comparePre(a, b string) int {
	na, ea := strconv.Atoi(a)
	nb, eb := strconv.Atoi(b)
	switch {
	case ea == nil && eb == nil:
		return cmpInt(na, nb)
	case ea == nil:
		return -1
	case eb == nil:
		return 1
	}
	return strings.Compare(a, b)
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
