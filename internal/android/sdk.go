package android

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/api"
)

// Defaults, overridable with environment variables (read by the daemon process).
const (
	defaultSystemImage = "system-images;android-37.1;google_apis_playstore_ps16k;arm64-v8a"
	defaultAVDPrefix   = "agemu-"
	defaultRAMMB       = 2048
	baselineSnapshot   = "agemu-baseline"
)

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func envInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil && v > 0 {
		return v
	}
	return def
}

func sdkRoot() string {
	for _, k := range []string{"ANDROID_HOME", "ANDROID_SDK_ROOT"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "Android", "sdk")
}

func sdkTool(parts ...string) string { return filepath.Join(append([]string{sdkRoot()}, parts...)...) }

func avdHome() string {
	if v := os.Getenv("ANDROID_AVD_HOME"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".android", "avd")
}

// discoveryDir is where the emulator writes pid_<pid>.ini for every running instance.
func discoveryDir() string {
	if v := os.Getenv("ANDROID_EMULATOR_DISCOVERY_DIR"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "Caches", "TemporaryItems", "avd", "running")
}

// managed reports whether agemu may touch this AVD. By default only AVDs whose name starts
// with "agemu-" are managed, so agemu never drives emulators other tools or people use.
// AGEMU_ANDROID_AVDS=* manages every AVD; any other value is a comma-separated list of
// name prefixes.
func managed(name string) bool {
	spec := env("AGEMU_ANDROID_AVDS", defaultAVDPrefix)
	if spec == "*" {
		return true
	}
	for _, p := range strings.Split(spec, ",") {
		if p = strings.TrimSpace(p); p != "" && strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

func readINI(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	m := map[string]string{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if !ok {
			continue
		}
		m[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return m, sc.Err()
}

type avdInfo struct {
	Name   string
	Dir    string
	Config map[string]string
}

func (a avdInfo) intCfg(k string) int { v, _ := strconv.Atoi(a.Config[k]); return v }

func (a avdInfo) hasBaseline() bool {
	_, err := os.Stat(filepath.Join(a.Dir, "snapshots", baselineSnapshot, "snapshot.pb"))
	return err == nil
}

func (a avdInfo) osVersion() string {
	t := a.Config["target"] // e.g. android-37.1
	if t == "" {
		return ""
	}
	return "Android API " + strings.TrimPrefix(t, "android-")
}

func listAVDs() ([]avdInfo, error) {
	ents, err := os.ReadDir(avdHome())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []avdInfo
	for _, e := range ents {
		name, ok := strings.CutSuffix(e.Name(), ".ini")
		if !ok || e.IsDir() || !managed(name) {
			continue
		}
		a, err := loadAVD(name)
		if err != nil {
			continue
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func loadAVD(name string) (avdInfo, error) {
	top, err := readINI(filepath.Join(avdHome(), name+".ini"))
	if err != nil {
		return avdInfo{}, err
	}
	dir := top["path"]
	if dir == "" {
		dir = filepath.Join(avdHome(), name+".avd")
	}
	cfg, err := readINI(filepath.Join(dir, "config.ini"))
	if err != nil {
		return avdInfo{}, err
	}
	return avdInfo{Name: name, Dir: dir, Config: cfg}, nil
}

// createAVD makes a new AVD from the (already installed) system image with a screen of
// the given profile and patches its config for headless agent use.
func createAVD(ctx context.Context, name string, prof api.Profile) (avdInfo, error) {
	img := env("AGEMU_ANDROID_IMAGE", defaultSystemImage)
	imgDir := sdkTool(strings.Split(img, ";")...)
	if _, err := os.Stat(filepath.Join(imgDir, "system.img")); err != nil {
		return avdInfo{}, fmt.Errorf("system image %s is not installed (%s missing); agemu does not download images", img, imgDir)
	}
	if _, err := os.Stat(filepath.Join(avdHome(), name+".ini")); err == nil {
		return avdInfo{}, fmt.Errorf("AVD %s already exists", name)
	}
	avdmanager := sdkTool("cmdline-tools", "latest", "bin", "avdmanager")
	cmd := exec.CommandContext(ctx, avdmanager, "create", "avd", "-n", name, "-k", img)
	cmd.Stdin = strings.NewReader("no\n") // "Do you wish to create a custom hardware profile?"
	if out, err := cmd.CombinedOutput(); err != nil {
		return avdInfo{}, fmt.Errorf("avdmanager create avd: %v: %s", err, lastLines(string(out), 5))
	}
	a, err := loadAVD(name)
	if err != nil {
		return avdInfo{}, err
	}
	// The SDK leaves an empty sdcard image behind for hw.sdCard=yes; we disable the SD card.
	_ = os.Remove(filepath.Join(a.Dir, "sdcard.img"))
	set := map[string]string{
		"AvdId": name, "avd.ini.displayname": name, "avd.id": name, "avd.name": name,
		"hw.lcd.width": strconv.Itoa(prof.Width), "hw.lcd.height": strconv.Itoa(prof.Height),
		"hw.lcd.density": strconv.Itoa(prof.Density),
		"hw.ramSize":     strconv.Itoa(envInt("AGEMU_ANDROID_RAM_MB", defaultRAMMB)),
		"vm.heapSize":    "256",
		// A phone: soft navigation, no hardware keyboard / dpad / trackball.
		"hw.keyboard": "no", "hw.mainKeys": "no", "hw.dPad": "no", "hw.trackBall": "no",
		"hw.gpu.enabled": "yes", "hw.gpu.mode": "auto",
		"hw.sdCard": "no", "sdcard.size": "0",
		"hw.audioInput": "no", "hw.audioOutput": "no",
		"showDeviceFrame":         "no",
		"disk.dataPartition.size": env("AGEMU_ANDROID_DATA_SIZE", "4G"),
		// agemu keeps exactly one snapshot (the baseline); no quickboot auto snapshots.
		"firstboot.bootFromDownloadableSnapshot": "no", "firstboot.bootFromLocalSnapshot": "no",
		"firstboot.saveToLocalSnapshot": "no", "fastboot.forceFastBoot": "no",
		"agemu.profile": prof.Name,
	}
	if err := patchINI(filepath.Join(a.Dir, "config.ini"), set, []string{"disk.dataPartition.path"}); err != nil {
		return avdInfo{}, err
	}
	return loadAVD(name)
}

// patchINI rewrites key=value pairs in place, keeping order; drop lists keys to delete.
func patchINI(path string, set map[string]string, drop []string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	dropped := map[string]bool{}
	for _, d := range drop {
		dropped[d] = true
	}
	seen := map[string]bool{}
	var out []string
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		k, _, ok := strings.Cut(line, "=")
		k = strings.TrimSpace(k)
		if ok && dropped[k] {
			continue
		}
		if v, has := set[k]; ok && has {
			line = k + "=" + v
			seen[k] = true
		}
		out = append(out, line)
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		if !seen[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, k+"="+set[k])
	}
	return os.WriteFile(path, []byte(strings.Join(out, "\n")+"\n"), 0o644)
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}

// runningEmu is one entry of the emulator discovery directory.
type runningEmu struct {
	PID       int
	AVD       string
	Console   int // console port; adb serial is emulator-<Console>
	ADBPort   int
	GRPCPort  int
	GRPCToken string
}

func (r runningEmu) serial() string { return fmt.Sprintf("emulator-%d", r.Console) }

func pidAlive(pid int) bool {
	return pid > 0 && syscall.Kill(pid, 0) == nil
}

// runningEmulators lists live emulator instances (all of them, managed or not; callers filter).
func runningEmulators() []runningEmu {
	files, _ := filepath.Glob(filepath.Join(discoveryDir(), "pid_*.ini"))
	var out []runningEmu
	for _, f := range files {
		pid, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(filepath.Base(f), "pid_"), ".ini"))
		if err != nil || !pidAlive(pid) {
			continue
		}
		m, err := readINI(f)
		if err != nil {
			continue
		}
		r := runningEmu{PID: pid, AVD: m["avd.id"], GRPCToken: m["grpc.token"]}
		r.Console, _ = strconv.Atoi(m["port.serial"])
		r.ADBPort, _ = strconv.Atoi(m["port.adb"])
		r.GRPCPort, _ = strconv.Atoi(m["grpc.port"])
		out = append(out, r)
	}
	return out
}

func findRunning(avd string) (runningEmu, bool) {
	for _, r := range runningEmulators() {
		if r.AVD == avd {
			return r, true
		}
	}
	return runningEmu{}, false
}
