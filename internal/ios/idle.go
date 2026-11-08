package ios

// Idle daemons after boot. iOS marks most of its on-demand daemons as safe to end when they are
// idle: their launchd jobs have EnablePressuredExit, and the kernel's dirty tracking (the same
// state the Mac's memorystatus keeps for the simulator's processes, since they are Mac
// processes) says "tracked, allows idle exit" and, between requests, "clean". A phone's jetsam
// ends such clean daemons first when it needs memory (JETSAM_REASON_MEMORY_IDLE_EXIT), and
// launchd starts them again at the next request; the simulator only does so when the Mac runs
// short of memory. After a cold boot iOS starts dozens of them for one-off work and they then
// sit idle. A lean simulator ends the ones that stay idle and clean, the way jetsam would, and
// leaves alone any that iOS starts again (see idlePasses); by default only those that serve no
// public app API (see appFacing).

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// idleKeepEnv: AGEMU_IOS_IDLE_KEEP=name,name... keeps these daemons (executable file names) too.
func idleKeepEnv() map[string]bool {
	m := map[string]bool{}
	for _, n := range strings.Split(os.Getenv("AGEMU_IOS_IDLE_KEEP"), ",") {
		if n = strings.TrimSpace(n); n != "" {
			m[n] = true
		}
	}
	return m
}

// idleExitOn: AGEMU_IOS_IDLE_EXIT=0 keeps idle daemons running on lean simulators.
func idleExitOn() bool {
	switch strings.ToLower(os.Getenv("AGEMU_IOS_IDLE_EXIT")) {
	case "0", "false", "no":
		return false
	}
	return true
}

// idleExitAll: AGEMU_IOS_IDLE_EXIT=all also ends the daemons behind public app APIs (appFacing).
func idleExitAll() bool { return strings.ToLower(os.Getenv("AGEMU_IOS_IDLE_EXIT")) == "all" }

// appFacing are idle-exit daemons that serve a public API an app under test calls. Ended, the
// app's first call after that waits for launchd to start the daemon again: Contacts and EventKit
// took about 2 s instead of 0.15 s, StoreKit, WatchConnectivity and the speech voice list about
// 0.9 s, MapKit directions up to 3 s (one probe run each way). So they are ended only with
// AGEMU_IOS_IDLE_EXIT=all; by default only daemons with no public API of their own are.
var appFacing = map[string]bool{
	"contactsd": true, "calaccessd": true, "remindd": true, // Contacts, EventKit
	"healthd": true, "usernotificationsd": true, "geod": true, // HealthKit, UserNotifications, MapKit
	"tccd": true, "securityd": true, "trustd": true, // privacy prompts, keychain, TLS trust
	"pasted": true, "mobiletimerd": true, "liveactivitiesd": true, // UIPasteboard, AlarmKit, ActivityKit
	"donotdisturbd": true, // Focus status
	// WatchConnectivity and the Watch pairing stack: with nanoregistrylaunchd and brookcompaniond
	// ended and nanoregistryd kept, pairedsyncd started and exited 4 times in 5 minutes (about 14
	// s of CPU) where it ran once at boot without the stop.
	"wcd": true, "nanoregistryd": true, "nanoregistrylaunchd": true, "brookcompaniond": true,
	"gamed": true, "linkd": true, "axassetsd": true, "callservicesd": true, // GameKit, App Intents, speech voices, CallKit
	"mediaremoted": true, "cloudd": true, "syncdefaultsd": true, // Now Playing, CloudKit, iCloud key-value store
	"amsaccountsd": true, "akd": true, "accountsd": true, "swcd": true, // StoreKit accounts, Sign in with Apple, Accounts, associated domains
	"searchd": true, "assetsd": true, "financed": true, "filecoordinationd": true, // CoreSpotlight, Photos, FinanceKit, NSFileCoordinator
	"fontservicesd": true, "containermanagerd": true, // font registration, app and group containers
	"installd": true, "installcoordinationd": true, // app installs (agemu install)
}

// The kernel's dirty-tracking flags of a process (proc_get_dirty: PROC_DIRTY_*).
const (
	dirtyTracked        = 0x1
	dirtyAllowsIdleExit = 0x2
	dirtyIsDirty        = 0x4
)

// idleExitable: the process takes part in dirty tracking, allows idle exit, and is clean now
// (no open transaction).
func idleExitable(flags uint32) bool {
	return flags&(dirtyTracked|dirtyAllowsIdleExit|dirtyIsDirty) == dirtyTracked|dirtyAllowsIdleExit
}

// idleKeep are idle-exit daemons that are never ended: diagnosticd and logd_helper serve agemu's
// own log stream (logs, crash notices) and came back within seconds; dasd schedules background
// tasks (an app's BGTaskScheduler work, iOS's own deferred boot work): ended at +65 s, it came back
// at +440 s and device management's on-reboot task ran at +567 s instead of +131..197 s;
// followupd serves the follow-up rows at the top of Settings ("Ready for Apple Intelligence"):
// ended, it is started again by Settings and the row came later (General at y=880 at first,
// y=1141 once the row was in), so a tap on General can land on the row; with followupd ended,
// 2 of 2 measured flows did not reach General's About screen; iconservicesagent serves app icons:
// ended, the home key from Settings took 1.44-1.57 s instead of 1.15-1.25 s (8 boots each; 1.15
// and 1.16 s with it kept); gamecontrollerd (the GameController framework) was started again at
// once, and the new one then used 0.17-0.19 s of CPU a minute, with diagnosticd and backboardd
// 0.3 s more, where the original one used none (4 of 4 boots; kept, it still did so in 1 of 4
// boots, and in none of 16 without the stop: what else sets it off was not found).
var idleKeep = map[string]bool{
	"diagnosticd": true, "logd_helper": true, "dasd": true, "followupd": true, "iconservicesagent": true,
	"gamecontrollerd": true,
}

// The passes of stopIdleDaemons: every 30 s from 1 to 5 minutes after the device is ready, like
// the background stop. A daemon is ended when two passes in a row found it idle-exitable and it
// used less than idleCPU of CPU time between them.
const (
	idleFirstPass = time.Minute
	idlePassEvery = 30 * time.Second
	idleLastPass  = 5 * time.Minute
	// idleCPU is the CPU time an idle daemon may use between two passes (timers, a notification
	// it reacted to).
	idleCPU = 20 * time.Millisecond
	// idleKillAfter: a daemon that is still running this long after SIGTERM and still clean is
	// sent SIGKILL, as launchd does for a pressured exit (most do not end on SIGTERM).
	idleKillAfter = 3 * time.Second
	// idleMaxEnds bounds how often one daemon is ended in a boot.
	idleMaxEnds = 3
)

// daemonProc is one process of the simulator that idlePasses looks at.
type daemonProc struct {
	pid   int
	name  string // executable file name
	flags uint32 // dirty-tracking flags
	cpu   time.Duration
}

// idleEnv is what the passes observe and act on: one cold boot of a device (deviceIdle), or a fake
// in tests.
type idleEnv interface {
	waitUntil(at time.Duration) bool
	current() bool
	// daemons lists the simulator runtime's processes that launchd_sim started, with their
	// dirty-tracking flags and CPU time.
	daemons() ([]daemonProc, error)
	// dirty reads one process's flags again; an error means it has exited.
	dirty(pid int) (uint32, error)
	signal(pid int, sig syscall.Signal) error
	sleep(d time.Duration)
}

// idlePasses runs the passes and returns how many processes it ended, by name. Each pass ends the
// daemons that the previous pass found too (same pid), that are idle-exitable at both passes and
// used under idleCPU in between: SIGTERM, then SIGKILL after idleKillAfter if still running and
// clean. A name is left alone for the rest of the boot when ending it did not hold: it was
// running again at the next pass (something asks for it all the time, so ending it only costs a
// relaunch), or it became dirty before the SIGKILL. A name that comes back later (a one-off job,
// such as device management's on-reboot task at +120..200 s, started it) may be ended again once
// it is idle, up to idleMaxEnds times per boot.
func idlePasses(env idleEnv, name string) map[string]int {
	stopped := map[string]int{}
	endedAt := map[string]int{} // pass index of the last end, by name
	keep := idleKeepEnv()
	if !idleExitAll() {
		for n := range appFacing {
			keep[n] = true
		}
	}
	var prev map[int]daemonProc
	i := 0
	for at := idleFirstPass; at <= idleLastPass; at, i = at+idlePassEvery, i+1 {
		if !env.waitUntil(at) || !env.current() {
			break
		}
		procs, err := env.daemons()
		if err != nil {
			log.Printf("ios lean %s: idle daemons: %v", name, err)
			break
		}
		cur := map[int]daemonProc{}
		for _, p := range procs {
			cur[p.pid] = p
			if e, ok := endedAt[p.name]; ok && e == i-1 {
				keep[p.name] = true // back by the next pass
			}
		}
		var stop []daemonProc
		for _, p := range procs {
			q, seen := prev[p.pid]
			if !seen || q.name != p.name || idleKeep[p.name] || keep[p.name] || stopped[p.name] >= idleMaxEnds ||
				!idleExitable(p.flags) || !idleExitable(q.flags) || p.cpu-q.cpu >= idleCPU {
				continue
			}
			stop = append(stop, p)
		}
		for _, p := range stop {
			_ = env.signal(p.pid, syscall.SIGTERM)
		}
		if len(stop) > 0 {
			env.sleep(idleKillAfter)
		}
		pass := map[string]int{}
		for _, p := range stop {
			if f, err := env.dirty(p.pid); err == nil {
				if !idleExitable(f) {
					keep[p.name] = true // busy again: it stays
					continue
				}
				_ = env.signal(p.pid, syscall.SIGKILL)
			}
			endedAt[p.name] = i
			delete(cur, p.pid)
			pass[p.name]++
			stopped[p.name]++
		}
		prev = cur
		if len(pass) > 0 {
			log.Printf("ios lean %s: ended %s that were idle (pass at +%.0f s)", name, countedNames(pass, "idle daemons"), at.Seconds())
		}
	}
	return stopped
}

// stopIdleDaemons runs idlePasses after a cold boot. A newer cold boot of the device (gen) or the
// backend closing ends it.
func (b *Backend) stopIdleDaemons(d simDevice, gen int) {
	idlePasses(&deviceIdle{deviceBackground{b: b, d: d, gen: gen, ready: time.Now()}}, d.Name)
}

// deviceIdle is the idleEnv of one cold boot of a simulator (its waiting and boot checks are the
// background stop's).
type deviceIdle struct{ deviceBackground }

func (e *deviceIdle) daemons() ([]daemonProc, error) {
	if e.b.isParked(e.d.UDID) {
		return nil, nil
	}
	out, err := exec.Command("ps", "-axwwo", "pid=,ppid=,command=").Output()
	if err != nil {
		return nil, err
	}
	procs, _ := simChildren(string(out), e.d.UDID)
	cpuOut, err := exec.Command("ps", "-axo", "pid=,time=").Output()
	if err != nil {
		return nil, err
	}
	return runtimeDaemons(procs, parseCPUTimes(string(cpuOut)), procDirty), nil
}

func (e *deviceIdle) dirty(pid int) (uint32, error)            { return procDirty(pid) }
func (e *deviceIdle) signal(pid int, sig syscall.Signal) error { return syscall.Kill(pid, sig) }
func (e *deviceIdle) sleep(d time.Duration)                    { time.Sleep(d) }

// runtimeDaemons keeps the processes that run an executable of the simulator runtime (not an
// app's) and whose dirty-tracking flags can be read, with their CPU time.
func runtimeDaemons(procs []simProc, cpu map[int]time.Duration, dirty func(int) (uint32, error)) []daemonProc {
	var ds []daemonProc
	for _, p := range procs {
		i := strings.Index(p.cmd, runtimeRoot)
		if i < 0 || !strings.HasPrefix(p.cmd, "/") || strings.Contains(p.cmd[:i], " /") {
			continue
		}
		f := strings.Fields(p.cmd[i+len(runtimeRoot):])
		if len(f) == 0 {
			continue
		}
		c, ok := cpu[p.pid]
		if !ok {
			continue
		}
		flags, err := dirty(p.pid)
		if err != nil {
			continue
		}
		ds = append(ds, daemonProc{pid: p.pid, name: path.Base(f[0]), flags: flags, cpu: c})
	}
	return ds
}

// parseCPUTimes reads `ps -axo pid=,time=` (user+system CPU time as [[dd-]hh:]mm:ss.cc).
func parseCPUTimes(out string) map[int]time.Duration {
	m := map[int]time.Duration{}
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) != 2 {
			continue
		}
		pid, err := strconv.Atoi(f[0])
		if err != nil {
			continue
		}
		if d, err := parsePsTime(f[1]); err == nil {
			m[pid] = d
		}
	}
	return m
}

func parsePsTime(s string) (time.Duration, error) {
	var days int
	if i := strings.Index(s, "-"); i >= 0 {
		n, err := strconv.Atoi(s[:i])
		if err != nil {
			return 0, err
		}
		days, s = n, s[i+1:]
	}
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, fmt.Errorf("bad time %q", s)
	}
	sec, err := strconv.ParseFloat(parts[len(parts)-1], 64)
	if err != nil {
		return 0, err
	}
	total := sec
	mul := 60.0
	for i := len(parts) - 2; i >= 0; i-- {
		n, err := strconv.Atoi(parts[i])
		if err != nil {
			return 0, err
		}
		total += float64(n) * mul
		mul *= 60
	}
	total += float64(days) * 86400
	return time.Duration(total * float64(time.Second)), nil
}
