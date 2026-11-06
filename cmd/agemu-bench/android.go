//go:build !noandroid

package main

// The Android suite. Every operation runs on the same agemu-* emulator:
//   - baseline: the standard tool, called directly against the device's serial
//     (adb exec-out screencap, adb shell input/uiautomator/am, a cold emulator boot),
//   - agemu CLI: one `agemu` process per call (what an agent shell-out costs),
//   - daemon: the same call over a persistent daemon connection (what the MCP server costs).
//
// Boot and reset use a cold boot with the same emulator flags as the baseline (N=3).
// "reset vs snapshot load" compares agemu reset with the emulator's own console command
// for loading the same snapshot, to show where the reset speed comes from.

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/client"
)

func init() { suites["android"] = androidSuite }

// stepBounds finds the bounds of Settings' "Network & internet" row in a uiautomator dump.
var stepBounds = regexp.MustCompile(`text="Network &amp; internet"[^>]*?bounds="\[(\d+),(\d+)\]\[(\d+),(\d+)\]"`)

func androidSDK() string {
	for _, k := range []string{"ANDROID_HOME", "ANDROID_SDK_ROOT"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "Android", "sdk")
}

// androidSerial finds the adb serial of the running emulator for an AVD (discovery files).
func androidSerial(avd string) (string, int, error) {
	home, _ := os.UserHomeDir()
	files, _ := filepath.Glob(filepath.Join(home, "Library", "Caches", "TemporaryItems", "avd", "running", "pid_*.ini"))
	for _, f := range files {
		pid, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(filepath.Base(f), "pid_"), ".ini"))
		if pid <= 0 || syscall.Kill(pid, 0) != nil {
			continue
		}
		fh, err := os.Open(f)
		if err != nil {
			continue
		}
		kv := map[string]string{}
		sc := bufio.NewScanner(fh)
		for sc.Scan() {
			if k, v, ok := strings.Cut(sc.Text(), "="); ok {
				kv[k] = v
			}
		}
		fh.Close()
		if kv["avd.id"] == avd {
			return "emulator-" + kv["port.serial"], pid, nil
		}
	}
	return "", 0, fmt.Errorf("emulator for %s is not running", avd)
}

func androidSuite(cfg Config) ([]Op, error) {
	dev := cfg.Device
	if dev == "" {
		dev = "android:agemu-pixel"
	}
	avd, ok := strings.CutPrefix(dev, "android:")
	if !ok {
		return nil, fmt.Errorf("--device must be android:<avd>")
	}
	adbBin := filepath.Join(androidSDK(), "platform-tools", "adb")
	emuBin := filepath.Join(androidSDK(), "emulator", "emulator")
	tmp := filepath.Join(cfg.OutDir, "tmp")
	_ = os.MkdirAll(tmp, 0o755)
	ag := func(args ...string) error {
		return Run(cfg.AgemuBin, append(args, "-d", dev)...)
	}

	// Start (or reuse) the daemon with the agemu binary, then hold one connection.
	if err := Run(cfg.AgemuBin, "ping"); err != nil {
		return nil, err
	}
	if err := Run(cfg.AgemuBin, "boot", dev); err != nil {
		return nil, err
	}
	cl, err := client.Dial()
	if err != nil {
		return nil, err
	}
	call := func(method string, p map[string]any) error {
		p["device"] = dev
		_, err := cl.Call(method, p, nil)
		return err
	}

	serial := func() string { s, _, _ := androidSerial(avd); return s }
	adb := func(args ...string) error { return Run(adbBin, append([]string{"-s", serial()}, args...)...) }
	adbOut := func(args ...string) (string, error) {
		out, err := Output(adbBin, append([]string{"-s", serial()}, args...)...)
		return string(out), err
	}

	// Resolve Settings' launcher activity once, for `am start -W -n`.
	settingsComp := "com.android.settings/.Settings"
	if out, err := adbOut("shell", "cmd package resolve-activity --brief -c android.intent.category.LAUNCHER com.android.settings | tail -1"); err == nil && strings.Contains(out, "/") {
		settingsComp = strings.TrimSpace(out)
	}

	home := func() error { return call("key", map[string]any{"key": "home"}) }
	settingsHome := func() error {
		if err := call("terminate", map[string]any{"app": "com.android.settings"}); err != nil {
			return err
		}
		return call("launch", map[string]any{"app": "com.android.settings"})
	}
	native := func() error { return call("set_profile", map[string]any{"profile": "pixel-9"}) }

	uiCalls := 0
	stepCalls := 0
	// plainField opens an empty new-contact form (Contacts is force-stopped first, so no text
	// from an earlier run is left and the cursor starts at the end) and focuses "First name".
	plainField := func() error {
		if err := call("terminate", map[string]any{"app": "com.google.android.contacts"}); err != nil {
			return err
		}
		if err := adb("shell", "am", "start", "-W", "-a", "android.intent.action.INSERT", "-t", "vnd.android.cursor.dir/contact"); err != nil {
			return err
		}
		sel := map[string]any{"text": "First name"}
		if err := call("wait", map[string]any{"selector": sel, "timeoutMs": 20000}); err != nil {
			return err
		}
		if err := call("tap", map[string]any{"selector": sel}); err != nil {
			return err
		}
		return call("wait", map[string]any{"selector": map[string]any{"role": "textfield"}, "timeoutMs": 5000})
	}
	shot := func(i *int) string { *i++; return filepath.Join(tmp, fmt.Sprintf("shot-%d.png", *i%4)) }
	// idle gives the app 500 ms (untimed) to finish handling the previous text change, as the
	// think time between an agent's calls would. Text changes run on the app's UI thread; a
	// back-to-back loop would measure how fast the app redraws, not how fast the tool is.
	idle := func() error { time.Sleep(500 * time.Millisecond); return nil }
	var si int

	// Cold boot with the same flags agemu uses, minus the snapshot; ready = sys.boot_completed.
	coldBoot := func() error {
		lf, err := os.Create(filepath.Join(tmp, "coldboot.log"))
		if err != nil {
			return err
		}
		cmd := exec.Command(emuBin, "-avd", avd, "-port", "5580", "-grpc", "8580", "-no-window", "-no-audio",
			"-no-boot-anim", "-gpu", "auto", "-memory", "2048", "-lowram", "-no-snapshot-load", "-no-snapshot-save")
		cmd.Stdout, cmd.Stderr = lf, lf
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			return err
		}
		go func() { _ = cmd.Wait(); lf.Close() }()
		deadline := time.Now().Add(240 * time.Second)
		for time.Now().Before(deadline) {
			if s, _, err := androidSerial(avd); err == nil {
				out, err := exec.Command(adbBin, "-s", s, "shell", "getprop", "sys.boot_completed").Output()
				if err == nil && strings.TrimSpace(string(out)) == "1" {
					return nil
				}
			}
			time.Sleep(50 * time.Millisecond)
		}
		return fmt.Errorf("cold boot timed out")
	}
	ensureBooted := func() error { return call("boot", map[string]any{}) }
	// powerOff stops the emulator process (a cold boot needs the AVD free).
	powerOff := func() error {
		s, pid, err := androidSerial(avd)
		if err != nil {
			return nil
		}
		_ = Run(adbBin, "-s", s, "emu", "kill")
		deadline := time.Now().Add(60 * time.Second)
		for syscall.Kill(pid, 0) == nil {
			if time.Now().After(deadline) {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
			time.Sleep(20 * time.Millisecond)
		}
		_, _ = cl.Call("devices", map[string]any{}, nil) // let the daemon notice
		return nil
	}
	// standby: running device -> agemu shutdown (baseline restored, VM paused).
	standby := func() error {
		if err := ensureBooted(); err != nil {
			return err
		}
		return call("shutdown", map[string]any{})
	}
	// phased returns a Before that runs first() for the first n calls (the baseline
	// iterations) and then() afterwards (agemu CLI and daemon iterations).
	phased := func(n int, first, then func() error) func() error {
		calls := 0
		return func() error {
			calls++
			if calls <= n {
				return first()
			}
			return then()
		}
	}
	// spaced: before every reset iteration (both tools, untimed) the device is booted and 3 s
	// pass. Emulator 37.1 deadlocks when an in-place snapshot load starts while the previous
	// load still restores textures in the background; agemu itself keeps loads 3 s apart, and
	// this keeps that wait out of the timed part, as an agent's work between resets would.
	spaced := func() error {
		if err := ensureBooted(); err != nil {
			return err
		}
		time.Sleep(3 * time.Second)
		return nil
	}
	waitAdbReady := func() error {
		deadline := time.Now().Add(60 * time.Second)
		for time.Now().Before(deadline) {
			out, err := exec.Command(adbBin, "-s", serial(), "shell", "getprop", "sys.boot_completed").Output()
			if err == nil && strings.TrimSpace(string(out)) == "1" {
				return nil
			}
			time.Sleep(20 * time.Millisecond)
		}
		return fmt.Errorf("adb not ready")
	}

	ops := []Op{
		{
			Name: "screenshot", Setup: func() error {
				if err := native(); err != nil {
					return err
				}
				return settingsHome()
			},
			BaseDesc: "adb exec-out screencap -p > f.png",
			Baseline: func() error {
				f, err := os.Create(shot(&si))
				if err != nil {
					return err
				}
				defer f.Close()
				c := exec.Command(adbBin, "-s", serial(), "exec-out", "screencap", "-p")
				c.Stdout = f
				return c.Run()
			},
			AgemDesc: "agemu screenshot -o f.png",
			Agemu:    func() error { return ag("screenshot", "-o", shot(&si)) },
			Daemon:   func() error { return call("screenshot", map[string]any{"path": shot(&si)}) },
		},
		{
			Name: "screenshot (small-phone profile)", Setup: func() error {
				return call("set_profile", map[string]any{"profile": "small-phone"})
			},
			BaseDesc: "adb exec-out screencap -p > f.png",
			Baseline: func() error {
				f, err := os.Create(shot(&si))
				if err != nil {
					return err
				}
				defer f.Close()
				c := exec.Command(adbBin, "-s", serial(), "exec-out", "screencap", "-p")
				c.Stdout = f
				return c.Run()
			},
			AgemDesc: "agemu screenshot -o f.png",
			Agemu:    func() error { return ag("screenshot", "-o", shot(&si)) },
			Daemon:   func() error { return call("screenshot", map[string]any{"path": shot(&si)}) },
		},
		{
			// The screen changes before every iteration (untimed): Settings scrolls by a swipe, then
			// 1.5 s pass, as between an agent's action and its next look at the screen. Every
			// iteration therefore encodes a new frame; agemu has received it from the emulator's
			// frame stream in the meantime.
			Name: "screenshot (after a screen change)", Setup: func() error {
				if err := native(); err != nil {
					return err
				}
				return settingsHome()
			},
			Before: func() func() error {
				down := false
				return func() error {
					down = !down
					y1, y2 := 1800, 900
					if !down {
						y1, y2 = 900, 1800
					}
					if err := call("swipe", map[string]any{"x": 540, "y": y1, "x2": 540, "y2": y2, "durationMs": 150}); err != nil {
						return err
					}
					time.Sleep(1500 * time.Millisecond)
					return nil
				}
			}(),
			BaseDesc: "adb exec-out screencap -p > f.png",
			Baseline: func() error {
				f, err := os.Create(shot(&si))
				if err != nil {
					return err
				}
				defer f.Close()
				c := exec.Command(adbBin, "-s", serial(), "exec-out", "screencap", "-p")
				c.Stdout = f
				return c.Run()
			},
			AgemDesc: "agemu screenshot -o f.png",
			Agemu:    func() error { return ag("screenshot", "-o", shot(&si)) },
			Daemon:   func() error { return call("screenshot", map[string]any{"path": shot(&si)}) },
		},
		{
			// Only one UiAutomation client can be connected, so uiautomator cannot dump while the
			// agemu agent runs: Setup stops the agent, and it is restarted (untimed) before the
			// first agemu iteration.
			Name: "ui tree", Setup: func() error {
				uiCalls = 0
				if err := native(); err != nil {
					return err
				}
				if err := settingsHome(); err != nil {
					return err
				}
				return adb("shell", "for p in $(pidof agemu-agent); do kill $p; done")
			},
			Before: func() error {
				uiCalls++
				if uiCalls == cfg.N+1 {
					return ag("ui")
				}
				return nil
			},
			BaseDesc: "adb shell uiautomator dump + adb exec-out cat",
			Baseline: func() error {
				if err := adb("shell", "uiautomator", "dump", "/sdcard/agemu-ui.xml"); err != nil {
					return err
				}
				out, err := adbOut("exec-out", "cat", "/sdcard/agemu-ui.xml")
				if err != nil || !strings.Contains(out, "<hierarchy") {
					return fmt.Errorf("no hierarchy: %v", err)
				}
				return nil
			},
			AgemDesc: "agemu ui --json",
			Agemu:    func() error { return ag("ui", "--json") },
			Daemon:   func() error { return call("ui", map[string]any{}) },
		},
		{
			// One complete agent step, as an agent loop does it: find "Network & internet" in the
			// UI tree, tap its centre, wait until the next screen shows "Internet", take a
			// screenshot. The baseline does the same with the standard tools (uiautomator dump to
			// find the element and to poll for the next screen, input tap, screencap). Before every
			// iteration (untimed) Settings is restarted on its home screen. uiautomator and the
			// agemu agent cannot hold UiAutomation at once, so Setup stops the agent and the
			// baseline runs first; the agent is restarted (untimed) before the agemu iterations.
			Name: "agent step: find + tap + wait for next screen + screenshot", N: 10,
			Setup: func() error {
				stepCalls = 0
				if err := native(); err != nil {
					return err
				}
				return adb("shell", "for p in $(pidof agemu-agent); do kill $p; done")
			},
			Before: func() error {
				stepCalls++
				if stepCalls <= 10 {
					return adb("shell", "am", "start", "-S", "-W", "-n", settingsComp)
				}
				if stepCalls == 11 {
					if err := ag("ui"); err != nil {
						return err
					}
				}
				return settingsHome()
			},
			BaseDesc: "uiautomator dump + input tap + uiautomator dump until next screen + screencap",
			Baseline: func() error {
				dump := func() (string, error) {
					if err := adb("shell", "uiautomator", "dump", "/sdcard/agemu-step.xml"); err != nil {
						return "", err
					}
					return adbOut("exec-out", "cat", "/sdcard/agemu-step.xml")
				}
				deadline := time.Now().Add(20 * time.Second)
				var x, y int
				for {
					xml, err := dump()
					if err != nil {
						return err
					}
					if m := stepBounds.FindStringSubmatch(xml); m != nil {
						x1, _ := strconv.Atoi(m[1])
						y1, _ := strconv.Atoi(m[2])
						x2, _ := strconv.Atoi(m[3])
						y2, _ := strconv.Atoi(m[4])
						x, y = (x1+x2)/2, (y1+y2)/2
						break
					}
					if time.Now().After(deadline) {
						return fmt.Errorf("Network & internet not found")
					}
				}
				if err := adb("shell", "input", "tap", strconv.Itoa(x), strconv.Itoa(y)); err != nil {
					return err
				}
				for {
					xml, err := dump()
					if err != nil {
						return err
					}
					if strings.Contains(xml, `text="Internet"`) {
						break
					}
					if time.Now().After(deadline) {
						return fmt.Errorf("next screen did not appear")
					}
				}
				f, err := os.Create(shot(&si))
				if err != nil {
					return err
				}
				defer f.Close()
				c := exec.Command(adbBin, "-s", serial(), "exec-out", "screencap", "-p")
				c.Stdout = f
				return c.Run()
			},
			AgemDesc: "agemu tap --text + agemu wait --text + agemu screenshot",
			Agemu: func() error {
				if err := ag("tap", "--text", "Network & internet"); err != nil {
					return err
				}
				if err := ag("wait", "--text", "Internet", "--timeout-ms", "20000"); err != nil {
					return err
				}
				return ag("screenshot", "-o", shot(&si))
			},
			Daemon: func() error {
				if err := call("tap", map[string]any{"selector": map[string]any{"text": "Network & internet"}}); err != nil {
					return err
				}
				if err := call("wait", map[string]any{"selector": map[string]any{"text": "Internet"}, "timeoutMs": 20000}); err != nil {
					return err
				}
				return call("screenshot", map[string]any{"path": shot(&si)})
			},
		},
		{
			// An empty spot of the home screen: taps there change nothing, so every iteration
			// does the same work.
			Name: "tap", Setup: home,
			BaseDesc: "adb shell input tap 800 1100",
			Baseline: func() error { return adb("shell", "input", "tap", "800", "1100") },
			AgemDesc: "agemu tap 800 1100",
			Agemu:    func() error { return ag("tap", "800", "1100") },
			Daemon:   func() error { return call("tap", map[string]any{"x": 800, "y": 1100}) },
		},
		{
			Name: "swipe (300 ms gesture)", Setup: settingsHome, InherentMs: 300,
			BaseDesc: "adb shell input swipe 540 1800 540 1000 300",
			Baseline: func() error { return adb("shell", "input", "swipe", "540", "1800", "540", "1000", "300") },
			AgemDesc: "agemu swipe 540 1800 540 1000 --ms 300",
			Agemu:    func() error { return ag("swipe", "540", "1800", "540", "1000", "--ms", "300") },
			Daemon: func() error {
				return call("swipe", map[string]any{"x": 540, "y": 1800, "x2": 540, "y2": 1000, "durationMs": 300})
			},
		},
		{
			Name: "swipe (50 ms gesture)", Setup: settingsHome, InherentMs: 50,
			BaseDesc: "adb shell input swipe 540 1800 540 1000 50",
			Baseline: func() error { return adb("shell", "input", "swipe", "540", "1800", "540", "1000", "50") },
			AgemDesc: "agemu swipe 540 1800 540 1000 --ms 50",
			Agemu:    func() error { return ag("swipe", "540", "1800", "540", "1000", "--ms", "50") },
			Daemon: func() error {
				return call("swipe", map[string]any{"x": 540, "y": 1800, "x2": 540, "y2": 1000, "durationMs": 50})
			},
		},
		{
			// A plain EditText (Contacts' new-contact form): measures input delivery.
			Name: "type text (plain field)", Setup: plainField, Before: idle,
			BaseDesc: "adb shell input text hello",
			Baseline: func() error { return adb("shell", "input", "text", "hello") },
			AgemDesc: "agemu type hello (sets the field's text)",
			Agemu:    func() error { return ag("type", "hello") },
			Daemon:   func() error { return call("type", map[string]any{"text": "hello"}) },
		},
		{
			// The same field with agemu's per-key mode (one key event per character, like input text).
			Name: "type text --keys (plain field)", Setup: plainField, Before: idle,
			BaseDesc: "adb shell input text hello",
			Baseline: func() error { return adb("shell", "input", "text", "hello") },
			AgemDesc: "agemu type --keys hello",
			Agemu:    func() error { return ag("type", "--keys", "hello") },
			Daemon:   func() error { return call("type", map[string]any{"text": "hello", "keys": true}) },
		},
		{
			// Settings search re-runs a query on every text change; both tools wait for the app.
			Name: "type text (Settings search)", Before: idle, Setup: func() error {
				if err := settingsHome(); err != nil {
					return err
				}
				// launch returns once Settings has drawn; its search bar may come a moment later.
				if err := call("wait", map[string]any{"selector": map[string]any{"text": "Search Settings"}, "timeoutMs": 10000}); err != nil {
					return err
				}
				if err := call("tap", map[string]any{"selector": map[string]any{"text": "Search Settings"}}); err != nil {
					return err
				}
				return call("wait", map[string]any{"selector": map[string]any{"role": "textfield"}, "timeoutMs": 5000})
			},
			BaseDesc: "adb shell input text hello",
			Baseline: func() error { return adb("shell", "input", "text", "hello") },
			AgemDesc: "agemu type hello (sets the field's text)",
			Agemu:    func() error { return ag("type", "hello") },
			Daemon:   func() error { return call("type", map[string]any{"text": "hello"}) },
		},
		{
			// agemu returns once the key is queued in InputManager (like tap); input keyevent waits
			// until the app has handled it.
			Name: "key home", Setup: home,
			BaseDesc: "adb shell input keyevent KEYCODE_HOME",
			Baseline: func() error { return adb("shell", "input", "keyevent", "KEYCODE_HOME") },
			AgemDesc: "agemu key home (returns when queued)",
			Agemu:    func() error { return ag("key", "home") },
			Daemon:   func() error { return call("key", map[string]any{"key": "home"}) },
		},
		{
			// Cold app start: Settings is force-stopped before every iteration (not timed).
			Name: "launch app (cold start)", N: 10,
			// A fresh Settings task (no leftover activities of other packages on top), then the
			// Settings process is killed before every iteration.
			Setup: func() error {
				return adb("shell", "am", "start", "-W", "--activity-clear-task", "-n", settingsComp)
			},
			Before: func() error {
				if err := adb("shell", "am", "force-stop", "com.android.settings"); err != nil {
					return err
				}
				return home()
			},
			BaseDesc: "adb shell am start -W -n " + settingsComp,
			Baseline: func() error {
				out, err := adbOut("shell", "am", "start", "-W", "-a", "android.intent.action.MAIN", "-c", "android.intent.category.LAUNCHER", "-n", settingsComp)
				if err == nil && !strings.Contains(out, "Status: ok") {
					return fmt.Errorf("am start: %s", out)
				}
				return err
			},
			AgemDesc: "agemu launch com.android.settings",
			Agemu:    func() error { return ag("launch", "com.android.settings") },
			Daemon:   func() error { return call("launch", map[string]any{"app": "com.android.settings"}) },
		},
		{
			// Warm start: Settings is running in the background (home pressed before each run).
			Name: "launch app (warm start)", N: 10,
			Setup: func() error {
				return adb("shell", "am", "start", "-W", "--activity-clear-task", "-n", settingsComp)
			},
			Before:   home,
			BaseDesc: "adb shell am start -W -n " + settingsComp,
			Baseline: func() error {
				out, err := adbOut("shell", "am", "start", "-W", "-a", "android.intent.action.MAIN", "-c", "android.intent.category.LAUNCHER", "-n", settingsComp)
				if err == nil && !strings.Contains(out, "Status: ok") {
					return fmt.Errorf("am start: %s", out)
				}
				return err
			},
			AgemDesc: "agemu launch com.android.settings",
			Agemu:    func() error { return ag("launch", "com.android.settings") },
			Daemon:   func() error { return call("launch", map[string]any{"app": "com.android.settings"}) },
		},
		{
			// Alternates small-phone <-> pixel-9 so every iteration is a real switch. agemu waits
			// until the foreground app has relaid out; `wm` returns before that.
			Name: "profile switch", N: 10, Setup: settingsHome,
			BaseDesc: "adb shell wm size WxH && adb shell wm density D",
			Baseline: func() func() error {
				small := false
				return func() error {
					small = !small
					size, dens := "720x1600", "320"
					if !small {
						size, dens = "reset", "reset"
					}
					if err := adb("shell", "wm", "size", size); err != nil {
						return err
					}
					return adb("shell", "wm", "density", dens)
				}
			}(),
			AgemDesc: "agemu profile small-phone|pixel-9 (waits for relayout)",
			Agemu: func() func() error {
				small := false
				return func() error {
					small = !small
					if small {
						return ag("profile", "small-phone")
					}
					return ag("profile", "pixel-9")
				}
			}(),
			Daemon: func() func() error {
				small := false
				return func() error {
					small = !small
					p := "pixel-9"
					if small {
						p = "small-phone"
					}
					return call("set_profile", map[string]any{"profile": p})
				}
			}(),
		},
		{
			// Standby is agemu's "shutdown": the baseline is restored and the VM paused, so boot
			// only resumes it. The baseline cold-boots a powered-off AVD.
			Name: "boot to agent-ready (from standby)", N: 3, BaseN: 3,
			Setup:    native,
			Before:   phased(3, powerOff, standby),
			BaseDesc: "emulator cold boot (same flags, -no-snapshot-load) until sys.boot_completed=1",
			Baseline: coldBoot,
			AgemDesc: "agemu boot (resume from standby + agent answering)",
			Agemu:    func() error { return Run(cfg.AgemuBin, "boot", dev) },
			Daemon:   func() error { return call("boot", map[string]any{}) },
		},
		{
			// No standby: a new emulator process loads the baseline snapshot.
			Name: "boot to agent-ready (powered off)", N: 3, BaseN: 3,
			Before:   powerOff,
			BaseDesc: "emulator cold boot (same flags, -no-snapshot-load) until sys.boot_completed=1",
			Baseline: coldBoot,
			AgemDesc: "agemu boot (new emulator process + baseline snapshot + agent answering)",
			Agemu:    func() error { return Run(cfg.AgemuBin, "boot", dev) },
			Daemon:   func() error { return call("boot", map[string]any{}) },
		},
		{
			Name: "reset to clean state", N: 3, BaseN: 3,
			Before:   spaced,
			BaseDesc: "adb emu kill + emulator cold boot until sys.boot_completed=1",
			Baseline: func() error {
				_, pid, err := androidSerial(avd)
				if err != nil {
					return err
				}
				if err := adb("emu", "kill"); err != nil {
					return err
				}
				for syscall.Kill(pid, 0) == nil {
					time.Sleep(20 * time.Millisecond)
				}
				return coldBoot()
			},
			AgemDesc: "agemu reset (snapshot load in place + agent answering)",
			Agemu:    func() error { return ag("reset") },
			Daemon:   func() error { return call("reset", map[string]any{}) },
		},
		{
			Name: "reset vs emulator snapshot load", N: 3, BaseN: 3,
			Before:   spaced,
			BaseDesc: "adb emu avd snapshot load agemu-baseline until sys.boot_completed=1",
			Baseline: func() error {
				out, err := adbOut("emu", "avd", "snapshot", "load", "agemu-baseline")
				if err != nil || !strings.Contains(out, "OK") {
					return fmt.Errorf("snapshot load: %v %s", err, out)
				}
				return waitAdbReady()
			},
			AgemDesc: "agemu reset",
			Agemu:    func() error { return ag("reset") },
			Daemon:   func() error { return call("reset", map[string]any{}) },
		},
	}
	return ops, nil
}
