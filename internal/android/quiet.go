package android

// Quiet baselines. A snapshot restores the guest exactly as it was saved, so work the guest had
// pending at the save runs again after every reset and boot: Play Store's daily hygiene jobs,
// the compaction of apps the freezer had not yet frozen, Play services' flag push (about 6
// minutes after a cold boot). A baseline saved 49 s after a cold boot (agemu's 20 s settle on a
// quiet host) replayed about 38 CPU-seconds of that in the 2 minutes after each reset; one saved
// after the guest had finished it replayed almost none. agemu therefore waits, before it saves the
// baseline, until the guest has been up AGEMU_ANDROID_BASELINE_UPTIME_S (default 420) and its CPU
// has been quiet for a while. This costs a few minutes once per baseline (a new device, or a
// retake); boots and resets from it are unchanged in speed. AGEMU_ANDROID_BASELINE_UPTIME_S=0
// saves as soon as the baseline steps are done (agemu before 2026-11-08).

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	quietWindow     = 10 * time.Second // one guest sample
	quietWindows    = 6                // consecutive quiet samples needed (a minute)
	quietBusyPct    = 10.0             // % of one vCPU; a settled guest idles at 4-6%
	quietRxKBps     = 50.0             // guest network receive rate; a Play Store download is ~1 MB/s
	quietAfterHome  = 60 * time.Second // the freezer freezes and compacts cached apps by then
	quietExtraLimit = 10 * time.Minute // past the uptime target, save even if never quiet
)

// baselineUptime is the guest uptime the baseline waits for (0: no wait).
func baselineUptime() time.Duration {
	// envInt treats 0 as unset; here 0 means "do not wait".
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv("AGEMU_ANDROID_BASELINE_UPTIME_S"))); err == nil && v >= 0 {
		return time.Duration(v) * time.Second
	}
	return 420 * time.Second
}

// procStatBusy returns the busy and total jiffies of the first ("cpu") line of /proc/stat.
// Busy is user+nice+system+irq+softirq; total adds idle and iowait.
func procStatBusy(stat string) (busy, total int64, err error) {
	for _, l := range strings.Split(stat, "\n") {
		f := strings.Fields(l)
		if len(f) < 8 || f[0] != "cpu" {
			continue
		}
		var v [7]int64
		for i := range v {
			n, perr := strconv.ParseInt(f[i+1], 10, 64)
			if perr != nil {
				return 0, 0, fmt.Errorf("bad /proc/stat field %q", f[i+1])
			}
			v[i] = n
		}
		busy = v[0] + v[1] + v[2] + v[5] + v[6]
		return busy, busy + v[3] + v[4], nil
	}
	return 0, 0, fmt.Errorf("no cpu line in /proc/stat")
}

// parseUptime reads the first number of /proc/uptime.
func parseUptime(s string) (time.Duration, error) {
	f := strings.Fields(s)
	if len(f) == 0 {
		return 0, fmt.Errorf("empty /proc/uptime")
	}
	sec, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return 0, err
	}
	return time.Duration(sec * float64(time.Second)), nil
}

// netRxBytes sums the received bytes of every interface in /proc/net/dev except loopback.
func netRxBytes(netdev string) int64 {
	var sum int64
	for _, l := range strings.Split(netdev, "\n") {
		name, rest, ok := strings.Cut(l, ":")
		if !ok || strings.TrimSpace(name) == "lo" {
			continue
		}
		f := strings.Fields(rest)
		if len(f) == 0 {
			continue
		}
		if n, err := strconv.ParseInt(f[0], 10, 64); err == nil {
			sum += n
		}
	}
	return sum
}

// guestSample is one read of the guest's uptime, CPU and network counters.
type guestSample struct {
	at          time.Time
	uptime      time.Duration
	busy, total int64
	rx          int64
}

const sampleSep = "--agemu--"

func readGuestSample(ctx context.Context, serial string) (guestSample, error) {
	out, err := adbShellOK(ctx, serial, "cat /proc/uptime; head -1 /proc/stat; echo "+sampleSep+"; cat /proc/net/dev")
	if err != nil {
		return guestSample{}, err
	}
	head, netdev, _ := strings.Cut(out, sampleSep)
	lines := strings.SplitN(head, "\n", 2)
	if len(lines) < 2 {
		return guestSample{}, fmt.Errorf("short answer %q", out)
	}
	up, err := parseUptime(lines[0])
	if err != nil {
		return guestSample{}, err
	}
	b, t, err := procStatBusy(lines[1])
	if err != nil {
		return guestSample{}, err
	}
	return guestSample{at: time.Now(), uptime: up, busy: b, total: t, rx: netRxBytes(netdev)}, nil
}

// rxKBps is the guest's network receive rate between two samples.
func rxKBps(a, b guestSample) float64 {
	el := b.at.Sub(a.at).Seconds()
	if el <= 0 {
		return 0
	}
	return float64(b.rx-a.rx) / 1024 / el
}

// busyPct is the guest CPU between two samples in % of one vCPU (USER_HZ 100: one jiffy is
// 10 ms, and a busy vCPU accrues 100 jiffies a second).
func busyPct(a, b guestSample) float64 {
	el := b.at.Sub(a.at).Seconds()
	if el <= 0 {
		return 0
	}
	return float64(b.busy-a.busy) / el
}

// settleDecision is the pure part of waitGuestSettled: given the uptime, the time since the
// home screen, the run of quiet samples and the time past the uptime target, is the guest
// settled, and if not, why.
func settleDecision(uptime, target, sinceHome time.Duration, quietRun int) (bool, string) {
	switch {
	case uptime < target:
		return false, fmt.Sprintf("uptime %v < %v", uptime.Round(time.Second), target)
	case sinceHome < quietAfterHome:
		return false, fmt.Sprintf("%v since going home < %v", sinceHome.Round(time.Second), quietAfterHome)
	case quietRun < quietWindows:
		return false, fmt.Sprintf("%d of %d quiet samples", quietRun, quietWindows)
	}
	return true, ""
}

// waitGuestSettled blocks until the guest has been up baselineUptime(), the home screen has been
// shown for quietAfterHome, and the guest's CPU stayed below quietBusyPct for quietWindows
// samples in a row while receiving less than quietRxKBps from the network (a Play Store app
// update downloads at about 1 MB/s with little CPU; a baseline saved during one resumed the
// download, the install and its dexopt after every reset); or until quietExtraLimit past the uptime target. Best effort: an adb error
// ends the wait (the baseline is then saved as before).
func (d *device) waitGuestSettled(ctx context.Context, serial string, home time.Time) {
	target := baselineUptime()
	if target <= 0 {
		return
	}
	t0 := time.Now()
	prev, err := readGuestSample(ctx, serial)
	if err != nil {
		logf("%s: could not read the guest's CPU (%v); saving the baseline without waiting for it to settle", d.name, err)
		return
	}
	logf("%s: waiting until the guest has been up %v and is quiet (uptime now %v; AGEMU_ANDROID_BASELINE_UPTIME_S=0 skips this)", d.name, target, prev.uptime.Round(time.Second))
	quietRun := 0
	var last, lastRx float64
	for {
		ok, why := settleDecision(prev.uptime, target, time.Since(home), quietRun)
		if ok {
			logf("%s: guest settled after %v (uptime %v, last %.1f%% of a vCPU, %.1f KB/s received)", d.name, time.Since(t0).Round(time.Second), prev.uptime.Round(time.Second), last, lastRx)
			return
		}
		if prev.uptime > target+quietExtraLimit {
			logf("%s: guest not quiet %v past the uptime target (%s, last %.1f%% of a vCPU, %.1f KB/s received); saving the baseline anyway", d.name, quietExtraLimit, why, last, lastRx)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(quietWindow):
		}
		cur, err := readGuestSample(ctx, serial)
		if err != nil {
			logf("%s: could not read the guest's CPU (%v); saving the baseline now", d.name, err)
			return
		}
		last, lastRx = busyPct(prev, cur), rxKBps(prev, cur)
		if last < quietBusyPct && lastRx < quietRxKBps {
			quietRun++
		} else {
			quietRun = 0
		}
		prev = cur
	}
}

// Zero-filled free memory (opt-in). The emulator stores a guest page that is all zeros as an
// index entry only, and restores it by leaving a zero host page as it is (RamLoader::zeroOutPage
// checks isBufferZeroed before bzero). Guest pages the kernel has freed still hold their old
// contents. AGEMU_ANDROID_BASELINE_ZERO_MB=N has a shell process read N MB of /dev/zero into
// one buffer and free it just before the save, so those pages are zeros in the snapshot. With
// 400 MB on a 2 GB guest: ram.bin 667 MB instead of 867-948 MB, 20% less emulator CPU per
// load, and the guest's page cache 200 MB smaller after every restore (cold launches of Settings
// and Chrome were not slower). The fill is capped zeroMarginMB below MemAvailable, so the low
// memory killer has nothing to kill for it.

const zeroMarginMB = 128

// meminfoKB returns a /proc/meminfo field in kB (0 if absent).
func meminfoKB(meminfo, field string) int {
	for _, l := range strings.Split(meminfo, "\n") {
		f := strings.Fields(l)
		if len(f) >= 2 && f[0] == field+":" {
			n, _ := strconv.Atoi(f[1])
			return n
		}
	}
	return 0
}

// zeroFillMB is how many MB to fill: the request, at most MemAvailable minus the margin.
func zeroFillMB(want int, meminfo string) int {
	limit := meminfoKB(meminfo, "MemAvailable")/1024 - zeroMarginMB
	if want > limit {
		want = limit
	}
	if want < 0 {
		return 0
	}
	return want
}

func (d *device) zeroFreeMemory(ctx context.Context, serial string) {
	want := envInt("AGEMU_ANDROID_BASELINE_ZERO_MB", 0)
	if want <= 0 {
		return
	}
	mi, err := adbShellOK(ctx, serial, "cat /proc/meminfo")
	if err != nil {
		logf("%s: could not read guest memory (%v); not zero-filling", d.name, err)
		return
	}
	mb := zeroFillMB(want, mi)
	if mb == 0 {
		logf("%s: guest has too little available memory to zero-fill (%d kB available)", d.name, meminfoKB(mi, "MemAvailable"))
		return
	}
	t := time.Now()
	if _, err := adbShellOK(ctx, serial, fmt.Sprintf("dd if=/dev/zero of=/dev/null bs=%dm count=1", mb)); err != nil {
		logf("%s: zero-fill failed: %v", d.name, err)
		return
	}
	logf("%s: zero-filled %d MB of guest memory in %v (free was %d MB)", d.name, mb, time.Since(t).Round(time.Millisecond), meminfoKB(mi, "MemFree")/1024)
}
