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
		quiet    int
		ok       bool
		why      string
	}{
		{100 * time.Second, 90 * time.Second, quietWindows, false, "uptime"},
		{500 * time.Second, 30 * time.Second, quietWindows, false, "since going home"},
		{500 * time.Second, 90 * time.Second, quietWindows - 1, false, "quiet samples"},
		{500 * time.Second, 90 * time.Second, quietWindows, true, ""},
		{420 * time.Second, 60 * time.Second, quietWindows, true, ""},
	}
	for _, c := range cases {
		ok, why := settleDecision(c.up, target, c.home, c.quiet)
		if ok != c.ok || !strings.Contains(why, c.why) {
			t.Errorf("settleDecision(%v, %v, %d) = %v %q, want %v ~%q", c.up, c.home, c.quiet, ok, why, c.ok, c.why)
		}
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
