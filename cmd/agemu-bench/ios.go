//go:build !noios && !windows

package main

// The iOS suite. Every operation runs on the same agemu-* simulator:
//   - baseline: the standard tool, called directly (xcrun simctl; Maestro where simctl has
//     no equivalent: tap, swipe, type, UI tree),
//   - agemu CLI: one `agemu` process per call (what an agent shell-out costs),
//   - daemon: the same call over a persistent daemon connection (what the MCP server costs).
//
// Maestro is the baseline for tap / swipe / type / UI tree because simctl cannot do them.
// Maestro is a JVM CLI that drives an XCUITest runner it starts on the simulator, so a single
// Maestro call includes JVM start-up and runner start-up. Rows marked "x10" run ten operations
// inside ONE Maestro flow, which amortizes that start-up, against ten agemu calls.

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/api"
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/client"
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/daemon"
)

func init() { suites["ios"] = iosSuite }

const settingsApp = "com.apple.Preferences"

type iosBench struct {
	cfg     Config
	id      string
	udid    string
	dir     string
	maestro string
	cl      *client.Client
	dev     api.Device
	seUDID  string
}

func findMaestro() string {
	if p := os.Getenv("MAESTRO"); p != "" {
		return p
	}
	if p, err := exec.LookPath("maestro"); err == nil {
		return p
	}
	home, _ := os.UserHomeDir()
	p := filepath.Join(home, ".maestro", "bin", "maestro")
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return ""
}

func (b *iosBench) agemu(args ...string) error {
	return Run(b.cfg.AgemuBin, append(args, "-d", b.id)...)
}

func (b *iosBench) call(method string, p daemon.Params) error {
	p.Device = b.id
	_, err := b.cl.Call(method, p, nil)
	return err
}

func (b *iosBench) simctl(args ...string) error {
	return Run("xcrun", append([]string{"simctl"}, args...)...)
}

// maestroFlow writes a flow file once and returns a baseline func that runs it.
func (b *iosBench) maestroFlow(name string, steps ...string) func() error {
	if b.maestro == "" {
		return nil
	}
	path := filepath.Join(b.dir, name+".yaml")
	body := "appId: " + settingsApp + "\n---\n" + strings.Join(steps, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return func() error { return err }
	}
	return func() error { return Run(b.maestro, "--device", b.udid, "test", path) }
}

func repeatStep(step string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = step
	}
	return out
}

func times(n int, fn func() error) func() error {
	return func() error {
		for i := 0; i < n; i++ {
			if err := fn(); err != nil {
				return err
			}
		}
		return nil
	}
}

func ip(v int) *int { return &v }

// terminateSettings stops Settings if it runs (not running is fine).
func (b *iosBench) terminateSettings() error {
	_ = b.agemu("terminate", settingsApp)
	time.Sleep(300 * time.Millisecond)
	return nil
}

// settings brings Settings to its root screen (not timed).
func (b *iosBench) settings() error {
	_ = b.agemu("terminate", settingsApp)
	if err := b.agemu("launch", settingsApp); err != nil {
		return err
	}
	return b.agemu("wait", "--id", "com.apple.settings.general", "--timeout-ms", "20000")
}

func (b *iosBench) waitShutdown() error { return b.waitShutdownUDID(b.udid) }

func (b *iosBench) waitShutdownUDID(udid string) error {
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		out, err := Output("xcrun", "simctl", "list", "devices", "-j")
		if err != nil {
			return err
		}
		var v struct {
			Devices map[string][]struct {
				UDID  string `json:"udid"`
				State string `json:"state"`
			} `json:"devices"`
		}
		if err := json.Unmarshal(out, &v); err != nil {
			return err
		}
		for _, ds := range v.Devices {
			for _, d := range ds {
				if d.UDID == udid && d.State == "Shutdown" {
					return nil
				}
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("%s did not shut down", udid)
}

func (b *iosBench) shutdown() error { return b.shutdownUDID(b.udid) }

func (b *iosBench) shutdownUDID(udid string) error {
	if err := b.simctl("shutdown", udid); err != nil && !strings.Contains(err.Error(), "current state: Shutdown") {
		return err
	}
	return b.waitShutdownUDID(udid)
}

// otherProfile returns the udid of agemu-iphone-se-3 (created if missing), the target of the
// profile-switch row.
func (b *iosBench) otherProfile() (string, error) {
	if b.seUDID != "" {
		return b.seUDID, nil
	}
	out, err := Output(b.cfg.AgemuBin, "create", "--platform", "ios", "--profile", "iphone-se-3")
	if err != nil {
		return "", err
	}
	var d api.Device
	if err := json.Unmarshal(out, &d); err != nil {
		return "", fmt.Errorf("create: %v: %s", err, out)
	}
	b.seUDID = strings.TrimPrefix(d.ID, "ios:")
	return b.seUDID, nil
}

func iosSuite(cfg Config) ([]Op, error) {
	b := &iosBench{cfg: cfg, maestro: findMaestro()}
	b.dir, _ = filepath.Abs(filepath.Join(cfg.OutDir, "ios-work"))
	if err := os.MkdirAll(b.dir, 0o755); err != nil {
		return nil, err
	}
	// Start the daemon from the agemu binary (client.Dial would start this bench binary).
	if err := Run(cfg.AgemuBin, "ping"); err != nil {
		return nil, err
	}
	b.id = cfg.Device
	if b.id == "" {
		out, err := Output(cfg.AgemuBin, "create", "--platform", "ios", "--profile", "iphone-17-pro")
		if err != nil {
			return nil, err
		}
		var d api.Device
		if err := json.Unmarshal(out, &d); err != nil {
			return nil, fmt.Errorf("create: %v: %s", err, out)
		}
		b.id = d.ID
	}
	if !strings.HasPrefix(b.id, "ios:") {
		return nil, fmt.Errorf("--device must be an ios:<udid> id, got %q", b.id)
	}
	b.udid = strings.TrimPrefix(b.id, "ios:")
	out, err := Output(cfg.AgemuBin, "boot", b.id)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(out, &b.dev); err != nil || b.dev.Screen == nil {
		return nil, fmt.Errorf("boot: %v: %s", err, out)
	}
	if b.cl, err = client.Dial(); err != nil {
		return nil, err
	}
	w, h := b.dev.Screen.Width, b.dev.Screen.Height
	// A harmless tap target: the "Settings" large title (no action). In pixels for agemu,
	// in percent of the screen for Maestro.
	tx, ty := w*21/100, h*16/100
	tapStep := `- tapOn:
    point: "21%,16%"`
	// Settings' search field, at the bottom of the screen on iOS 26.
	sx, sy := w/2, h*94/100
	swipeStep := `- swipe:
    start: "50%,65%"
    end: "50%,50%"
    duration: 300`
	maestroDesc := func(s string) string {
		if b.maestro == "" {
			return "maestro (not installed)"
		}
		return s
	}

	ops := []Op{
		{
			Name: "screenshot", Setup: b.settings,
			BaseDesc: "xcrun simctl io <udid> screenshot (png)",
			Baseline: func() error { return b.simctl("io", b.udid, "screenshot", filepath.Join(b.dir, "simctl.png")) },
			AgemDesc: "agemu screenshot (png)",
			Agemu:    func() error { return b.agemu("screenshot", "-o", filepath.Join(b.dir, "agemu.png")) },
			Daemon: func() error {
				return b.call("screenshot", daemon.Params{Path: filepath.Join(b.dir, "daemon.png"), Format: "png"})
			},
		},
		{
			Name:     "screenshot-jpeg",
			BaseDesc: "xcrun simctl io <udid> screenshot --type=jpeg",
			Baseline: func() error {
				return b.simctl("io", b.udid, "screenshot", "--type=jpeg", filepath.Join(b.dir, "simctl.jpg"))
			},
			AgemDesc: "agemu screenshot --format jpeg",
			Agemu: func() error {
				return b.agemu("screenshot", "--format", "jpeg", "-o", filepath.Join(b.dir, "agemu.jpg"))
			},
			Daemon: func() error {
				return b.call("screenshot", daemon.Params{Path: filepath.Join(b.dir, "daemon.jpg"), Format: "jpeg"})
			},
		},
		{
			Name: "ui-tree", BaseN: 3, Setup: b.settings,
			BaseDesc: maestroDesc("maestro hierarchy (JVM + XCUITest runner per call)"),
			Baseline: func() error {
				if b.maestro == "" {
					return fmt.Errorf("maestro not installed")
				}
				return Run(b.maestro, "--device", b.udid, "hierarchy")
			},
			AgemDesc: "agemu ui",
			Agemu:    func() error { return b.agemu("ui") },
			Daemon:   func() error { return b.call("ui", daemon.Params{}) },
		},
		{
			Name: "find-element x10", N: 3, BaseN: 3,
			BaseDesc: maestroDesc("one maestro flow: 10x assertVisible"),
			Baseline: b.maestroFlow("find10", repeatStep(`- assertVisible: "Settings"`, 10)...),
			AgemDesc: "10x agemu find --text Settings",
			Agemu:    times(10, func() error { return b.agemu("find", "--text", "Settings") }),
			Daemon: times(10, func() error {
				return b.call("find", daemon.Params{Selector: &api.Selector{Text: "Settings"}})
			}),
		},
		{
			Name: "tap", BaseN: 3,
			BaseDesc: maestroDesc("maestro test (1x tapOn point)"),
			Baseline: b.maestroFlow("tap1", tapStep),
			AgemDesc: "agemu tap x y",
			Agemu:    func() error { return b.agemu("tap", fmt.Sprint(tx), fmt.Sprint(ty)) },
			Daemon:   func() error { return b.call("tap", daemon.Params{X: ip(tx), Y: ip(ty)}) },
		},
		{
			Name: "tap x10", N: 3, BaseN: 3,
			BaseDesc: maestroDesc("one maestro flow: 10x tapOn point"),
			Baseline: b.maestroFlow("tap10", repeatStep(tapStep, 10)...),
			AgemDesc: "10x agemu tap x y",
			Agemu:    times(10, func() error { return b.agemu("tap", fmt.Sprint(tx), fmt.Sprint(ty)) }),
			Daemon:   times(10, func() error { return b.call("tap", daemon.Params{X: ip(tx), Y: ip(ty)}) }),
		},
		{
			Name: "swipe (300 ms gesture)", N: 10, BaseN: 3,
			BaseDesc: maestroDesc("maestro test (1x swipe, 300 ms)"),
			Baseline: b.maestroFlow("swipe1", swipeStep),
			AgemDesc: "agemu swipe --ms 300",
			Agemu: func() error {
				return b.agemu("swipe", fmt.Sprint(w/2), fmt.Sprint(h*65/100), fmt.Sprint(w/2), fmt.Sprint(h/2), "--ms", "300")
			},
			Daemon: func() error {
				return b.call("swipe", daemon.Params{X: ip(w / 2), Y: ip(h * 65 / 100), X2: w / 2, Y2: h / 2, DurationMs: 300})
			},
		},
		{
			Name: "type text", BaseN: 3,
			Setup: func() error {
				if err := b.settings(); err != nil {
					return err
				}
				return b.agemu("tap", fmt.Sprint(sx), fmt.Sprint(sy))
			},
			BaseDesc: maestroDesc(`maestro test (inputText "hello")`),
			Baseline: b.maestroFlow("type1", `- inputText: "hello"`),
			AgemDesc: "agemu type hello",
			Agemu:    func() error { return b.agemu("type", "hello") },
			Daemon:   func() error { return b.call("type", daemon.Params{Text: "hello"}) },
		},
		{
			Name: "launch app", N: 10,
			Before:   b.terminateSettings,
			BaseDesc: "xcrun simctl launch <udid> com.apple.Preferences",
			Baseline: func() error { return b.simctl("launch", b.udid, settingsApp) },
			AgemDesc: "agemu launch com.apple.Preferences",
			Agemu:    func() error { return b.agemu("launch", settingsApp) },
			Daemon:   func() error { return b.call("launch", daemon.Params{App: settingsApp}) },
		},
		{
			Name: "launch app + first screen", N: 5,
			Before:   b.terminateSettings,
			AgemDesc: "agemu launch + agemu wait --id com.apple.settings.general",
			Agemu: func() error {
				if err := b.agemu("launch", settingsApp); err != nil {
					return err
				}
				return b.agemu("wait", "--id", "com.apple.settings.general", "--timeout-ms", "20000")
			},
		},
		{
			Name: "boot (device kept booted by agemu)", N: 10,
			BaseDesc: "xcrun simctl bootstatus <udid> -b",
			Baseline: func() error { return b.simctl("bootstatus", b.udid, "-b") },
			AgemDesc: "agemu boot (returns when agent-ready)",
			Agemu:    func() error { return b.agemu("boot", b.id) },
			Daemon:   func() error { return b.call("boot", daemon.Params{}) },
		},
		{
			Name: "boot (cold from shutdown)", N: 2,
			Before:   b.shutdown,
			BaseDesc: "xcrun simctl boot + simctl bootstatus -b",
			Baseline: func() error {
				if err := b.simctl("boot", b.udid); err != nil {
					return err
				}
				return b.simctl("bootstatus", b.udid, "-b")
			},
			AgemDesc: "agemu boot (until interactive: boot finished, framebuffer, HID; UI tree warming up)",
			Agemu:    func() error { return b.agemu("boot", b.id) },
		},
		{
			Name: "boot (cold) + first UI tree", N: 2,
			Before:   b.shutdown,
			AgemDesc: "agemu boot + agemu ui (until the UI tree is read)",
			Agemu: func() error {
				if err := b.agemu("boot", b.id); err != nil {
					return err
				}
				return b.agemu("ui")
			},
		},
		{
			// A profile switch to another device type is a cold boot of that type's simulator
			// (iOS has no runtime resolution change). Starts from this device booted and the
			// other one shut down.
			Name: "profile switch to iphone-se-3 (cold)", N: 2,
			Before: func() error {
				se, err := b.otherProfile()
				if err != nil {
					return err
				}
				if err := b.shutdownUDID(se); err != nil {
					return err
				}
				return Run(b.cfg.AgemuBin, "boot", b.id)
			},
			BaseDesc: "simctl shutdown <this> + simctl boot <se> + simctl bootstatus <se> -b",
			Baseline: func() error {
				if err := b.shutdown(); err != nil {
					return err
				}
				if err := b.simctl("boot", b.seUDID); err != nil {
					return err
				}
				return b.simctl("bootstatus", b.seUDID, "-b")
			},
			AgemDesc: "agemu profile iphone-se-3 (until interactive)",
			Agemu:    func() error { return b.agemu("profile", "iphone-se-3") },
		},
		{
			Name: "reset to clean state", N: 2,
			Before: func() error {
				// Dirty the device: an app running, navigated into a sub-screen.
				if err := b.agemu("boot", b.id); err != nil {
					return err
				}
				if err := b.settings(); err != nil {
					return err
				}
				return b.agemu("tap", "--id", "com.apple.settings.general")
			},
			BaseDesc: "simctl shutdown + erase + boot + bootstatus -b",
			Baseline: func() error {
				if err := b.shutdown(); err != nil {
					return err
				}
				if err := b.simctl("erase", b.udid); err != nil {
					return err
				}
				if err := b.simctl("boot", b.udid); err != nil {
					return err
				}
				return b.simctl("bootstatus", b.udid, "-b")
			},
			AgemDesc: "agemu reset (soft reset, no reboot)",
			Agemu:    func() error { return b.agemu("reset") },
		},
	}
	return ops, nil
}
