package android

// Quiet baselines. A snapshot restores the guest exactly as it was saved, so work the guest had
// pending at the save runs again after every reset and boot: Play Store's daily hygiene jobs and
// app updates, the compaction of apps the freezer had not yet frozen, Play services' flag push.
// Emulator CPU in the 2 minutes after a reset: 154 CPU-s with a new device's old-style baseline
// (saved about 50 s after boot), 54 with a retake saved at 49 s, 10-17 with a settled one. agemu
// therefore waits, before it saves the baseline, until the guest has been up
// AGEMU_ANDROID_BASELINE_UPTIME_S (default 420) and its last minute was quiet (CPU and network).
// That makes a first boot (or a retake) take 7-8 minutes, at most 20 (prepMax, a hard bound); the boot
// goes on in the background meanwhile (prepare.go). Boots and resets from the baseline are
// unchanged in speed. AGEMU_ANDROID_BASELINE_UPTIME_S=0 saves as soon as the baseline steps are
// done (agemu before 2026-11-08).

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Variables only so tests can shorten them.
var (
	quietWindow     = 10 * time.Second // one guest sample
	quietExtraLimit = 5 * time.Minute  // past the uptime target, save even if never quiet
	// readGuestSampleFn reads one sample (tests replace it).
	readGuestSampleFn = readGuestSample
)

const (
	quietWindows       = 6                // samples in the quiet minute
	quietMeanPct       = 15.0             // mean over the minute, % of one vCPU; a settled guest reads 4-11%
	quietPeakPct       = 30.0             // no sample above this (a job, an install, a dexopt)
	quietRxKBps        = 50.0             // guest network receive rate; a Play Store download is ~1 MB/s
	quietAfterHome     = 60 * time.Second // the freezer freezes and compacts cached apps by then
	quietSampleTimeout = 15 * time.Second // one guest read (adb)
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

// quietSample is one 10 s sample of the guest.
type quietSample struct {
	busyPct float64 // % of one vCPU
	rxKBps  float64
}

// quietVerdict says whether the last quietWindows samples are a quiet minute: their mean CPU is
// below quietMeanPct, none is above quietPeakPct (a job, an install or a dexopt shows as a burst)
// and none received quietRxKBps or more (a download uses little CPU). A mean, not a per-sample
// threshold: a settled guest reads 4-11% of a vCPU per sample, more on a loaded host, and a
// per-sample threshold of 10% could keep the wait going for its whole cap.
func quietVerdict(win []quietSample) (bool, string) {
	if len(win) < quietWindows {
		return false, fmt.Sprintf("%d of %d samples", len(win), quietWindows)
	}
	win = win[len(win)-quietWindows:]
	var sum float64
	for _, q := range win {
		if q.rxKBps >= quietRxKBps {
			return false, fmt.Sprintf("receiving %.0f KB/s", q.rxKBps)
		}
		if q.busyPct >= quietPeakPct {
			return false, fmt.Sprintf("a sample at %.1f%% of a vCPU", q.busyPct)
		}
		sum += q.busyPct
	}
	if mean := sum / float64(len(win)); mean >= quietMeanPct {
		return false, fmt.Sprintf("mean %.1f%% of a vCPU over the last minute", mean)
	}
	return true, ""
}

// settleDecision is the pure part of waitGuestSettled: given the uptime, the time since the
// home screen and the quiet verdict of the last minute, is the guest settled, and if not, why.
func settleDecision(uptime, target, sinceHome time.Duration, quiet bool, why string) (bool, string) {
	switch {
	case uptime < target:
		return false, fmt.Sprintf("uptime %v < %v", uptime.Round(time.Second), target)
	case sinceHome < quietAfterHome:
		return false, fmt.Sprintf("%v since going home < %v", sinceHome.Round(time.Second), quietAfterHome)
	case !quiet:
		return false, why
	}
	return true, ""
}

// waitGuestSettled blocks until the guest has been up baselineUptime(), the home screen has been
// shown for quietAfterHome, and the last minute was quiet (quietVerdict: a Play Store app update
// downloads at about 1 MB/s with little CPU; a baseline saved during one resumed the download,
// the install and its dexopt after every reset); or until quietExtraLimit past the uptime target.
// Every sample has its own timeout and the whole wait a deadline, so a stuck adb read cannot hold
// the boot (and the device's lifecycle lock) for longer. It returns an error only when ctx is
// cancelled (a shutdown, or the daemon stopping); an adb error ends the wait and the baseline is
// saved as before.
func (d *device) waitGuestSettled(ctx context.Context, serial string, home time.Time) error {
	target := baselineUptime()
	if target <= 0 {
		return nil
	}
	t0 := time.Now()
	sample := func() (guestSample, error) {
		sctx, cancel := context.WithTimeout(ctx, quietSampleTimeout)
		defer cancel()
		return readGuestSampleFn(sctx, serial)
	}
	// Until the first sample gives the uptime, the deadline assumes a guest that just booted.
	deadline := t0.Add(target + quietExtraLimit + time.Minute)
	var prev *guestSample
	var win []quietSample
	quiet, why := false, "no sample yet"
	var last quietSample
	failures, first, refined := 0, true, false
	for {
		if prev != nil {
			ok, reason := settleDecision(prev.uptime, target, time.Since(home), quiet, why)
			if ok {
				logf("%s: guest settled after %v (uptime %v, last %.1f%% of a vCPU, %.1f KB/s received)", d.name, time.Since(t0).Round(time.Second), prev.uptime.Round(time.Second), last.busyPct, last.rxKBps)
				return nil
			}
			if prev.uptime > target+quietExtraLimit {
				logf("%s: guest not quiet %v past the uptime target (%s); saving the baseline anyway", d.name, quietExtraLimit, reason)
				return nil
			}
		}
		if time.Now().After(deadline) {
			logf("%s: the wait for a quiet guest reached its deadline after %v (%s, %d failed samples); saving the baseline anyway", d.name, time.Since(t0).Round(time.Second), why, failures)
			return nil
		}
		if !first {
			if err := sleepCtx(ctx, quietWindow); err != nil {
				return d.settleCancelled(ctx, t0)
			}
		}
		first = false
		cur, err := sample()
		if err != nil {
			if ctx.Err() != nil {
				return d.settleCancelled(ctx, t0)
			}
			// A failed or slow read says nothing about the guest being quiet: the quiet minute
			// starts over, and the wait goes on until its deadline.
			failures++
			if failures == 1 || failures%6 == 0 {
				logf("%s: could not read the guest's CPU (%v; %d failed samples so far); counting it as not quiet", d.name, err, failures)
			}
			prev, win, quiet, why = nil, nil, false, "a guest sample failed"
			continue
		}
		if prev == nil {
			if !refined {
				refined = true
				// The wait cannot outlast the uptime cap by much, whatever the samples do.
				deadline = t0.Add(target - cur.uptime + quietExtraLimit + time.Minute)
				logf("%s: waiting until the guest has been up %v and is quiet (uptime now %v; at most until %v of uptime; AGEMU_ANDROID_BASELINE_UPTIME_S=0 skips this)", d.name, target, cur.uptime.Round(time.Second), target+quietExtraLimit)
			}
			prev = &cur
			continue
		}
		last = quietSample{busyPct: busyPct(*prev, cur), rxKBps: rxKBps(*prev, cur)}
		win = append(win, last)
		if len(win) > quietWindows {
			win = win[len(win)-quietWindows:]
		}
		quiet, why = quietVerdict(win)
		prev = &cur
	}
}

func (d *device) settleCancelled(ctx context.Context, t0 time.Time) error {
	logf("%s: the wait for the guest to settle was cancelled after %v (%v); no baseline is saved", d.name, time.Since(t0).Round(time.Second), context.Cause(ctx))
	return fmt.Errorf("preparing the baseline was cancelled: %w", context.Cause(ctx))
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
