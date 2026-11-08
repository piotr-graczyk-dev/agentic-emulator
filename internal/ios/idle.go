package ios

// Idle daemons after boot. iOS marks most of its on-demand daemons as safe to end when they are
// idle: their launchd jobs have EnablePressuredExit, and the kernel's dirty tracking (the same
// state the Mac's memorystatus keeps for the simulator's processes, since they are Mac
// processes) says "tracked, allows idle exit" and, between requests, "clean". A phone's jetsam
// ends such clean daemons first when it needs memory (JETSAM_REASON_MEMORY_IDLE_EXIT), and
// launchd starts them again at the next request; the simulator only does so when the Mac runs
// short of memory. After a cold boot iOS starts dozens of them for one-off work and they then
// sit idle. A lean simulator ends the ones that stay idle and clean, the way jetsam would (see
// idlePasses for what counts as idle); by default not those in appFacing, the daemons found to
// serve a public app API.

import (
	"log"
	"os"
	"os/exec"
	"path"
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

// appFacing are idle-exit daemons found to serve a public API an app under test can call. Ended,
// the app's first call after that waits for launchd to start the daemon again: Contacts and
// EventKit took about 2 s instead of 0.15 s, StoreKit, WatchConnectivity and the speech voice list
// about 0.9 s, MapKit directions up to 3 s (one probe run each way). So they are ended only with
// AGEMU_IOS_IDLE_EXIT=all. By default every other idle daemon may be ended; some of those may
// serve public APIs not identified here.
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
	// Found by the round-6 skeptic among the daemons the default ended (a probe app called
	// WeatherKit, AdServices, ManagedSettings, FamilyControls, File Provider, Verify with Wallet,
	// ...): they serve public APIs too.
	"weatherd": true, "adprivacyd": true, // WeatherKit, AdServices / AdAttributionKit
	"ManagedSettingsAgent": true, "FamilyControlsAgent": true, // ManagedSettings, FamilyControls
	"fileproviderd": true, "bird": true, "coreidvd": true, // File Provider, iCloud Drive, Verify with Wallet
	"backgroundassets.user": true, "AppSSODaemon": true, "GSSCred": true, // Background Assets, Extensible SSO, GSS/Kerberos
	"deviceaccessd": true, "clipserviced": true, "splashboardd": true, // AccessorySetupKit, App Clips, launch snapshots
	"StatusKitAgent": true, "healthappd": true, "webprivacyd": true, // Focus status sharing, Health, WebKit privacy
	"dataaccessd": true, "videosubscriptionsd": true, "biomed": true, // EventKit/Contacts accounts, VideoSubscriberAccount, INInteraction donations
	"profiled": true, "amsondevicestoraged": true, "ProtectedCloudKeySyncing": true, // managed app configuration, App Store media services, CloudKit keys
	"communicationtrustd": true, "finhealthd": true, // Live Caller ID Lookup, FinanceKit
}

// The kernel's dirty-tracking flags of a process (proc_get_dirty: PROC_DIRTY_*).
const (
	dirtyTracked        = 0x1
	dirtyAllowsIdleExit = 0x2
	dirtyIsDirty        = 0x4
	dirtyLaunching      = 0x8 // PROC_DIRTY_LAUNCH_IN_PROGRESS
)

// idleExitable: the process takes part in dirty tracking, allows idle exit, is clean now (no open
// transaction) and is not being launched.
func idleExitable(flags uint32) bool {
	return flags&(dirtyTracked|dirtyAllowsIdleExit|dirtyIsDirty|dirtyLaunching) == dirtyTracked|dirtyAllowsIdleExit
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
// 0.3 s more, where the original one used none (4 of 4 boots). (The original one too is busy
// like that in some boots, without the stop as well.)
var idleKeep = map[string]bool{
	"diagnosticd": true, "logd_helper": true, "dasd": true, "followupd": true, "iconservicesagent": true,
	"gamecontrollerd": true,
}

// The passes of stopIdleDaemons: every 30 s from 1 to 5 minutes after the device is ready, like
// the background stop. A daemon is idle when idleQuietPasses+1 passes in a row (60 s) found it
// idle-exitable and its CPU time (in the kernel's own units, not rounded) did not change at all
// between them: a daemon that serves an app's calls, even one every 45-60 s, uses CPU for each.
const (
	idleFirstPass   = time.Minute
	idlePassEvery   = 30 * time.Second
	idleLastPass    = 5 * time.Minute
	idleQuietPasses = 2
	// idleKillAfter: a daemon that is still running this long after SIGTERM and still clean is
	// sent SIGKILL, as launchd does for a pressured exit (most do not end on SIGTERM).
	idleKillAfter = 3 * time.Second
	// idleMaxEnds bounds how often one daemon is ended in a boot.
	idleMaxEnds = 3
)

// procUsage is what the kernel's resource accounting (proc_pid_rusage) says about a process: its
// user+system CPU time and its start time, both in mach units. Two reads of the same process have
// the same start; a reused pid does not.
type procUsage struct {
	cpu, start uint64
}

// daemonProc is one process of the simulator that idlePasses looks at.
type daemonProc struct {
	pid   int
	name  string // executable file name
	flags uint32 // dirty-tracking flags
	usage procUsage
}

// idleSnapshot is one look at a simulator for idlePasses.
type idleSnapshot struct {
	// root is the simulator's launchd_sim (0: the simulator is not running).
	root int
	// daemons are the simulator runtime's processes that launchd_sim started.
	daemons []daemonProc
	// firstApp is the start time of the earliest running process of an installed app (its
	// executable or one of its extensions); 0 when none runs.
	firstApp uint64
}

// idleEnv is what the passes observe and act on: one cold boot of a device (deviceIdle), or a fake
// in tests.
type idleEnv interface {
	waitUntil(at time.Duration) bool
	current() bool
	// held: agemu is installing, launching or uninstalling an app on the device, or the device is
	// parked or being parked. A pass then does nothing.
	held() bool
	snapshot() (idleSnapshot, error)
	// recheck reads p again: gone when it has exited (or its pid now names another process),
	// killable when it is the same process, still idle-exitable and the device is not held.
	recheck(p daemonProc) (gone, killable bool)
	signal(pid int, sig syscall.Signal) error
	sleep(d time.Duration)
}

// idlePasses runs the passes and returns how many processes it ended, by name.
//
// A daemon is ended when it has been idle for idleQuietPasses intervals in a row (same process,
// idle-exitable at each pass, CPU time unchanged), it started before the first process of an app
// in this boot (a daemon an app started, or that iOS started after the app was launched, may serve
// that app and is left alone), and its name is not kept. Ending is SIGTERM, then SIGKILL after
// idleKillAfter if recheck finds the same process still idle-exitable. A name is left alone for
// the rest of the boot when ending it did not hold: it was running again at the next pass
// (something asks for it all the time, so ending it only costs a relaunch), or it became busy
// before the SIGKILL. A name that comes back later (a one-off job, such as device management's
// on-reboot task at +120..200 s, started it) may be ended again once it is idle, up to
// idleMaxEnds times per boot. A pass while the device is held does nothing and starts the
// counting again; the passes stop when the simulator's launchd_sim changes (a boot not made by
// agemu) or is gone.
func idlePasses(env idleEnv, name string) map[string]int {
	stopped := map[string]int{}
	endedAt := map[string]int{} // pass index of the last end, by name
	keep := idleKeepEnv()
	if !idleExitAll() {
		for n := range appFacing {
			keep[n] = true
		}
	}
	prev := map[int]daemonProc{}
	quiet := map[int]int{} // pid -> idle intervals in a row
	root := 0
	var firstApp uint64
	i := 0
	for at := idleFirstPass; at <= idleLastPass; at, i = at+idlePassEvery, i+1 {
		if !env.waitUntil(at) || !env.current() {
			break
		}
		if env.held() {
			prev, quiet = map[int]daemonProc{}, map[int]int{}
			continue
		}
		snap, err := env.snapshot()
		if err != nil {
			log.Printf("ios lean %s: idle daemons: %v", name, err)
			break
		}
		if snap.root == 0 || (root != 0 && snap.root != root) {
			if snap.root != 0 {
				log.Printf("ios lean %s: the simulator was booted again outside agemu; idle passes stop", name)
			}
			break
		}
		root = snap.root
		if snap.firstApp != 0 && (firstApp == 0 || snap.firstApp < firstApp) {
			firstApp = snap.firstApp
		}
		cur := map[int]daemonProc{}
		nextQuiet := map[int]int{}
		var stop []daemonProc
		for _, p := range snap.daemons {
			cur[p.pid] = p
			if e, ok := endedAt[p.name]; ok && e == i-1 {
				keep[p.name] = true // back by the next pass
			}
			q, seen := prev[p.pid]
			if seen && q.name == p.name && q.usage.start == p.usage.start && idleExitable(p.flags) && idleExitable(q.flags) &&
				p.usage.cpu == q.usage.cpu {
				nextQuiet[p.pid] = quiet[p.pid] + 1
			}
			if nextQuiet[p.pid] < idleQuietPasses || idleKeep[p.name] || keep[p.name] || stopped[p.name] >= idleMaxEnds ||
				(firstApp != 0 && p.usage.start >= firstApp) {
				continue
			}
			stop = append(stop, p)
		}
		prev, quiet = cur, nextQuiet
		if len(stop) == 0 {
			continue
		}
		if env.held() {
			prev, quiet = map[int]daemonProc{}, map[int]int{}
			continue
		}
		for _, p := range stop {
			_ = env.signal(p.pid, syscall.SIGTERM)
		}
		env.sleep(idleKillAfter)
		pass := map[string]int{}
		for _, p := range stop {
			gone, killable := env.recheck(p)
			if !gone {
				if !killable {
					keep[p.name] = true // busy again (or the device is held): it stays
					continue
				}
				_ = env.signal(p.pid, syscall.SIGKILL)
			}
			endedAt[p.name] = i
			delete(prev, p.pid)
			pass[p.name]++
			stopped[p.name]++
		}
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

func (e *deviceIdle) held() bool { return e.b.isParked(e.d.UDID) || e.b.appOpBusy(e.d.UDID) }

func (e *deviceIdle) snapshot() (idleSnapshot, error) {
	out, err := exec.Command("ps", "-axwwo", "pid=,ppid=,command=").Output()
	if err != nil {
		return idleSnapshot{}, err
	}
	root, procs, _ := simRootChildren(string(out), e.d.UDID)
	return idleSnapshotOf(root, procs, procDirty, procUsageOf), nil
}

func (e *deviceIdle) recheck(p daemonProc) (gone, killable bool) {
	u, err := procUsageOf(p.pid)
	if err != nil || u.start != p.usage.start {
		return true, false
	}
	f, err := procDirty(p.pid)
	if err != nil {
		return true, false
	}
	return false, idleExitable(f) && !e.held()
}

func (e *deviceIdle) signal(pid int, sig syscall.Signal) error { return syscall.Kill(pid, sig) }
func (e *deviceIdle) sleep(d time.Duration)                    { time.Sleep(d) }

// appBundlePath marks the executables of installed apps (and their extensions) in a simulator.
const appBundlePath = "/data/Containers/Bundle/Application/"

// idleSnapshotOf builds a snapshot from launchd_sim's children: the processes that run an
// executable of the simulator runtime (not an app's) and whose dirty-tracking flags and usage can
// be read, and the earliest start of an app's process.
func idleSnapshotOf(root int, procs []simProc, dirty func(int) (uint32, error), usage func(int) (procUsage, error)) idleSnapshot {
	snap := idleSnapshot{root: root}
	for _, p := range procs {
		// An app's path has no spaces (the runtime's has), so its executable is the first field.
		if f := strings.Fields(p.cmd); len(f) > 0 && strings.HasPrefix(f[0], "/") && strings.Contains(f[0], appBundlePath) {
			if u, err := usage(p.pid); err == nil && (snap.firstApp == 0 || u.start < snap.firstApp) {
				snap.firstApp = u.start
			}
			continue
		}
		i := strings.Index(p.cmd, runtimeRoot)
		if i < 0 || !strings.HasPrefix(p.cmd, "/") || strings.Contains(p.cmd[:i], " /") {
			continue
		}
		f := strings.Fields(p.cmd[i+len(runtimeRoot):])
		if len(f) == 0 {
			continue
		}
		u, err := usage(p.pid)
		if err != nil {
			continue
		}
		flags, err := dirty(p.pid)
		if err != nil {
			continue
		}
		snap.daemons = append(snap.daemons, daemonProc{pid: p.pid, name: path.Base(f[0]), flags: flags, usage: u})
	}
	return snap
}
