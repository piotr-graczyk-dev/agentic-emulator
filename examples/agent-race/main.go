// agent-race runs one shopping task in the Agemu Shop example app (examples/shop) the way an
// agent's tool calls would, and records it: the screen (the emulator's own gRPC frame stream,
// host-side, so it does not touch the guest), every tool call with its start and end, and the
// CPU and memory of the emulator and of the control plane. compose.py turns two runs into a
// side-by-side video.
//
//	agent-race run --arm classic --avd Pixel_9_Pro --out runs/classic-1
//	agent-race run --arm agemu   --avd agemu-lab   --out runs/agemu-1 --agemu bin/agemu
//
// Both arms follow the same plan, step for step (see plan in plan.go). The classic arm uses the
// standard tools an agent with a shell has: adb shell input, adb exec-out uiautomator dump to
// read the screen (and to poll it until the expected screen shows), adb exec-out screencap. The
// agemu arm uses the agemu CLI, one process per call, with an action and its wait in one call.
// There is no model think time in either arm: the next call starts as soon as the previous one
// returns, so the difference is the tools' own time.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const appID = "dev.agemu.shop"

// Event is one tool call.
type Event struct {
	Step  int     `json:"step"`
	Label string  `json:"label"`
	Cmd   string  `json:"cmd"`
	Start float64 `json:"start"` // seconds since the task started
	End   float64 `json:"end"`
	OK    bool    `json:"ok"`
}

// Recorder is shared by the drivers: it runs commands and logs each as one tool call.
type Recorder struct {
	t0     time.Time
	mu     sync.Mutex
	events []Event
	step   int
	label  string
}

func (r *Recorder) since(t time.Time) float64 { return t.Sub(r.t0).Seconds() }

// run executes one tool call; display is how it is shown in the video.
func (r *Recorder) run(display string, name string, args ...string) ([]byte, error) {
	start := time.Now()
	out, err := exec.Command(name, args...).Output()
	end := time.Now()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			err = fmt.Errorf("%v: %s", err, strings.TrimSpace(string(ee.Stderr)))
		}
	}
	r.mu.Lock()
	r.events = append(r.events, Event{Step: r.step, Label: r.label, Cmd: display, Start: r.since(start), End: r.since(end), OK: err == nil})
	r.mu.Unlock()
	return out, err
}

func main() {
	if len(os.Args) < 2 || os.Args[1] != "run" {
		fmt.Fprintln(os.Stderr, "usage: agent-race run --arm classic|agemu --avd NAME --out DIR [--agemu PATH]")
		os.Exit(2)
	}
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	arm := fs.String("arm", "", "classic or agemu")
	avd := fs.String("avd", "", "AVD name of the running emulator")
	out := fs.String("out", "", "output directory")
	agemuBin := fs.String("agemu", "bin/agemu", "agemu CLI (agemu arm)")
	width := fs.Int("width", 540, "recorded frame width")
	_ = fs.Parse(os.Args[2:])
	if *out == "" || *avd == "" || (*arm != "classic" && *arm != "agemu") {
		fs.Usage()
		os.Exit(2)
	}
	if err := runRace(*arm, *avd, *out, *agemuBin, *width); err != nil {
		fmt.Fprintln(os.Stderr, "agent-race:", err)
		os.Exit(1)
	}
}

func runRace(arm, avd, out, agemuBin string, width int) error {
	emu, err := findEmulator(avd)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(out, "frames"), 0o755); err != nil {
		return err
	}
	agemuAbs, _ := filepath.Abs(agemuBin)
	rec := &Recorder{}
	var d Driver
	var plane []int // control-plane processes, sampled with the emulator
	switch arm {
	case "classic":
		c := &classic{r: rec, serial: emu.serial(), tmp: filepath.Join(out, "shots")}
		d = c
		plane = pgrep("adb", "fork-server")
	case "agemu":
		d = &agemuDriver{r: rec, bin: agemuAbs, dev: "android:" + avd, tmp: filepath.Join(out, "shots")}
		plane = pgrep("agemud", "")
	}
	_ = os.MkdirAll(filepath.Join(out, "shots"), 0o755)

	// Untimed: one warm-up launch (the first launch after a device resumes can take seconds),
	// then the app stopped and the home screen in front, so the timed launch is a cold start.
	adb := filepath.Join(androidSDK(), "platform-tools", "adb")
	_ = exec.Command(adb, "-s", emu.serial(), "shell", "am", "start", "-W", "-n", appID+"/.LoginActivity").Run()
	time.Sleep(1 * time.Second)
	_ = exec.Command(adb, "-s", emu.serial(), "shell", "am", "force-stop", appID).Run()
	_ = exec.Command(adb, "-s", emu.serial(), "shell", "input", "keyevent", "KEYCODE_HOME").Run()
	time.Sleep(1500 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	frames, err := startFrames(ctx, emu, width, filepath.Join(out, "frames"))
	if err != nil {
		return fmt.Errorf("frame stream: %w", err)
	}
	samples := startSampler(ctx, append([]int{emu.PID}, plane...), emu.PID)
	time.Sleep(1 * time.Second) // lead-in on the home screen

	rec.t0 = time.Now()
	frames.setT0(rec.t0)
	samples.setT0(rec.t0)
	result, runErr := runPlan(rec, d)
	total := time.Since(rec.t0).Seconds()
	time.Sleep(1500 * time.Millisecond) // the final screen
	cancel()
	frames.wait()
	samples.wait()

	load, _ := exec.Command("sysctl", "-n", "vm.loadavg").Output()
	summary := map[string]any{
		"arm":        arm,
		"avd":        avd,
		"ok":         runErr == nil,
		"error":      errString(runErr),
		"total_s":    total,
		"tool_calls": len(rec.events),
		"tool_s":     toolTime(rec.events),
		"result":     result,
		"steps":      stepNames(),
		"frame_w":    frames.w,
		"frame_h":    frames.h,
		"screen_w":   frames.sw,
		"screen_h":   frames.sh,
		"loadavg":    strings.TrimSpace(string(load)),
		"date":       time.Now().Format(time.RFC3339),
		"pids":       map[string]any{"emulator": emu.PID, "control_plane": plane},
	}
	if err := writeJSON(filepath.Join(out, "summary.json"), summary); err != nil {
		return err
	}
	if err := writeJSONL(filepath.Join(out, "events.jsonl"), rec.events); err != nil {
		return err
	}
	if err := writeJSONL(filepath.Join(out, "frames.jsonl"), frames.index); err != nil {
		return err
	}
	if err := writeJSONL(filepath.Join(out, "samples.jsonl"), samples.rows); err != nil {
		return err
	}
	fmt.Printf("%s: %.2f s, %d tool calls, ok=%v %s\n", arm, total, len(rec.events), runErr == nil, errString(runErr))
	return runErr
}

func toolTime(ev []Event) float64 {
	t := 0.0
	for _, e := range ev {
		t += e.End - e.Start
	}
	return t
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

func writeJSONL[T any](path string, rows []T) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	for _, r := range rows {
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	return w.Flush()
}

func androidSDK() string {
	for _, k := range []string{"ANDROID_HOME", "ANDROID_SDK_ROOT"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "Android", "sdk")
}

type emulator struct {
	PID       int
	Console   int
	GRPCPort  int
	GRPCToken string
}

func (e emulator) serial() string { return fmt.Sprintf("emulator-%d", e.Console) }

// findEmulator reads the emulator's discovery file (pid_<pid>.ini) for an AVD.
func findEmulator(avd string) (emulator, error) {
	home, _ := os.UserHomeDir()
	files, _ := filepath.Glob(filepath.Join(home, "Library", "Caches", "TemporaryItems", "avd", "running", "pid_*.ini"))
	for _, f := range files {
		pid, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(filepath.Base(f), "pid_"), ".ini"))
		if pid <= 0 || syscall.Kill(pid, 0) != nil {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		kv := map[string]string{}
		for _, line := range strings.Split(string(b), "\n") {
			if k, v, ok := strings.Cut(line, "="); ok {
				kv[k] = v
			}
		}
		if kv["avd.id"] != avd {
			continue
		}
		e := emulator{PID: pid, GRPCToken: kv["grpc.token"]}
		e.Console, _ = strconv.Atoi(kv["port.serial"])
		e.GRPCPort, _ = strconv.Atoi(kv["grpc.port"])
		if e.GRPCPort == 0 {
			return e, fmt.Errorf("emulator %s has no gRPC port (start it with -grpc <port>)", avd)
		}
		return e, nil
	}
	return emulator{}, fmt.Errorf("emulator for %s is not running", avd)
}

// pgrep returns the pids of processes whose name is name and whose arguments contain arg.
func pgrep(name, arg string) []int {
	out, _ := exec.Command("pgrep", "-x", name).Output()
	var pids []int
	for _, f := range strings.Fields(string(out)) {
		pid, _ := strconv.Atoi(f)
		if pid <= 0 {
			continue
		}
		if arg != "" {
			cmd, _ := exec.Command("ps", "-o", "args=", "-p", f).Output()
			if !strings.Contains(string(cmd), arg) {
				continue
			}
		}
		pids = append(pids, pid)
	}
	return pids
}
