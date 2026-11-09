package ios

// Parking (agemu park / resume): a booted simulator is stopped in place with SIGSTOP, so it
// uses no CPU and the host can compress or swap out its memory, and SIGCONT resumes it, with
// its processes, the app's state and its screen as they were.
//
// A simulator is a tree of host processes: launchd_sim and the processes it starts (every
// simulator process is its direct child; none were deeper on iOS 26.5). Parking stops
// launchd_sim first, so it starts nothing new, then its children, and looks again until no
// unstopped child is left. Resuming continues the processes the park stopped (a child that was
// already stopped, by a debugger say, stays stopped), then launchd_sim, and waits until the
// device answers as after a boot (accessibility, framebuffer, HID). Only agemu's own simulators
// are parked or resumed (managed, or AGEMU_IOS_ALLOW_FOREIGN=1, as for every other call).
//
// What an app sees (measured, docs/IOS.md "Parking"): its clocks run on through the park (wall
// clock and uptime both jump by its length), timers fire once on resume, a local notification
// that came due during the park is shown on resume, late by the rest of the park, and a TCP
// connection to the host stayed open with its queued data arriving at once in 16 of 18 parks.

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// parkRounds bounds the passes that look for children started while launchd_sim was being
// stopped (a spawn in flight); one more pass found none in every park measured.
const parkRounds = 5

// simTree finds, in `ps -axwwo pid=,ppid=,stat=,command=` output, the simulator's launchd_sim,
// its children and whether each is stopped (STAT starting with "T").
func simTree(ps, udid string) (root int, rootStopped bool, children map[int]bool) {
	type proc struct {
		ppid    int
		stopped bool
	}
	procs := map[int]proc{}
	for _, line := range strings.Split(ps, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil {
			continue
		}
		stopped := strings.HasPrefix(f[2], "T")
		procs[pid] = proc{ppid, stopped}
		// launchd_sim's argument is <device>/data/var/run/launchd_bootstrap.plist.
		if (f[3] == "launchd_sim" || strings.HasSuffix(f[3], "/launchd_sim")) && strings.Contains(line, "/"+udid+"/") {
			root, rootStopped = pid, stopped
		}
	}
	if root == 0 {
		return 0, false, nil
	}
	children = map[int]bool{}
	for pid, p := range procs {
		if p.ppid == root {
			children[pid] = p.stopped
		}
	}
	return root, rootStopped, children
}

// psTree runs ps for simTree.
func psTree() (string, error) {
	out, err := exec.Command("ps", "-axwwo", "pid=,ppid=,stat=,command=").Output()
	return string(out), err
}

// signaller sends a signal to a process; tests replace it.
type signaller func(pid int, sig syscall.Signal) error

// stopTree stops launchd_sim, then its children, looking again (up to parkRounds times) for
// children that were not stopped yet. It returns the processes it stopped (launchd_sim
// included), also when it fails, so that they can be continued: children that were stopped
// already are not among them.
func stopTree(list func() (string, error), udid string, kill signaller) (map[int]bool, error) {
	ps, err := list()
	if err != nil {
		return nil, err
	}
	root, _, children := simTree(ps, udid)
	if root == 0 {
		return nil, fmt.Errorf("no launchd_sim runs for simulator %s", udid)
	}
	if err := kill(root, syscall.SIGSTOP); err != nil {
		return nil, fmt.Errorf("stopping launchd_sim (pid %d): %w", root, err)
	}
	stopped := map[int]bool{root: true}
	done := map[int]bool{}
	for round := 0; round < parkRounds; round++ {
		todo := 0
		for pid, already := range children {
			if done[pid] || already {
				continue
			}
			todo++
			// A child that exited in between is gone: nothing to stop.
			if kill(pid, syscall.SIGSTOP) == nil {
				stopped[pid] = true
			}
			done[pid] = true
		}
		if todo == 0 {
			return stopped, nil
		}
		if ps, err = list(); err != nil {
			return stopped, err
		}
		_, _, children = simTree(ps, udid)
	}
	return stopped, fmt.Errorf("simulator %s kept starting processes while it was being parked", udid)
}

// continueTree continues the simulator's children, then launchd_sim. With only, it continues
// just the children in it (those the park stopped); nil continues every child (a park from
// before this daemon, whose stopped processes are not known). It returns how many processes it
// signalled.
func continueTree(list func() (string, error), udid string, kill signaller, only map[int]bool) (int, error) {
	ps, err := list()
	if err != nil {
		return 0, err
	}
	root, _, children := simTree(ps, udid)
	if root == 0 {
		return 0, fmt.Errorf("no launchd_sim runs for simulator %s", udid)
	}
	n := 0
	for pid := range children {
		if only != nil && !only[pid] {
			continue
		}
		if kill(pid, syscall.SIGCONT) == nil {
			n++
		}
	}
	if err := kill(root, syscall.SIGCONT); err != nil {
		return n, fmt.Errorf("continuing launchd_sim (pid %d): %w", root, err)
	}
	return n + 1, nil
}

func sysKill(pid int, sig syscall.Signal) error { return syscall.Kill(pid, sig) }

// parkLock returns the simulator's lock that keeps a park and a shutdown apart: a boot that
// shuts down the least recently used simulator to make room can pick the one the daemon's
// auto-park is stopping at that moment, and a launchd_sim stopped during its shutdown would
// hold the shutdown (and the boot) for a minute.
func (b *Backend) parkLock(udid string) *sync.Mutex {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.parkLocks == nil {
		b.parkLocks = map[string]*sync.Mutex{}
	}
	l, ok := b.parkLocks[udid]
	if !ok {
		l = &sync.Mutex{}
		b.parkLocks[udid] = l
	}
	return l
}

// Park stops a booted simulator in place (see the comment at the top of this file). It does
// not count as a use of the device: it does not move the simulator to most recently used in the
// order a boot shuts simulators down by (a simulator idle longer is still shut down first).
func (b *Backend) Park(ctx context.Context, id string) error {
	udid, err := udidOf(id)
	if err != nil {
		return err
	}
	pl := b.parkLock(udid)
	// A shutdown of the simulator holds the lock for up to a minute; the park gives up when its
	// context ends instead of waiting it out (the simulator is shut down by then anyway).
	for !pl.TryLock() {
		select {
		case <-ctx.Done():
			return fmt.Errorf("parking %s: waiting for its shutdown: %w", udid, ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
	defer pl.Unlock()
	d, err := b.lookup(ctx, id)
	if err != nil {
		return err
	}
	if d.StateCode != stateBooted {
		return fmt.Errorf("simulator %s is %s; only a booted simulator can be parked", d.Name, normalizeState(d.State))
	}
	if b.isParked(d.UDID) {
		return nil
	}
	// Marked first: the lean passes skip a parked simulator, also one being stopped. parking
	// keeps a listing from dropping the mark before launchd_sim is stopped (stillParked).
	b.mu.Lock()
	b.parked[d.UDID] = true
	b.parking[d.UDID] = true
	if b.parks == nil {
		b.parks = map[string]int{}
	}
	b.parks[d.UDID]++
	b.mu.Unlock()
	start := time.Now()
	stopped, err := stopTree(psTree, d.UDID, sysKill)
	b.mu.Lock()
	delete(b.parking, d.UDID)
	if err == nil {
		if b.stoppedPids == nil {
			b.stoppedPids = map[string]map[int]bool{}
		}
		b.stoppedPids[d.UDID] = stopped
	}
	b.mu.Unlock()
	if err != nil {
		// Never leave a half-stopped simulator behind.
		if len(stopped) > 0 {
			_, _ = continueTree(psTree, d.UDID, sysKill, stopped)
		}
		b.mu.Lock()
		delete(b.parked, d.UDID)
		b.mu.Unlock()
		return fmt.Errorf("parking %s: %w", d.Name, err)
	}
	log.Printf("ios park %s: stopped %d processes in %.0f ms", d.Name, len(stopped), ms(time.Since(start)))
	return nil
}

// parkPoll is how often a post-boot lean pass held by a park looks whether it may run.
var parkPoll = time.Second

// waitWhileParked holds a post-boot lean pass (lean.go, background.go) while the simulator is
// parked: its processes cannot be looked at, and a pass that ran then would find nothing. The
// caller moves its later passes by the time returned, so they stay as far apart as planned
// (the background stop tells an idle process by its surviving from one pass to the next). ok
// is false once the backend closes or a newer boot of the device (gen) started.
func (b *Backend) waitWhileParked(udid string, gen int) (held time.Duration, ok bool) {
	start := time.Now()
	for b.isParked(udid) {
		if !b.currentBoot(udid, gen) {
			return time.Since(start), false
		}
		select {
		case <-b.bg.Done():
			return time.Since(start), false
		case <-time.After(parkPoll):
		}
	}
	return time.Since(start), b.currentBoot(udid, gen)
}

// drivable reports whether agemu may act on a simulator of this name (see target).
func drivable(name string) bool { return managed(name) || envOn("AGEMU_IOS_ALLOW_FOREIGN") }

// Parked reports whether the simulator is parked: from memory, after one look (parkedAtStart)
// for simulators a previous daemon left parked. Only agemu's own simulators are ever marked, so
// a call on another simulator never resumes it (it is refused by target).
func (b *Backend) Parked(id string) bool {
	udid, err := udidOf(id)
	if err != nil {
		return false
	}
	return b.isParked(udid)
}

// parkCount is how many parks of the simulator this daemon has begun.
func (b *Backend) parkCount(udid string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.parks[udid]
}

func (b *Backend) isParked(udid string) bool {
	b.parkScan.Do(b.parkedAtStart)
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.parked[udid]
}

// unpark continues a parked simulator's processes without waiting for it to answer (shutdown
// uses it: CoreSimulator cannot shut down a stopped launchd_sim). It reports whether the
// simulator was parked.
func (b *Backend) unpark(udid string) (bool, int, error) {
	if !b.isParked(udid) {
		return false, 0, nil
	}
	b.mu.Lock()
	only := b.stoppedPids[udid]
	b.mu.Unlock()
	n, err := continueTree(psTree, udid, sysKill, only)
	if err != nil {
		return true, n, err
	}
	b.mu.Lock()
	delete(b.parked, udid)
	delete(b.stoppedPids, udid)
	b.mu.Unlock()
	return true, n, nil
}

// Resume continues a parked simulator and returns once it is agent-ready again (the readiness
// boot waits for). It reports whether the device was parked; resuming an agemu simulator that
// is not parked is a no-op, and any other simulator is refused (target).
func (b *Backend) Resume(ctx context.Context, id string) (bool, error) {
	udid, err := udidOf(id)
	if err != nil {
		return false, err
	}
	if !b.isParked(udid) {
		if _, err := b.target(ctx, id); err != nil {
			return false, err
		}
		return false, nil
	}
	start := time.Now()
	was, n, err := b.unpark(udid)
	if !was {
		return false, nil
	}
	if err != nil {
		return true, fmt.Errorf("resuming %s: %w", udid, err)
	}
	cont := time.Since(start)
	rctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	var r readyResult
	if err := b.call(rctx, "ready", map[string]any{"udid": udid, "timeoutMs": 55000, "waitAccessibility": true}, &r); err != nil {
		return true, fmt.Errorf("resuming %s: the simulator did not answer after its processes were continued: %w", udid, err)
	}
	log.Printf("ios resume %s: continued %d processes in %.0f ms, agent-ready %.0f ms after the start", udid, n, ms(cont), ms(time.Since(start)))
	return true, nil
}

// stillParked checks the simulators marked parked against their processes, with one ps when
// any is marked (none usually, so listing stays free of it): a mark whose launchd_sim runs
// (another daemon or a person resumed it) is dropped. It returns the ones still stopped.
func (b *Backend) stillParked(list func() (string, error), udids []string) map[string]bool {
	out := map[string]bool{}
	if len(udids) == 0 {
		return out
	}
	ps, err := list()
	if err != nil {
		for _, u := range udids {
			out[u] = true
		}
		return out
	}
	for _, u := range udids {
		if root, rootStopped, _ := simTree(ps, u); root != 0 && rootStopped {
			out[u] = true
			continue
		}
		b.mu.Lock()
		if b.parking[u] {
			out[u] = true // being parked now
		} else {
			delete(b.parked, u)
		}
		b.mu.Unlock()
	}
	return out
}

// parkedAtStart marks the agemu simulators whose launchd_sim is stopped as parked: a daemon
// that restarted finds the devices the previous one parked (or that were stopped by hand), so
// the next call on them resumes them instead of timing out.
func (b *Backend) parkedAtStart() {
	ps, err := psTree()
	if err != nil {
		return
	}
	b.markStopped(ps, simNameAt)
}

// markStopped marks as parked each simulator in ps whose launchd_sim is stopped and whose name
// (nameOf of its device directory) agemu may drive. Another tool's or a person's stopped
// simulator is left alone: a call naming it is refused, never resumed.
func (b *Backend) markStopped(ps string, nameOf func(devDir string) string) {
	for _, line := range strings.Split(ps, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 || !strings.HasPrefix(f[2], "T") || !(f[3] == "launchd_sim" || strings.HasSuffix(f[3], "/launchd_sim")) {
			continue
		}
		udid := simUDID(line)
		if udid == "" {
			continue
		}
		if name := nameOf(simDevDir(line, udid)); !drivable(name) {
			continue
		}
		b.mu.Lock()
		b.parked[udid] = true
		b.mu.Unlock()
		log.Printf("ios: simulator %s is stopped (parked); the next call on it resumes it", udid)
	}
}

// simDevDir is the device directory (.../Devices/<UDID>) in launchd_sim's ps line.
func simDevDir(line, udid string) string {
	i := strings.Index(line, "/Devices/"+udid+"/")
	if i < 0 {
		return ""
	}
	start := strings.LastIndex(line[:i], " ") + 1
	return line[start : i+len("/Devices/")+len(udid)]
}

// simNameAt reads a simulator's name from its device.plist ("" when it cannot be read).
func simNameAt(devDir string) string {
	if devDir == "" {
		return ""
	}
	out, err := exec.Command("plutil", "-extract", "name", "raw", "-o", "-", filepath.Join(devDir, "device.plist")).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// simUDID reads the device's UDID from launchd_sim's argument,
// .../CoreSimulator/Devices/<UDID>/data/var/run/launchd_bootstrap.plist.
func simUDID(line string) string {
	_, rest, ok := strings.Cut(line, "/Devices/")
	if !ok {
		return ""
	}
	udid, _, ok := strings.Cut(rest, "/data/")
	if !ok || len(udid) != 36 {
		return ""
	}
	return udid
}
