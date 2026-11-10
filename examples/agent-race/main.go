//go:build !windows

// agent-race runs one shopping task in the Agemu Shop example app (examples/shop) the way an
// agent's tool calls would, and records it: the screen (host-side: the emulator's gRPC frame
// stream, or the Simulator's own video), every tool call with its start and end, and the CPU and
// memory of the device and of the tool's host processes. compose.py renders runs side by side.
//
//	agent-race run --platform android --arm classic      --device Pixel_9_Pro --out runs/classic-1
//	agent-race run --platform android --arm agent-device --device Pixel_9_Pro --out runs/agent-device-1
//	agent-race run --platform android --arm argent       --device Pixel_9_Pro --out runs/argent-1
//	agent-race run --platform android --arm agemu        --device agemu-lab   --out runs/agemu-1
//	agent-race run --platform ios     --arm agemu        --device <udid>      --out runs/agemu-1
//
// Every arm follows the same plan (plan.go); only the tools differ (drivers*.go):
//   - classic (Android): adb shell input, adb exec-out uiautomator dump to read the screen and to
//     poll it until the expected screen shows, adb exec-out screencap;
//   - agent-device: its CLI, press/fill by selector, wait text, scroll --until, snapshot;
//   - argent: its CLI (argent run <tool>), describe + gesture-tap, fill, await-ui-element;
//   - agemu: the agemu CLI, one process per call, an action and its wait in one call.
//
// There is no model think time: the next call starts as soon as the previous one returns, so the
// differences are the tools' own time.
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
			err = fmt.Errorf("%v: %s", err, lastLine(string(ee.Stderr)))
		} else if len(out) > 0 {
			err = fmt.Errorf("%v: %s", err, lastLine(string(out)))
		}
	}
	r.mu.Lock()
	r.events = append(r.events, Event{Step: r.step, Label: r.label, Cmd: display, Start: r.since(start), End: r.since(end), OK: err == nil})
	r.mu.Unlock()
	return out, err
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "\n"); i >= 0 && len(s)-i < 400 {
		return s[i+1:]
	}
	if len(s) > 400 {
		return s[len(s)-400:]
	}
	return s
}

var arms = map[string]bool{"classic": true, "agemu": true, "agent-device": true, "argent": true}

func main() {
	if len(os.Args) < 2 || os.Args[1] != "run" {
		fmt.Fprintln(os.Stderr, "usage: agent-race run --platform android|ios --arm classic|agemu|agent-device|argent --device NAME --out DIR")
		os.Exit(2)
	}
	home, _ := os.UserHomeDir()
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	o := opts{}
	fs.StringVar(&o.platform, "platform", "android", "android or ios")
	fs.StringVar(&o.arm, "arm", "", "classic (Android), agemu, agent-device or argent")
	fs.StringVar(&o.device, "device", "", "Android: the running emulator's AVD name; iOS: the simulator's UDID")
	fs.StringVar(&o.out, "out", "", "output directory")
	fs.StringVar(&o.agemu, "agemu", "bin/agemu", "agemu CLI")
	fs.StringVar(&o.tools, "tools", filepath.Join(home, ".cache", "agemu-race-tools", "node_modules", ".bin"),
		"directory with the agent-device and argent CLIs")
	fs.IntVar(&o.width, "width", 540, "recorded frame width")
	_ = fs.Parse(os.Args[2:])
	if o.out == "" || o.device == "" || !arms[o.arm] || (o.platform != "android" && o.platform != "ios") ||
		(o.arm == "classic" && o.platform != "android") {
		fs.Usage()
		os.Exit(2)
	}
	if err := runRace(o); err != nil {
		fmt.Fprintln(os.Stderr, "agent-race:", err)
		os.Exit(1)
	}
}

type opts struct {
	platform, arm, device, out, agemu, tools string
	width                                    int
}

func runRace(o opts) error {
	if err := os.MkdirAll(filepath.Join(o.out, "frames"), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(o.out, "shots"), 0o755); err != nil {
		return err
	}
	agemuAbs, _ := filepath.Abs(o.agemu)
	shots := filepath.Join(o.out, "shots")
	rec := &Recorder{}
	ios := o.platform == "ios"

	var emu emulator
	var devSel func(map[int]proc) []int
	var agemuID string
	var adSel []string
	var udid string // Argent's device id: the iOS UDID or the Android serial
	if ios {
		udid = o.device
		devSel = simTree(o.device)
		agemuID = "ios:" + o.device
		adSel = []string{"--platform", "ios", "--udid", o.device}
	} else {
		var err error
		if emu, err = findEmulator(o.device); err != nil {
			return err
		}
		udid = emu.serial()
		devSel = byPID(emu.PID)
		agemuID = "android:" + o.device
		adSel = []string{"--platform", "android", "--serial", emu.serial()}
	}

	var d Driver
	var plane func(map[int]proc) []int
	var warm [][]string // untimed: start the tool's daemons before the task
	adBin, arBin := filepath.Join(o.tools, "agent-device"), filepath.Join(o.tools, "argent")
	switch o.arm {
	case "classic":
		d = &classic{r: rec, serial: emu.serial(), tmp: shots}
		plane = byArgs([]string{"adb", "fork-server"})
		warm = [][]string{{adbPath(), "-s", emu.serial(), "shell", "true"}}
	case "agemu":
		d = &agemuDriver{r: rec, bin: agemuAbs, dev: agemuID, tmp: shots}
		plane = byArgs([]string{"agemud"}, []string{"agemu-ios-helper"})
		warm = [][]string{{agemuAbs, "ui", "-d", agemuID}}
	case "agent-device":
		d = &agentDevice{r: rec, bin: adBin, tmp: shots, sel: adSel, plat: o.platform}
		plane = byArgs([]string{"agent-device"})
		warm = [][]string{append([]string{adBin, "open", appID, "--relaunch"}, adSel...), append([]string{adBin, "snapshot"}, adSel...)}
	case "argent":
		d = &argent{r: rec, bin: arBin, udid: udid, tmp: shots, plat: o.platform}
		plane = byArgs([]string{"argent"})
		warm = [][]string{{arBin, "run", "launch-app", "--udid", udid, "--bundleId", appID}, {arBin, "run", "describe", "--udid", udid}}
	}
	for _, w := range warm { // a daemon's first start can time out; it is retried
		var err error
		var out []byte
		for try := 0; try < 3; try++ {
			if out, err = exec.Command(w[0], w[1:]...).CombinedOutput(); err == nil {
				break
			}
			time.Sleep(3 * time.Second)
		}
		if err != nil {
			return fmt.Errorf("warm-up %v: %v: %s", w, err, lastLine(string(out)))
		}
	}

	// Untimed: one launch (the first launch after a device resumes can take seconds), then the
	// app stopped and the home screen in front, so the timed launch is a cold start.
	if ios {
		_ = exec.Command("xcrun", "simctl", "launch", o.device, appID).Run()
		time.Sleep(1 * time.Second)
		_ = exec.Command("xcrun", "simctl", "terminate", o.device, appID).Run()
		time.Sleep(2 * time.Second)
	} else {
		adb := adbPath()
		_ = exec.Command(adb, "-s", emu.serial(), "shell", "am", "start", "-W", "-n", appID+"/.LoginActivity").Run()
		time.Sleep(1 * time.Second)
		_ = exec.Command(adb, "-s", emu.serial(), "shell", "am", "force-stop", appID).Run()
		_ = exec.Command(adb, "-s", emu.serial(), "shell", "input", "keyevent", "KEYCODE_HOME").Run()
		time.Sleep(1500 * time.Millisecond)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var frames frameSource
	var err error
	if ios {
		frames, err = startIOSRecording(ctx, o.device, o.width, filepath.Join(o.out, "frames"))
	} else {
		frames, err = startFrames(ctx, emu, o.width, filepath.Join(o.out, "frames"))
	}
	if err != nil {
		return fmt.Errorf("recording: %w", err)
	}
	samples := startSampler(ctx, procSel{device: devSel, plane: plane})
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

	fw, fh, sw, sh := frames.size()
	load, _ := exec.Command("sysctl", "-n", "vm.loadavg").Output()
	summary := map[string]any{
		"platform":   o.platform,
		"arm":        o.arm,
		"device":     o.device,
		"ok":         runErr == nil,
		"error":      errString(runErr),
		"total_s":    total,
		"tool_calls": len(rec.events),
		"tool_s":     toolTime(rec.events),
		"result":     result,
		"steps":      stepNames(),
		"frame_w":    fw,
		"frame_h":    fh,
		"screen_w":   sw,
		"screen_h":   sh,
		"loadavg":    strings.TrimSpace(string(load)),
		"date":       time.Now().Format(time.RFC3339),
		"versions":   toolVersions(o, adBin, arBin, agemuAbs),
	}
	if err := writeJSON(filepath.Join(o.out, "summary.json"), summary); err != nil {
		return err
	}
	if err := writeJSONL(filepath.Join(o.out, "events.jsonl"), rec.events); err != nil {
		return err
	}
	if err := writeJSONL(filepath.Join(o.out, "frames.jsonl"), frames.frames()); err != nil {
		return err
	}
	if err := writeJSONL(filepath.Join(o.out, "samples.jsonl"), samples.rows); err != nil {
		return err
	}
	fmt.Printf("%s %s: %.2f s, %d tool calls, ok=%v %s\n", o.platform, o.arm, total, len(rec.events), runErr == nil, errString(runErr))
	return runErr
}

func toolVersions(o opts, adBin, arBin, agemuBin string) map[string]string {
	v := map[string]string{}
	get := func(name string, args ...string) string {
		out, _ := exec.Command(name, args...).Output()
		return lastLine(string(out))
	}
	switch o.arm {
	case "agent-device":
		v["agent-device"] = get(adBin, "--version")
	case "argent":
		v["argent"] = get(arBin, "--version")
	case "agemu":
		v["agemu"] = get("git", "-C", filepath.Dir(agemuBin), "rev-parse", "--short", "HEAD")
	}
	return v
}

func adbPath() string { return filepath.Join(androidSDK(), "platform-tools", "adb") }

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
