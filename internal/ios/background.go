//go:build !windows

package ios

// Background work after boot. In the first minutes after a cold boot iOS 26.5 starts some
// built-in apps, app extensions and services for one-off work and then keeps them idle. A phone
// reclaims such processes when it needs memory (jetsam); the simulator only when the Mac itself
// runs short of memory (launchd_sim's idle exit), so they normally stay. A lean simulator stops
// the ones that are not started again (see backgroundProcesses), so an app under test sees a
// device where that idle work was reclaimed. Opening Calendar, using App Intents or device
// management starts them again as usual. Others were tried and are left alone because iOS
// restarts them: WidgetRenderer_Default (SpringBoard relaunched it at once), the Spotlight UI
// (relaunched whenever the home screen shows, for example after unlock) and the speech synthesis
// extensions (axassetsd's voice database retries load them again every few minutes).

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// backgroundProcess is a process iOS starts in the background after boot that a lean simulator
// stops.
type backgroundProcess struct {
	// exe is the executable's path inside the simulator runtime's root.
	exe string
	// app is the bundle identifier of an app: it is terminated through CoreSimulator, and only if
	// no pass found it in front (see backgroundPasses). Other processes ("") get SIGTERM.
	app string
}

// remoteManagement is where device management's (MDM and declarative management) executables
// live in the runtime.
const remoteManagement = "System/Library/PrivateFrameworks/RemoteManagement.framework/"

var backgroundProcesses = append([]backgroundProcess{
	// The Calendar app (80-84 MB). calaccessd opens it in the background after changes to the
	// calendar database: after a boot (rebuilding the Birthdays calendar) it opened it 16-130 s
	// after the device was ready, in some boots not at all.
	{"Applications/MobileCal.app/MobileCal", "com.apple.mobilecal"},
	// App Intents extensions of Calendar (16 MB), Safari (21 MB) and Messages (9 MB). linkd
	// starts them 16-100 s after the device is ready to read their actions.
	{"Applications/MobileCal.app/Extensions/CalendarIntentsExtension.appex/CalendarIntentsExtension", ""},
	{"Applications/MobileSafari.app/Extensions/SafariLinkExtension.appex/SafariLinkExtension", ""},
	{"Applications/MobileSMS.app/Extensions/MessagesActionExtension.appex/MessagesActionExtension", ""},
}, subscribers(
	// Device management. Its launchd job runs a one-off on-reboot task 120-200 s after boot,
	// which starts remotemanagementd and these subscriber services (XPC services, which launchd
	// starts on demand; 10-14 MB each, 106 MB together). remotemanagementd itself (7 MB) stays:
	// it did not exit on SIGTERM. Only the subscribers seen after a boot are listed; the
	// framework has 3 more (MDMAppSubscriber, PowerSubscriber, SoftwareUpdateSubscriber), which
	// are left alone.
	"ASConfigurationSubscriber", "AccountSubscriber", "InteractiveLegacyProfilesSubscriber",
	"LegacyProfilesSubscriber", "ManagedAppsSubscriber", "ManagedSettingsSubscriber",
	"ManagementTestSubscriber", "PasscodeSettingsSubscriber", "SecuritySubscriber",
	"WatchEnrollmentSubscriber",
)...)

// subscribers lists device management's subscriber XPC services by name.
func subscribers(names ...string) []backgroundProcess {
	ps := make([]backgroundProcess, len(names))
	for i, n := range names {
		ps[i] = backgroundProcess{exe: remoteManagement + "XPCServices/" + n + ".xpc/" + n}
	}
	return ps
}

// runtimeRoot ends the path of the simulator runtime's root in an executable's path.
const runtimeRoot = ".simruntime/Contents/Resources/RuntimeRoot/"

// runningBackground is a running backgroundProcess.
type runningBackground struct {
	pid int
	// name is the executable's file name (for the log).
	name string
	backgroundProcess
}

// backgroundToStop picks, from the simulator's processes, the running backgroundProcesses. Only
// executables of the simulator runtime match (an app's own extension of the same name does not).
func backgroundToStop(procs []simProc) []runningBackground {
	var stop []runningBackground
	for _, p := range procs {
		// The runtime's path has spaces ("iOS 26.5.simruntime") but no argument before it: a
		// process that only names the path in its arguments does not match.
		i := strings.Index(p.cmd, runtimeRoot)
		if i < 0 || !strings.HasPrefix(p.cmd, "/") || strings.Contains(p.cmd[:i], " /") {
			continue
		}
		exe := p.cmd[i+len(runtimeRoot):]
		for _, bp := range backgroundProcesses {
			if exe == bp.exe || strings.HasPrefix(exe, bp.exe+" ") {
				stop = append(stop, runningBackground{p.pid, path.Base(bp.exe), bp})
				break
			}
		}
	}
	return stop
}

// The passes of stopBackgroundApps run every 30 s from 1 to 5 minutes after the device is ready.
// A process is stopped at the first pass that finds it again, so it has run for at least 30 s and
// finished what it was started for (remotemanagementd's on-reboot task took a few seconds).
const (
	backgroundFirstPass = time.Minute
	backgroundPassEvery = 30 * time.Second
	backgroundLastPass  = 5 * time.Minute
)

// backgroundEnv is what the passes of backgroundPasses observe and act on: one cold boot of a
// device (deviceBackground), or a fake in tests.
type backgroundEnv interface {
	// waitUntil waits until at after the device was ready; false when the backend closed.
	waitUntil(at time.Duration) bool
	// current reports whether this is still the device's latest cold boot.
	current() bool
	// processes lists the processes the simulator's launchd_sim started.
	processes() ([]simProc, error)
	// frontmostPid is the pid of the frontmost app.
	frontmostPid() (int, error)
	terminate(bundleID string) error
	kill(pid int) error
}

// stopBackgroundApps stops the backgroundProcesses after a cold boot. A newer cold boot of the
// device (gen) or the backend closing ends it.
func (b *Backend) stopBackgroundApps(d simDevice, gen int) {
	backgroundPasses(&deviceBackground{b: b, d: d, gen: gen, ready: time.Now()}, d.Name)
}

// backgroundPasses runs the passes, and logs what each pass stopped. It returns the number of
// processes stopped, by executable name.
func backgroundPasses(env backgroundEnv, name string) map[string]int {
	stopped := map[string]int{}
	seen := map[int]bool{}
	// opened are the app processes a pass found in front: the user or a test opened them, so
	// they are not background work and stay, also after they go to the background.
	opened := map[int]bool{}
	for at := backgroundFirstPass; at <= backgroundLastPass; at += backgroundPassEvery {
		if !env.waitUntil(at) || !env.current() {
			break
		}
		procs, err := env.processes()
		if err != nil {
			log.Printf("ios lean %s: ps: %v", name, err)
			break
		}
		running := backgroundToStop(procs)
		// The frontmost app is asked once per pass, and only when an app is running.
		var front int
		var frontErr error
		asked := false
		frontmost := func() (int, error) {
			if !asked {
				front, frontErr = env.frontmostPid()
				asked = true
			}
			return front, frontErr
		}
		for _, p := range running {
			if p.app != "" {
				// An app that may have been in front (the lookup failed, or found no app) is
				// treated as opened too: a later pass that finds it in the background cannot
				// tell that it was never used.
				if f, err := frontmost(); err != nil || f == 0 || f == p.pid {
					opened[p.pid] = true
				}
			}
		}
		pass := map[string]int{}
		for _, p := range olderThanAPass(running, seen) {
			if stopBackground(env, p, frontmost, opened) {
				pass[p.name]++
				stopped[p.name]++
			}
		}
		if len(pass) > 0 {
			log.Printf("ios lean %s: stopped %s iOS started in the background after boot (pass at +%.0f s)",
				name, countedNames(pass, "processes"), at.Seconds())
		}
	}
	return stopped
}

// olderThanAPass returns the running processes that an earlier pass found too (seen), and
// remembers the others for the next pass.
func olderThanAPass(running []runningBackground, seen map[int]bool) []runningBackground {
	var old []runningBackground
	for _, p := range running {
		if seen[p.pid] {
			old = append(old, p)
		}
		seen[p.pid] = true
	}
	return old
}

// stopBackground stops one running background process: SIGTERM, or for an app termination
// through CoreSimulator, unless a pass found it in front (opened, which includes this pass) or
// the frontmost app cannot be told.
func stopBackground(env backgroundEnv, p runningBackground, frontmost func() (int, error), opened map[int]bool) bool {
	if p.app == "" {
		return env.kill(p.pid) == nil
	}
	if opened[p.pid] {
		return false
	}
	if front, err := frontmost(); err != nil || front == 0 {
		return false
	}
	return env.terminate(p.app) == nil
}

// countedNames formats counts by name as "3 processes (a, b x2)".
func countedNames(counts map[string]int, what string) string {
	n := 0
	var ps []string
	for _, p := range sortedKeys(counts) {
		n += counts[p]
		if counts[p] > 1 {
			p += " x" + strconv.Itoa(counts[p])
		}
		ps = append(ps, p)
	}
	return fmt.Sprintf("%d %s (%s)", n, what, strings.Join(ps, ", "))
}

// deviceBackground is the backgroundEnv of one cold boot (gen) of a simulator.
type deviceBackground struct {
	b     *Backend
	d     simDevice
	gen   int
	ready time.Time
}

// waitUntil also holds a pass while the simulator is parked, and moves the later passes by as
// long (waitWhileParked): a pass that fell into the park runs once it is resumed.
func (e *deviceBackground) waitUntil(at time.Duration) bool {
	for {
		select {
		case <-e.b.bg.Done():
			return false
		case <-time.After(time.Until(e.ready.Add(at))):
		}
		held, ok := e.b.waitWhileParked(e.d.UDID, e.gen)
		if !ok {
			return false
		}
		if held < parkPoll {
			return true
		}
		e.ready = e.ready.Add(held)
	}
}

func (e *deviceBackground) current() bool { return e.b.currentBoot(e.d.UDID, e.gen) }

func (e *deviceBackground) processes() ([]simProc, error) {
	if e.b.isParked(e.d.UDID) {
		// Parked after waitUntil let the pass run: its processes cannot be asked whether they
		// are in front, so the pass finds nothing and stops nothing.
		return nil, nil
	}
	out, err := exec.Command("ps", "-axwwo", "pid=,ppid=,command=").Output()
	if err != nil {
		return nil, err
	}
	procs, _ := simChildren(string(out), e.d.UDID)
	return procs, nil
}

func (e *deviceBackground) frontmostPid() (int, error) {
	ctx, cancel := context.WithTimeout(e.b.bg, 10*time.Second)
	defer cancel()
	var front struct {
		Pid int `json:"pid"`
	}
	err := e.b.call(ctx, "frontmost", map[string]any{"udid": e.d.UDID, "pidOnly": true, "timeoutMs": 3000}, &front)
	return front.Pid, err
}

func (e *deviceBackground) terminate(bundleID string) error {
	ctx, cancel := context.WithTimeout(e.b.bg, 10*time.Second)
	defer cancel()
	return e.b.call(ctx, "terminate", map[string]any{"udid": e.d.UDID, "bundleId": bundleID}, nil)
}

func (e *deviceBackground) kill(pid int) error { return syscall.Kill(pid, syscall.SIGTERM) }
