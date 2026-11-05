// Command agemu-bench measures agent-loop operations: the standard tool (baseline)
// versus agemu, on the same device, and prints a table with speedups.
//
//	agemu-bench --platform android --device android:agemu-pixel --n 20 --agemu ./bin/agemu
//
// Each platform registers a suite (android.go, ios.go) in this package via init().
// The agemu side is measured the way an agent uses it: by executing the agemu CLI
// (one process per call, like `adb shell input tap`). The persistent-connection
// latency (MCP / daemon client) is measured too and reported separately.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// Op is one measured operation. Baseline or Agemu may be nil when not applicable.
type Op struct {
	Name     string
	N        int          // iterations (0 = global --n)
	BaseN    int          // baseline iterations when the baseline is slow (0 = N)
	Setup    func() error // run once before the op's iterations (not timed)
	Before   func() error // run before every iteration (not timed)
	Baseline func() error
	BaseDesc string // e.g. "adb exec-out screencap -p"
	Agemu    func() error
	AgemDesc string       // e.g. "agemu screenshot"
	Daemon   func() error // same op over a persistent daemon connection (optional)
}

// Config is passed to suites.
type Config struct {
	Device   string
	AgemuBin string
	N        int
	OutDir   string
}

var suites = map[string]func(cfg Config) ([]Op, error){}

type Stats struct {
	N      int     `json:"n"`
	MinMs  float64 `json:"minMs"`
	MedMs  float64 `json:"medianMs"`
	P95Ms  float64 `json:"p95Ms"`
	MeanMs float64 `json:"meanMs"`
	Err    string  `json:"error,omitempty"`
}

type Result struct {
	Op        string  `json:"op"`
	Baseline  *Stats  `json:"baseline,omitempty"`
	BaseDesc  string  `json:"baselineTool,omitempty"`
	Agemu     *Stats  `json:"agemu,omitempty"`
	AgemDesc  string  `json:"agemuTool,omitempty"`
	Daemon    *Stats  `json:"daemon,omitempty"`
	SpeedupX  float64 `json:"speedupCli,omitempty"`
	SpeedupDX float64 `json:"speedupDaemon,omitempty"`
}

func measure(n int, before, fn func() error) *Stats {
	if fn == nil {
		return nil
	}
	var ds []float64
	for i := 0; i < n; i++ {
		if before != nil {
			if err := before(); err != nil {
				return &Stats{Err: "before: " + err.Error()}
			}
		}
		t := time.Now()
		if err := fn(); err != nil {
			return &Stats{N: len(ds), Err: err.Error()}
		}
		ds = append(ds, float64(time.Since(t).Microseconds())/1000)
	}
	sort.Float64s(ds)
	s := &Stats{N: len(ds), MinMs: ds[0], MedMs: ds[len(ds)/2], P95Ms: ds[min(len(ds)-1, int(float64(len(ds))*0.95))]}
	for _, d := range ds {
		s.MeanMs += d
	}
	s.MeanMs /= float64(len(ds))
	return s
}

// Run executes a command and returns an error including its output on failure.
func Run(name string, args ...string) error {
	_, err := Output(name, args...)
	return err
}

func Output(name string, args ...string) ([]byte, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func main() {
	platform := flag.String("platform", "", "android | ios")
	dev := flag.String("device", "", "device id, e.g. android:agemu-pixel")
	n := flag.Int("n", 20, "iterations per cheap op")
	bin := flag.String("agemu", "./bin/agemu", "path to the agemu binary")
	out := flag.String("out", "bench/out", "output directory for JSON + markdown")
	only := flag.String("only", "", "comma-separated op names to run")
	flag.Parse()

	suite, ok := suites[*platform]
	if !ok {
		var names []string
		for k := range suites {
			names = append(names, k)
		}
		fmt.Fprintf(os.Stderr, "unknown --platform %q (have: %s)\n", *platform, strings.Join(names, ", "))
		os.Exit(2)
	}
	absBin, _ := filepath.Abs(*bin)
	_ = os.MkdirAll(*out, 0o755)
	cfg := Config{Device: *dev, AgemuBin: absBin, N: *n, OutDir: *out}
	ops, err := suite(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "suite:", err)
		os.Exit(1)
	}
	want := map[string]bool{}
	for _, o := range strings.Split(*only, ",") {
		if o != "" {
			want[o] = true
		}
	}
	var results []Result
	for _, op := range ops {
		if len(want) > 0 && !want[op.Name] {
			continue
		}
		iters := op.N
		if iters == 0 {
			iters = *n
		}
		fmt.Fprintf(os.Stderr, "== %s (n=%d)\n", op.Name, iters)
		if op.Setup != nil {
			if err := op.Setup(); err != nil {
				fmt.Fprintf(os.Stderr, "   setup failed: %v\n", err)
				results = append(results, Result{Op: op.Name, Baseline: &Stats{Err: "setup: " + err.Error()}})
				continue
			}
		}
		r := Result{Op: op.Name, BaseDesc: op.BaseDesc, AgemDesc: op.AgemDesc}
		baseIters := iters
		if op.BaseN > 0 {
			baseIters = op.BaseN
		}
		r.Baseline = measure(baseIters, op.Before, op.Baseline)
		r.Agemu = measure(iters, op.Before, op.Agemu)
		r.Daemon = measure(iters, op.Before, op.Daemon)
		if r.Baseline != nil && r.Agemu != nil && r.Baseline.Err == "" && r.Agemu.Err == "" && r.Agemu.MedMs > 0 {
			r.SpeedupX = r.Baseline.MedMs / r.Agemu.MedMs
		}
		if r.Baseline != nil && r.Daemon != nil && r.Baseline.Err == "" && r.Daemon.Err == "" && r.Daemon.MedMs > 0 {
			r.SpeedupDX = r.Baseline.MedMs / r.Daemon.MedMs
		}
		fmt.Fprintf(os.Stderr, "   %s\n", line(r))
		results = append(results, r)
	}

	md := markdown(*platform, *dev, results)
	fmt.Print(md)
	stamp := time.Now().Format("20060102-150405")
	js, _ := json.MarshalIndent(map[string]any{"platform": *platform, "device": *dev, "host": host(), "time": time.Now().Format(time.RFC3339), "results": results}, "", "  ")
	_ = os.WriteFile(filepath.Join(*out, fmt.Sprintf("%s-%s.json", *platform, stamp)), js, 0o644)
	_ = os.WriteFile(filepath.Join(*out, fmt.Sprintf("%s-%s.md", *platform, stamp)), []byte(md), 0o644)
}

func fmtStats(s *Stats) string {
	if s == nil {
		return "-"
	}
	if s.Err != "" {
		return "ERR: " + s.Err
	}
	return fmt.Sprintf("%.1f ms (p95 %.1f, n=%d)", s.MedMs, s.P95Ms, s.N)
}

func line(r Result) string {
	return fmt.Sprintf("baseline %s | agemu cli %s | daemon %s | speedup cli %.1fx daemon %.1fx",
		fmtStats(r.Baseline), fmtStats(r.Agemu), fmtStats(r.Daemon), r.SpeedupX, r.SpeedupDX)
}

func host() string {
	out, _ := exec.Command("sysctl", "-n", "machdep.cpu.brand_string").Output()
	ver, _ := exec.Command("sw_vers", "-productVersion").Output()
	return fmt.Sprintf("%s, macOS %s, %s", strings.TrimSpace(string(out)), strings.TrimSpace(string(ver)), runtime.GOARCH)
}

func markdown(platform, dev string, rs []Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "### %s benchmark (%s)\n\nHost: %s. Device: `%s`. Median of n runs (p95 and n in parentheses).\n\n", platform, time.Now().Format("2006-01-02"), host(), dev)
	b.WriteString("| Operation | Baseline tool | Baseline | agemu CLI | agemu daemon/MCP | Speedup (CLI) | Speedup (daemon) |\n|---|---|---|---|---|---|---|\n")
	for _, r := range rs {
		sx, sd := "-", "-"
		if r.SpeedupX > 0 {
			sx = fmt.Sprintf("**%.1f×**", r.SpeedupX)
		}
		if r.SpeedupDX > 0 {
			sd = fmt.Sprintf("**%.1f×**", r.SpeedupDX)
		}
		fmt.Fprintf(&b, "| %s | `%s` | %s | %s | %s | %s | %s |\n", r.Op, r.BaseDesc, fmtStats(r.Baseline), fmtStats(r.Agemu), fmtStats(r.Daemon), sx, sd)
	}
	return b.String()
}
