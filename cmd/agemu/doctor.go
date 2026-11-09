package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/api"
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/client"
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/daemon"
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/toolchain"
)

// `agemu doctor` checks everything agemu needs on this Mac, per platform, and prints the
// commands that fix what is missing. It runs in the CLI and does not start the daemon.

type checkStatus string

const (
	statusOK   checkStatus = "ok"
	statusWarn checkStatus = "warn" // works, but worth fixing
	statusFail checkStatus = "fail" // the platform cannot work until it is fixed
)

type check struct {
	Name   string      `json:"name"`
	Status checkStatus `json:"status"`
	Detail string      `json:"detail"`
	Fix    []string    `json:"fix,omitempty"` // shell commands or steps, in order
}

type doctorSection struct {
	Title  string  `json:"title"`
	Ready  *bool   `json:"ready,omitempty"` // platforms only
	Checks []check `json:"checks"`
}

type doctorReport struct {
	Sections []doctorSection `json:"sections"`
	Ready    map[string]bool `json:"ready"` // per platform
	Next     []string        `json:"next"`  // every fix, in order, without repeats
	MCP      string          `json:"mcp"`   // the command that adds agemu to Claude Code
}

func (s *doctorSection) add(c check) { s.Checks = append(s.Checks, c) }

func (s *doctorSection) failed() bool {
	for _, c := range s.Checks {
		if c.Status == statusFail {
			return true
		}
	}
	return false
}

// doctorCommand runs `agemu doctor [--platform ios|android] [--json]`.
func doctorCommand(args []string) error {
	c := parse(args)
	only := c.flags["platform"]
	if only != "" && only != "ios" && only != "android" {
		return fmt.Errorf("usage: doctor [--platform ios|android] [--json]")
	}
	rep := runDoctor(only)
	if c.flags["json"] == "true" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		_ = enc.Encode(rep)
	} else {
		printDoctor(rep)
	}
	ready := false
	for _, r := range rep.Ready {
		ready = ready || r
	}
	if !ready {
		return errDoctor
	}
	return nil
}

// errDoctor fails doctor without an extra message: the report already says what is missing.
var errDoctor = errors.New("")

// daemonInfo is the running daemon as doctor found it.
type daemonInfo struct {
	ping  daemon.Ping
	state daemonState
}

// lacks reports whether the running daemon was built without the platform (noios/noandroid).
func (d daemonInfo) lacks(plat api.Platform) bool {
	return d.state == daemonUp && len(d.ping.Platforms) > 0 && !slices.Contains(d.ping.Platforms, plat)
}

func runDoctor(only string) doctorReport {
	p, state := probeDaemon()
	d := daemonInfo{p, state}
	core := coreChecks(d)
	rep := doctorReport{Sections: []doctorSection{core}, Ready: map[string]bool{}}
	for _, plat := range []struct {
		key string
		run func(daemonInfo) doctorSection
	}{{"ios", iosChecks}, {"android", androidChecks}} {
		if only != "" && only != plat.key {
			continue
		}
		s := plat.run(d)
		if d.lacks(api.Platform(plat.key)) {
			s.add(check{Name: "daemon backend", Status: statusFail,
				Detail: fmt.Sprintf("the running agemud (%s) was built without %s", tilde(d.ping.Exe), s.Title),
				Fix:    []string{"use a build with both platforms (the npm and release builds have both), then: agemu daemon restart"}})
		}
		ready := !core.failed() && !s.failed()
		s.Ready = &ready
		rep.Ready[plat.key] = ready
		rep.Sections = append(rep.Sections, s)
	}
	seen := map[string]bool{}
	for _, s := range rep.Sections {
		for _, c := range s.Checks {
			if len(c.Fix) == 0 || c.Status == statusOK {
				continue
			}
			step := strings.Join(c.Fix, "\n")
			if !seen[step] {
				seen[step] = true
				rep.Next = append(rep.Next, step)
			}
		}
	}
	rep.MCP = "claude mcp add agemu -- " + agemuCmd() + " mcp"
	return rep
}

// selfPath is this agemu binary, symlinks resolved.
func selfPath() string {
	exe, err := os.Executable()
	if err != nil {
		return "agemu"
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		return r
	}
	return exe
}

// selfOnPath returns `agemu` on PATH when that is this binary.
func selfOnPath() (string, bool) {
	p, err := exec.LookPath("agemu")
	if err != nil {
		return "", false
	}
	r, err := filepath.EvalSymlinks(p)
	return p, err == nil && r == selfPath()
}

// agemuCmd is how to run this agemu in a printed command: plain "agemu" when that is this binary
// on PATH (it keeps working after an upgrade moves the files), else its absolute path.
func agemuCmd() string {
	if _, ok := selfOnPath(); ok {
		return "agemu"
	}
	return shellQuote(selfPath())
}

// mcpBinary is the absolute path an MCP config should run, for clients that do not see the
// shell's PATH: the PATH entry (a symlink npm keeps current) when there is one.
func mcpBinary() string {
	if p, ok := selfOnPath(); ok {
		if abs, err := filepath.Abs(p); err == nil {
			return abs
		}
	}
	return selfPath()
}

func coreChecks(d daemonInfo) doctorSection {
	s := doctorSection{Title: "agemu"}

	host := check{Name: "Apple Silicon Mac", Status: statusOK}
	ver, _ := output(5*time.Second, "sw_vers", "-productVersion")
	host.Detail = strings.TrimSpace("macOS " + ver + " (" + runtime.GOARCH + ")")
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		host.Status, host.Detail = statusFail, fmt.Sprintf("%s/%s: agemu runs on Apple Silicon Macs only", runtime.GOOS, runtime.GOARCH)
	}
	s.add(host)

	if p, err := client.DaemonPath(); err != nil {
		s.add(check{Name: "agemud", Status: statusFail, Detail: err.Error(), Fix: []string{"make build    # in the agemu checkout; or: make install"}})
	} else {
		s.add(check{Name: "agemud", Status: statusOK, Detail: tilde(p)})
	}

	if p := d.ping; d.state == daemonAbsent {
		s.add(check{Name: "daemon", Status: statusOK, Detail: "not running (the first command starts it)"})
	} else if d.state == daemonHung {
		s.add(check{Name: "daemon", Status: statusFail, Detail: hungMessage(), Fix: []string{agemuCmd() + " daemon restart    # devices keep running"}})
	} else if r := staleReason(p); r != "" {
		fix := agemuCmd() + " daemon restart    # devices keep running"
		if len(p.Preparing) > 0 {
			fix = agemuCmd() + " daemon restart    # once the preparation is done; now it would cancel it"
			r += "; " + preparingText(p)
		}
		s.add(check{Name: "daemon", Status: statusWarn, Detail: fmt.Sprintf("pid %d is out of date: %s", p.PID, r), Fix: []string{fix}})
	} else if versionOlder(daemon.Version, p.Version) {
		s.add(check{Name: "daemon", Status: statusWarn, Detail: fmt.Sprintf("pid %d runs %s, newer than this agemu (%s)", p.PID, p.Version, daemon.Version),
			Fix: []string{"npm install -g agentic-emulator@latest    # or update the agemu you run"}})
	} else {
		detail := fmt.Sprintf("running, pid %d, version %s", p.PID, p.Version)
		if len(p.Preparing) > 0 {
			detail += "; " + preparingText(p)
		}
		s.add(check{Name: "daemon", Status: statusOK, Detail: detail})
	}

	self := selfPath()
	onPath := check{Name: "on PATH", Status: statusOK}
	if p, err := exec.LookPath("agemu"); err != nil {
		dir := filepath.Dir(self)
		onPath.Status = statusWarn
		onPath.Detail = "agemu is not on PATH; agents and MCP configs then need its full path"
		switch _, mk := os.Stat(filepath.Join(dir, "..", "Makefile")); {
		case strings.HasSuffix(dir, "/.local/bin"):
			onPath.Fix = []string{`echo 'export PATH="$HOME/.local/bin:$PATH"' >> ~/.zshrc && exec zsh`}
		case mk == nil && filepath.Base(dir) == "bin":
			onPath.Fix = []string{"make install    # in " + tilde(filepath.Dir(dir)) + ": copies agemu to ~/.local/bin"}
		default:
			onPath.Fix = []string{`echo 'export PATH="` + dir + `:$PATH"' >> ~/.zshrc && exec zsh`}
		}
	} else if r, _ := filepath.EvalSymlinks(p); r != self {
		onPath.Status = statusWarn
		onPath.Detail = fmt.Sprintf("`agemu` on PATH is %s, not this one (%s)", tilde(p), tilde(self))
	} else {
		onPath.Detail = tilde(p)
	}
	s.add(onPath)
	return s
}

func iosChecks(d daemonInfo) doctorSection {
	s := doctorSection{Title: "iOS"}
	installXcode := []string{
		"Install Xcode from the App Store, open it once, then:",
		"sudo xcode-select -s /Applications/Xcode.app/Contents/Developer",
	}
	dev, err := output(10*time.Second, "xcode-select", "-p")
	switch {
	case err != nil:
		s.add(check{Name: "Xcode", Status: statusFail, Detail: "no developer directory selected", Fix: installXcode})
		return s
	case strings.Contains(dev, "CommandLineTools"):
		fix := installXcode
		if _, err := os.Stat("/Applications/Xcode.app"); err == nil {
			fix = installXcode[1:]
		}
		s.add(check{Name: "Xcode", Status: statusFail, Detail: "only the Command Line Tools are selected (" + dev + "); simulators need Xcode", Fix: fix})
		return s
	}
	xv, _ := output(15*time.Second, "xcodebuild", "-version")
	xv, _, _ = strings.Cut(xv, "\n")
	s.add(check{Name: "Xcode", Status: statusOK, Detail: strings.TrimSpace(xv + " (" + tilde(dev) + ")")})

	raw, err := output(30*time.Second, "xcrun", "simctl", "list", "runtimes", "-j")
	if err != nil {
		s.add(check{Name: "iOS Simulator runtime", Status: statusFail, Detail: "simctl failed: " + firstLine(err.Error()),
			Fix: []string{"sudo xcodebuild -license accept && sudo xcodebuild -runFirstLaunch"}})
	} else if rt := newestIOSRuntime(raw); rt == "" {
		s.add(check{Name: "iOS Simulator runtime", Status: statusFail, Detail: "no iOS runtime installed",
			Fix: []string{"xcodebuild -downloadPlatform iOS    # about 16 GB of disk; or Xcode › Settings › Components"}})
	} else if id := os.Getenv("AGEMU_IOS_RUNTIME"); id != "" && !iosRuntimeAvailable(raw, id) {
		s.add(check{Name: "iOS Simulator runtime", Status: statusFail, Detail: "AGEMU_IOS_RUNTIME=" + id + " is not an available runtime (" + rt + " is)",
			Fix: []string{"unset AGEMU_IOS_RUNTIME    # or an identifier from: xcrun simctl list runtimes"}})
	} else {
		detail := rt
		if id != "" {
			detail = id + " (AGEMU_IOS_RUNTIME)"
		}
		s.add(check{Name: "iOS Simulator runtime", Status: statusOK, Detail: detail})
	}

	if p := os.Getenv("AGEMU_IOS_HELPER"); p != "" {
		if !isExecutable(p) {
			s.add(check{Name: "agemu-ios-helper", Status: statusFail, Detail: "AGEMU_IOS_HELPER=" + p + " is not an executable file", Fix: []string{"unset AGEMU_IOS_HELPER"}})
		} else {
			s.add(check{Name: "agemu-ios-helper", Status: statusOK, Detail: tilde(p) + " (AGEMU_IOS_HELPER)"})
		}
	} else if d, err := client.DaemonPath(); err == nil {
		if r, err := filepath.EvalSymlinks(d); err == nil {
			d = r
		}
		h := filepath.Join(filepath.Dir(d), "agemu-ios-helper")
		if !isExecutable(h) {
			s.add(check{Name: "agemu-ios-helper", Status: statusFail, Detail: "missing or not executable next to agemud (" + tilde(filepath.Dir(d)) + ")",
				Fix: []string{"make build    # in the agemu checkout (needs Xcode's Swift); or: make install"}})
		} else {
			s.add(check{Name: "agemu-ios-helper", Status: statusOK, Detail: tilde(h)})
		}
	}
	return s
}

// iosRuntimeAvailable reports whether `simctl list runtimes -j` has the runtime with this
// identifier, available.
func iosRuntimeAvailable(raw, id string) bool {
	for _, r := range parseRuntimes(raw) {
		if r.Identifier == id {
			return r.IsAvailable
		}
	}
	return false
}

type simRuntime struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Platform    string `json:"platform"`
	Identifier  string `json:"identifier"`
	IsAvailable bool   `json:"isAvailable"`
}

func parseRuntimes(raw string) []simRuntime {
	var v struct {
		Runtimes []simRuntime `json:"runtimes"`
	}
	_ = json.Unmarshal([]byte(raw), &v)
	return v.Runtimes
}

// newestIOSRuntime is the newest available iOS runtime in `simctl list runtimes -j`, as
// "iOS 26.5", or "".
func newestIOSRuntime(raw string) string {
	best, bestVer := "", ""
	for _, r := range parseRuntimes(raw) {
		if !r.IsAvailable || (r.Platform != "iOS" && !strings.Contains(r.Identifier, ".iOS-")) {
			continue
		}
		if best == "" || versionBefore(bestVer, r.Version) {
			best, bestVer = r.Name, r.Version
		}
	}
	return best
}

func versionBefore(a, b string) bool {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		var x, y int
		if i < len(as) {
			x, _ = atoi(as[i])
		}
		if i < len(bs) {
			y, _ = atoi(bs[i])
		}
		if x != y {
			return x < y
		}
	}
	return false
}

func androidChecks(d daemonInfo) doctorSection {
	s := doctorSection{Title: "Android"}
	sdk, source := toolchain.AndroidSDKSource()
	where := tilde(sdk)
	if source != "default" {
		where += " (" + source + ")"
	}
	if st, err := os.Stat(sdk); err != nil || !st.IsDir() {
		fix := []string{
			"brew install --cask android-commandlinetools temurin    # agemu finds this SDK by itself",
		}
		if source[0] == '$' {
			fix = append([]string{"Point " + source[1:] + " at an Android SDK, or unset it and:"}, fix...)
		}
		s.add(check{Name: "Android SDK", Status: statusFail, Detail: "no SDK at " + where, Fix: fix})
		return s
	}
	s.add(check{Name: "Android SDK", Status: statusOK, Detail: where})
	if ds := d.ping.AndroidSDK; d.state == daemonUp && ds != "" && ds != sdk {
		// The daemon reads ANDROID_HOME from the process that started it, maybe an MCP client.
		c := check{Name: "daemon's SDK", Status: statusWarn, Detail: "the running daemon uses " + tilde(ds) + ", from the environment it started in",
			Fix: []string{agemuCmd() + " daemon restart    # from this shell, so the daemon uses " + tilde(sdk)}}
		if st, err := os.Stat(ds); err != nil || !st.IsDir() {
			c.Status, c.Detail = statusFail, "the running daemon uses "+tilde(ds)+", which is not an SDK"
		}
		s.add(c)
	}

	java := toolchain.JavaHome()
	switch major, err := javaMajor(java); {
	case java == "":
		s.add(check{Name: "Java", Status: statusFail, Detail: "no Java runtime; the SDK's avdmanager and sdkmanager need Java 17+",
			Fix: []string{"brew install --cask temurin"}})
	case err != nil:
		s.add(check{Name: "Java", Status: statusFail, Detail: "the Java runtime at " + tilde(java) + " does not run: " + firstLine(err.Error()),
			Fix: []string{"brew install --cask temurin    # then unset JAVA_HOME, or point it at the new JDK"}})
	case major < 17:
		s.add(check{Name: "Java", Status: statusFail, Detail: fmt.Sprintf("Java %d at %s; avdmanager needs 17+", major, tilde(java)),
			Fix: []string{"brew install --cask temurin    # then unset JAVA_HOME, or point it at the new JDK"}})
	default:
		s.add(check{Name: "Java", Status: statusOK, Detail: fmt.Sprintf("Java %d, %s", major, tilde(java))})
	}

	sdkmanager := filepath.Join(sdk, "cmdline-tools", "latest", "bin", "sdkmanager")
	avdmanager := filepath.Join(sdk, "cmdline-tools", "latest", "bin", "avdmanager")
	if !isExecutable(avdmanager) {
		fix := []string{"Android Studio › Settings › Languages & Frameworks › Android SDK › SDK Tools › Android SDK Command-line Tools (latest)"}
		if sdk == toolchain.HomebrewAndroidSDK {
			fix = []string{"brew reinstall --cask android-commandlinetools"}
		}
		s.add(check{Name: "command-line tools", Status: statusFail, Detail: "avdmanager missing (" + tilde(filepath.Dir(avdmanager)) + ")", Fix: fix})
		sdkmanager = "sdkmanager"
	} else {
		s.add(check{Name: "command-line tools", Status: statusOK, Detail: tilde(filepath.Dir(avdmanager))})
	}

	image := toolchain.AndroidImage()
	var missing []string // sdkmanager packages
	for _, p := range []struct{ name, pkg, file string }{
		{"emulator", "emulator", filepath.Join(sdk, "emulator", "emulator")},
		{"platform-tools (adb)", "platform-tools", filepath.Join(sdk, "platform-tools", "adb")},
		{"system image", image, filepath.Join(toolchain.AndroidImageDir(image), "system.img")},
	} {
		c := check{Name: p.name, Status: statusOK, Detail: tilde(filepath.Dir(p.file))}
		if p.pkg == image {
			c.Detail = image
			if os.Getenv("AGEMU_ANDROID_IMAGE") != "" {
				c.Detail += " (AGEMU_ANDROID_IMAGE)"
			}
		}
		present := isExecutable(p.file)
		if p.pkg == image {
			_, err := os.Stat(p.file)
			present = err == nil
		}
		if !present {
			c.Status = statusFail
			c.Detail = "not installed: " + p.pkg
			missing = append(missing, p.pkg)
		}
		s.add(c)
	}
	if len(missing) > 0 {
		sm := shellPath(sdkmanager)
		pkgs := make([]string, len(missing))
		for i, m := range missing {
			pkgs[i] = shellQuote(m)
		}
		fix := []string{
			"yes | " + sm + " --licenses > /dev/null",
			sm + " --install " + strings.Join(pkgs, " ") + "    # the image takes about 8 GB of disk",
		}
		if java != "" && os.Getenv("JAVA_HOME") == "" && java == toolchain.AndroidStudioJBR {
			fix = append([]string{"export JAVA_HOME=" + shellQuote(java)}, fix...)
		}
		// One fix for all missing packages, attached to the first of them.
		for i := range s.Checks {
			if s.Checks[i].Status == statusFail && s.Checks[i].Fix == nil && strings.HasPrefix(s.Checks[i].Detail, "not installed") {
				s.Checks[i].Fix = fix
				break
			}
		}
	}
	return s
}

func printDoctor(rep doctorReport) {
	color := colorOutput()
	paint := func(code, s string) string {
		if !color {
			return s
		}
		return "\x1b[" + code + "m" + s + "\x1b[0m"
	}
	mark := map[checkStatus]string{statusOK: paint("32", "✓"), statusWarn: paint("33", "!"), statusFail: paint("31", "✗")}
	width := 0
	for _, s := range rep.Sections {
		for _, c := range s.Checks {
			width = max(width, len(c.Name))
		}
	}
	for i, s := range rep.Sections {
		if i > 0 {
			fmt.Println()
		}
		title := paint("1", s.Title)
		if s.Ready != nil {
			if *s.Ready {
				title += "  " + paint("32", "ready")
			} else {
				title += "  " + paint("31", "not ready")
			}
		}
		fmt.Println(title)
		for _, c := range s.Checks {
			fmt.Printf("  %s %-*s  %s\n", mark[c.Status], width, c.Name, c.Detail)
		}
	}
	if len(rep.Next) > 0 {
		fmt.Println()
		fmt.Println(paint("1", "To fix"))
		for i, step := range rep.Next {
			for j, line := range strings.Split(step, "\n") {
				prefix := "     "
				if j == 0 {
					prefix = fmt.Sprintf("  %d. ", i+1)
				}
				fmt.Println(prefix + line)
			}
		}
		fmt.Println("  Then run `agemu doctor` again.")
	}
	ready := false
	for _, r := range rep.Ready {
		ready = ready || r
	}
	if ready {
		fmt.Println()
		fmt.Println(paint("1", "Connect an agent"))
		fmt.Println("  Claude Code:      " + rep.MCP)
		fmt.Println("  Any MCP client:   command " + shellQuote(mcpBinary()) + ", args [\"mcp\"] (stdio)")
		fmt.Println()
		fmt.Println(paint("1", "First device"))
		for _, plat := range []string{"ios", "android"} {
			if rep.Ready[plat] {
				name := map[string]string{"ios": "iphone", "android": "pixel"}[plat]
				fmt.Printf("  %s create --platform %s --name %s && %s boot agemu-%s\n", agemuCmd(), plat, name, agemuCmd(), name)
			}
		}
	}
}

func colorOutput() bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	st, err := os.Stdout.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// output runs a command with a timeout and returns its trimmed stdout.
func output(timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("%w: %s", err, msg)
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// isExecutable reports whether p is a regular file someone may execute.
func isExecutable(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular() && st.Mode().Perm()&0o111 != 0
}

// javaMajor runs the Java runtime at javaHome and returns its major version (8 for "1.8.0").
func javaMajor(javaHome string) (int, error) {
	if javaHome == "" {
		return 0, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, filepath.Join(javaHome, "bin", "java"), "-version").CombinedOutput()
	if err != nil {
		return 0, err
	}
	return parseJavaVersion(string(out))
}

// parseJavaVersion reads the major version from `java -version` output.
func parseJavaVersion(out string) (int, error) {
	m := javaVersionRe.FindStringSubmatch(out)
	if m == nil {
		return 0, fmt.Errorf("unrecognised java -version output: %s", firstLine(out))
	}
	major, _ := strconv.Atoi(m[1])
	if major == 1 && m[2] != "" { // 1.8.0_402 is Java 8
		major, _ = strconv.Atoi(m[2])
	}
	return major, nil
}

var javaVersionRe = regexp.MustCompile(`version "(\d+)(?:\.(\d+))?`)

func firstLine(s string) string { l, _, _ := strings.Cut(s, "\n"); return l }

// tilde shortens a path under the home directory for display.
func tilde(p string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" && strings.HasPrefix(p, home+"/") {
		return "~" + p[len(home):]
	}
	return p
}

// shellPath is a path for a printed command: ~/... when that needs no quoting.
func shellPath(p string) string {
	if t := tilde(p); t != p && shellQuote(t[1:]) == t[1:] {
		return t
	}
	return shellQuote(p)
}

// shellQuote quotes s for a POSIX shell when it needs it.
func shellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_./=:@+,") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
