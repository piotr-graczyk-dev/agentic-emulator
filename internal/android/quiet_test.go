package android

import (
	"strings"
	"testing"
	"time"
)

func TestProcStatBusy(t *testing.T) {
	// The first lines of a guest's /proc/stat (user nice system idle iowait irq softirq steal ...).
	stat := "cpu  131 5 183 23509 12 167 6 0 0 0\ncpu0 30 1 40 5800 3 40 1 0 0 0\n"
	busy, total, err := procStatBusy(stat)
	if err != nil {
		t.Fatal(err)
	}
	if busy != 131+5+183+167+6 || total != busy+23509+12 {
		t.Fatalf("busy %d total %d", busy, total)
	}
	if _, _, err := procStatBusy("cpu0 1 2 3 4 5 6 7\n"); err == nil {
		t.Fatal("a stat without the aggregate cpu line must fail")
	}
	if _, _, err := procStatBusy("cpu  1 2 x 4 5 6 7\n"); err == nil {
		t.Fatal("a bad field must fail")
	}
}

func TestParseUptime(t *testing.T) {
	d, err := parseUptime("456.07 1661.41\n")
	if err != nil || d != 456070*time.Millisecond {
		t.Fatalf("got %v %v", d, err)
	}
	if _, err := parseUptime(""); err == nil {
		t.Fatal("empty must fail")
	}
}

func TestBusyPct(t *testing.T) {
	t0 := time.Unix(1000, 0)
	a := guestSample{at: t0, busy: 1000}
	b := guestSample{at: t0.Add(10 * time.Second), busy: 1250} // 2.5 s of CPU in 10 s
	if got := busyPct(a, b); got != 25 {
		t.Fatalf("got %v, want 25 (%% of one vCPU)", got)
	}
	if got := busyPct(b, b); got != 0 {
		t.Fatalf("zero elapsed: %v", got)
	}
}

func TestSettleDecision(t *testing.T) {
	target := 420 * time.Second
	cases := []struct {
		up, home time.Duration
		quiet    bool
		ok       bool
		why      string
	}{
		{100 * time.Second, 90 * time.Second, true, false, "uptime"},
		{500 * time.Second, 30 * time.Second, true, false, "since going home"},
		{500 * time.Second, 90 * time.Second, false, false, "busy"},
		{500 * time.Second, 90 * time.Second, true, true, ""},
		{420 * time.Second, 60 * time.Second, true, true, ""},
	}
	for _, c := range cases {
		ok, why := settleDecision(c.up, target, c.home, c.quiet, "busy")
		if ok != c.ok || !strings.Contains(why, c.why) {
			t.Errorf("settleDecision(%v, %v, %v) = %v %q, want %v ~%q", c.up, c.home, c.quiet, ok, why, c.ok, c.why)
		}
	}
}

func TestQuietVerdict(t *testing.T) {
	q := func(pcts ...float64) []quietSample {
		var w []quietSample
		for _, p := range pcts {
			w = append(w, quietSample{busyPct: p, rxKBps: 1})
		}
		return w
	}
	cases := []struct {
		name string
		win  []quietSample
		ok   bool
		why  string
	}{
		{"too few samples", q(5, 5, 5), false, "of 6 samples"},
		{"settled guest on an idle host", q(5, 6, 4, 7, 5, 6), true, ""},
		// The skeptic's settled guests read 5.5-11% per sample on a loaded host: a
		// per-sample 10% threshold kept failing, the mean passes.
		{"settled guest on a loaded host", q(9, 11, 10, 9.5, 11, 10), true, ""},
		{"busy minute", q(16, 14, 18, 15, 16, 17), false, "mean"},
		{"one burst (a job, a dexopt)", q(5, 5, 45, 5, 5, 5), false, "a sample at 45.0%"},
		{"older samples do not count", append(q(80, 80), q(5, 5, 5, 5, 5, 5)...), true, ""},
	}
	for _, c := range cases {
		ok, why := quietVerdict(c.win)
		if ok != c.ok || !strings.Contains(why, c.why) {
			t.Errorf("%s: quietVerdict = %v %q, want %v ~%q", c.name, ok, why, c.ok, c.why)
		}
	}
	// A download: little CPU, but bytes coming in.
	w := q(5, 5, 5, 5, 5, 5)
	w[3].rxKBps = 900
	if ok, why := quietVerdict(w); ok || !strings.Contains(why, "receiving") {
		t.Errorf("download: %v %q", ok, why)
	}
}

func TestPrepMaxCoversTheWait(t *testing.T) {
	t.Setenv("AGEMU_ANDROID_BASELINE_UPTIME_S", "")
	if got := prepMax(); got < baselineUptime()+quietExtraLimit || got > 20*time.Minute {
		t.Fatalf("prepMax %v", got)
	}
}

func TestBaselineUptimeEnv(t *testing.T) {
	t.Setenv("AGEMU_ANDROID_BASELINE_UPTIME_S", "")
	if baselineUptime() != 420*time.Second {
		t.Fatalf("default %v", baselineUptime())
	}
	t.Setenv("AGEMU_ANDROID_BASELINE_UPTIME_S", "0")
	if baselineUptime() != 0 {
		t.Fatal("0 must turn the wait off")
	}
	t.Setenv("AGEMU_ANDROID_BASELINE_UPTIME_S", "600")
	if baselineUptime() != 600*time.Second {
		t.Fatal("override")
	}
}

func TestZeroFillMB(t *testing.T) {
	mi := "MemTotal:        2041792 kB\nMemFree:          237712 kB\nMemAvailable:     658944 kB\n"
	if meminfoKB(mi, "MemFree") != 237712 || meminfoKB(mi, "Nope") != 0 {
		t.Fatal("meminfoKB")
	}
	if got := zeroFillMB(400, mi); got != 400 {
		t.Fatalf("400 fits: got %d", got)
	}
	if got := zeroFillMB(1000, mi); got != 643-zeroMarginMB {
		t.Fatalf("capped: got %d", got)
	}
	if got := zeroFillMB(400, "MemAvailable: 100000 kB\n"); got != 0 {
		t.Fatalf("too little: got %d", got)
	}
}

func TestNetRx(t *testing.T) {
	netdev := `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo:  123456     100    0    0    0     0          0         0   123456     100    0    0    0     0       0          0
 wlan0: 1000000    2000    0    0    0     0          0         0   50000     300    0    0    0     0       0          0
radio0:    2048      10    0    0    0     0          0         0       0       0    0    0    0     0       0          0
`
	if got := netRxBytes(netdev); got != 1002048 {
		t.Fatalf("rx %d", got)
	}
	t0 := time.Unix(1000, 0)
	a := guestSample{at: t0, rx: 0}
	b := guestSample{at: t0.Add(10 * time.Second), rx: 10 * 1024 * 1024}
	if got := rxKBps(a, b); got != 1024 {
		t.Fatalf("rate %v", got)
	}
}
