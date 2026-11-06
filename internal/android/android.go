// Package android is the Android backend of agemu.
//
// Devices are stock Android emulators (SDK QEMU + HVF) running the stock SDK system image.
// agemu only replaces the control plane:
//
//   - emulator gRPC (EmulatorController / SnapshotService): framebuffer screenshots,
//     touch, keys, text, snapshots, shutdown;
//   - an on-device agent (android-agent/, app_process as the shell user) holding one
//     UiAutomation connection: UI tree, launch/stop, display profile overrides;
//   - the adb server's smart-socket protocol, spoken directly (no adb process per call).
//
// Device ids are "android:<avd-name>". Only AVDs named agemu-* are managed by default
// (see managed in sdk.go), so agemu never touches other emulators on the machine.
package android

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/android/emupb"
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/api"
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/profiles"
)

type Backend struct {
	mu   sync.Mutex
	devs map[string]*device
}

// New is cheap: it spawns nothing and connects to nothing until a device is used.
func New() *Backend { return &Backend{devs: map[string]*device{}} }

func (b *Backend) Platform() api.Platform { return api.Android }

type device struct {
	name string
	life sync.Mutex // serializes boot / shutdown / reset / profile

	mu      sync.Mutex // guards the fields below
	booting bool
	run     runningEmu
	emu     *emuClient
	agent   *agentClient
	disp    displayInfo // last known logical display
	dispAt  time.Time
	proc    *exec.Cmd // emulator we launched (reaped by a goroutine)
	retook  bool      // baseline already retaken by this daemon
	agentOK bool      // agent version checked on the current connection

	// lastInput is when agemu last sent input (unix ns); screenshots taken right after an
	// input wait for the frame that shows its effect (see frameStream.get).
	lastInput atomic.Int64

	lastLoad time.Time // when the last in-place snapshot load finished (see loadBaseline)
}

// loadSpacing is the minimum time between the end of an in-place snapshot load and the start of
// the next one. After a load, the emulator restores GPU textures in a background thread
// (gfxstream GLBackgroundLoader). A load started while it runs deadlocks emulator 37.1: the
// LoadSnapshot handler waits in snapshot::Loader::~Loader for that thread, which waits on the
// texture file's lock (seen with `sample` on a hung emulator: a reset started 0.1 s after the
// previous one; resets 2.2 s apart did not hang).
const loadSpacing = 3 * time.Second

// loadBaseline restores the baseline in place, keeping loads loadSpacing apart, with a timeout:
// a hung emulator surfaces as an error instead of a reset that never returns.
func (d *device) loadBaseline(ctx context.Context, e *emuClient) error {
	d.mu.Lock()
	wait := loadSpacing - time.Since(d.lastLoad)
	d.mu.Unlock()
	if wait > 0 && wait <= loadSpacing {
		logf("%s: waiting %v before the next snapshot load (emulator texture loader)", d.name, wait.Round(time.Millisecond))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
	lctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	err := e.loadSnapshot(lctx, baselineSnapshot)
	e.stream.invalidate()
	d.mu.Lock()
	d.lastLoad = time.Now()
	d.mu.Unlock()
	if err != nil && lctx.Err() != nil && ctx.Err() == nil {
		return fmt.Errorf("the emulator did not finish loading %s in 2 minutes (it may be hung; `agemu shutdown` twice powers it off): %w", baselineSnapshot, err)
	}
	return err
}

func (d *device) markInput() { d.lastInput.Store(time.Now().UnixNano()) }

// redirector owns the console port redirection to the agent's guest TCP port for one
// emulator process (agentClient.tcp). It has its own lock: it is called with the agent
// client's lock held, and device.mu is taken around agent calls elsewhere.
type redirector struct {
	name    string
	console int
	mu      sync.Mutex
	port    int
	failed  time.Time // last failure; retried after a second (the console is busy during a snapshot load)
}

func (r *redirector) get(ctx context.Context, refresh bool) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.port != 0 && !refresh {
		return r.port, nil
	}
	if !r.failed.IsZero() && time.Since(r.failed) < time.Second {
		return 0, errors.New("no console redirection")
	}
	c, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	hp, err := ensureRedir(c, r.console, agentGuestPort, r.port)
	if err != nil {
		if r.failed.IsZero() {
			logf("%s: agent TCP redirection unavailable for now (%v); using adb", r.name, err)
		}
		r.failed = time.Now()
		return 0, err
	}
	r.port, r.failed = hp, time.Time{}
	return hp, nil
}

// drop deletes the redirection. The emulator saves its user-mode network (slirp) state in
// snapshots, and a snapshot saved with the redirection's listening socket cannot be loaded
// again ("Failed to load slirp-sbuf ... error while loading state ... of device 'slirp'",
// -EINVAL). The next get re-creates it.
func (r *redirector) drop(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, _, err := consoleRun(c, r.console, "redir list")
	if err != nil {
		logf("%s: redir list: %v", r.name, err)
		return
	}
	var cmds []string
	for _, line := range strings.Split(out[0], "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == "ipv4" {
			cmds = append(cmds, "redir del "+f[1])
		}
	}
	if len(cmds) > 0 {
		if _, errs, err := consoleRun(c, r.console, cmds...); err != nil {
			logf("%s: redir del: %v %v", r.name, err, errs)
		}
	}
	r.port = 0
}

// newAgent returns an agent client for a running emulator: TCP via the console redirection
// (AGEMU_ANDROID_AGENT_TCP=0 disables it), adb otherwise.
func newAgent(name string, r runningEmu) *agentClient {
	a := &agentClient{serial: r.serial()}
	if env("AGEMU_ANDROID_AGENT_TCP", "1") != "0" && r.Console > 0 {
		if tok := agentToken(name); tok != "" {
			a.rd = &redirector{name: name, console: r.Console}
			a.tcp, a.token = a.rd.get, tok
		}
	}
	return a
}

func logf(format string, args ...any) { log.Printf("android: "+format, args...) }

// trace enables per-operation debug lines in the daemon log (AGEMU_ANDROID_TRACE=1).
var trace = os.Getenv("AGEMU_ANDROID_TRACE") == "1"

func avdName(id string) (string, error) {
	name, ok := strings.CutPrefix(id, string(api.Android)+":")
	if !ok || name == "" {
		return "", fmt.Errorf("not an android device id: %q", id)
	}
	if !managed(name) {
		return "", fmt.Errorf("AVD %q is not managed by agemu (only %s* by default; set AGEMU_ANDROID_AVDS)", name, defaultAVDPrefix)
	}
	return name, nil
}

func (b *Backend) dev(name string) *device {
	b.mu.Lock()
	defer b.mu.Unlock()
	d, ok := b.devs[name]
	if !ok {
		d = &device{name: name}
		b.devs[name] = d
	}
	return d
}

func (b *Backend) devFor(id string) (*device, error) {
	name, err := avdName(id)
	if err != nil {
		return nil, err
	}
	return b.dev(name), nil
}

// ------------------------------------------------------------------ connections

// conn returns live clients for a running device, (re)connecting when the emulator changed.
// A device in standby is reported as shut down.
func (d *device) conn(ctx context.Context) (*emuClient, *agentClient, error) {
	e, a, err := d.connRaw(ctx)
	if err != nil {
		return nil, nil, err
	}
	d.mu.Lock()
	checked := d.agentOK
	d.mu.Unlock()
	if !checked {
		c, cancel := context.WithTimeout(ctx, 2*time.Second)
		st, verr := e.vmState(c)
		cancel()
		if verr == nil && st == emupb.VmRunState_PAUSED {
			return nil, nil, fmt.Errorf("device android:%s is shut down (in standby); agemu boot android:%s", d.name, d.name)
		}
		// First contact with this emulator from this daemon: make sure our agent version runs.
		if _, err := ensureAgent(ctx, a, logf); err != nil {
			return nil, nil, err
		}
		d.mu.Lock()
		d.agentOK = true
		d.mu.Unlock()
	}
	return e, a, nil
}

// connRaw returns clients without checking the VM state or the agent.
func (d *device) connRaw(ctx context.Context) (*emuClient, *agentClient, error) {
	d.mu.Lock()
	if d.emu != nil && pidAlive(d.run.PID) && !isZombie(d) {
		e, a := d.emu, d.agent
		d.mu.Unlock()
		return e, a, nil
	}
	d.mu.Unlock()
	r, ok := findRunning(d.name)
	if !ok {
		d.drop()
		return nil, nil, fmt.Errorf("device android:%s is not running (agemu boot android:%s)", d.name, d.name)
	}
	e, err := dialEmu(r)
	if err != nil {
		return nil, nil, err
	}
	a := newAgent(d.name, r)
	d.mu.Lock()
	if d.emu != nil {
		d.emu.Close()
	}
	if d.agent != nil {
		d.agent.Close()
	}
	d.run, d.emu, d.agent, d.dispAt, d.agentOK = r, e, a, time.Time{}, false
	d.mu.Unlock()
	if av, err := loadAVD(d.name); err == nil {
		e.enableShm(filepath.Join(logDir(), d.name+".fb"), av.intCfg("hw.lcd.width"), av.intCfg("hw.lcd.height"))
	}
	return e, a, nil
}

// ready returns clients and makes sure the on-device agent runs (starting it if needed).
func (d *device) ready(ctx context.Context) (*emuClient, *agentClient, error) {
	e, a, err := d.conn(ctx)
	if err != nil {
		return nil, nil, err
	}
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	p, perr := a.ping(pctx)
	cancel()
	if perr == nil && p.Version == agentVersion {
		return e, a, nil
	}
	if _, err := ensureAgent(ctx, a, logf); err != nil {
		return nil, nil, err
	}
	return e, a, nil
}

// call sends a request to the on-device agent. If the agent is gone (killed, or evicted by
// another UiAutomation client such as `uiautomator dump`), it is restarted and the request
// retried once; that is safe because an unreachable agent never received the request.
func (d *device) call(ctx context.Context, req map[string]any, out any) error {
	_, a, err := d.conn(ctx)
	if err != nil {
		return err
	}
	err = a.call(ctx, req, out)
	var de *agentDialError
	if err == nil || !errors.As(err, &de) || ctx.Err() != nil {
		return err
	}
	logf("%s: agent unreachable (%v); restarting it", d.name, de.err)
	// If the guest itself restarted (adb offline / not booted), wait for it first.
	if err := d.waitBooted(ctx, 180*time.Second); err != nil {
		return fmt.Errorf("%v; device did not come back: %w", de, err)
	}
	if _, rerr := ensureAgent(ctx, a, logf); rerr != nil {
		return fmt.Errorf("%v; restarting the agent failed: %w", err, rerr)
	}
	return a.call(ctx, req, out)
}

// isZombie reports whether the emulator we are connected to is a child of this daemon that
// has exited (its pid is not reusable until reaped, so kill(0) alone would say "alive").
func isZombie(d *device) bool {
	return d.proc != nil && d.proc.Process != nil && d.proc.Process.Pid == d.run.PID && d.proc.ProcessState != nil
}

func (d *device) drop() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.emu != nil {
		d.emu.Close()
	}
	if d.agent != nil {
		d.agent.Close()
	}
	d.emu, d.agent, d.run, d.dispAt, d.agentOK = nil, nil, runningEmu{}, time.Time{}, false
}

// display returns the logical display, refreshing it when older than maxAge.
func (d *device) display(ctx context.Context, a *agentClient, maxAge time.Duration) (displayInfo, error) {
	d.mu.Lock()
	di, at := d.disp, d.dispAt
	d.mu.Unlock()
	if !at.IsZero() && time.Since(at) <= maxAge {
		return di, nil
	}
	if err := d.call(ctx, map[string]any{"cmd": "display"}, &di); err != nil {
		return di, err
	}
	d.setDisplay(di)
	return di, nil
}

func (d *device) setDisplay(di displayInfo) {
	d.mu.Lock()
	d.disp, d.dispAt = di, time.Now()
	d.mu.Unlock()
}

// ------------------------------------------------------------------ devices

func (b *Backend) List(ctx context.Context) ([]api.Device, error) {
	avds, err := listAVDs()
	if err != nil {
		return nil, err
	}
	run := map[string]runningEmu{}
	for _, r := range runningEmulators() {
		run[r.AVD] = r
	}
	out := []api.Device{}
	for _, a := range avds {
		out = append(out, b.describe(a, run))
	}
	return out, nil
}

func (b *Backend) describe(a avdInfo, run map[string]runningEmu) api.Device {
	d := b.dev(a.Name)
	dev := api.Device{ID: "android:" + a.Name, Name: a.Name, Platform: api.Android, State: "shutdown", OSVersion: a.osVersion()}
	scr := api.Screen{Width: a.intCfg("hw.lcd.width"), Height: a.intCfg("hw.lcd.height"), Density: a.intCfg("hw.lcd.density")}
	d.mu.Lock()
	booting := d.booting
	if !d.dispAt.IsZero() {
		scr = api.Screen{Width: d.disp.W, Height: d.disp.H, Density: d.disp.Density}
	}
	d.mu.Unlock()
	if _, ok := run[a.Name]; ok {
		dev.State = "booted"
		if standbyEnabled() && d.inStandby(context.Background()) {
			dev.State = "shutdown" // standby: paused at the baseline, boots in a moment
		}
	}
	if booting {
		dev.State = "booting"
	}
	dev.Screen = &scr
	dev.Profile = profileName(scr)
	return dev
}

func profileName(s api.Screen) string {
	for _, p := range profiles.All(api.Android) {
		if p.Width == s.Width && p.Height == s.Height && p.Density == s.Density {
			return p.Name
		}
	}
	if s.Width == 0 {
		return ""
	}
	return profiles.Custom(s.Width, s.Height, s.Density).Name
}

func (b *Backend) device(id string) (api.Device, error) {
	name, err := avdName(id)
	if err != nil {
		return api.Device{}, err
	}
	a, err := loadAVD(name)
	if err != nil {
		return api.Device{}, fmt.Errorf("unknown device %s: %w", id, err)
	}
	run := map[string]runningEmu{}
	if r, ok := findRunning(name); ok {
		run[name] = r
	}
	return b.describe(a, run), nil
}

func (b *Backend) Create(ctx context.Context, req api.CreateRequest) (api.Device, error) {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return api.Device{}, errors.New("create needs a name")
	}
	if strings.ContainsAny(name, "/: ") {
		return api.Device{}, fmt.Errorf("bad AVD name %q", name)
	}
	if !strings.HasPrefix(name, defaultAVDPrefix) {
		name = defaultAVDPrefix + name
	}
	prof := profiles.Default(api.Android)
	if req.Profile != "" {
		p, err := profiles.Get(req.Profile)
		if err != nil {
			return api.Device{}, err
		}
		if p.Platform != api.Android {
			return api.Device{}, fmt.Errorf("profile %s is not an android profile", p.Name)
		}
		prof = p
	}
	if _, err := createAVD(ctx, name, prof); err != nil {
		return api.Device{}, err
	}
	return b.device("android:" + name)
}

// ------------------------------------------------------------------ boot / shutdown

type ports struct{ console, grpc int }

func canListen(port int) bool {
	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	l.Close()
	return true
}

// allocPorts picks console/adb/gRPC ports that no running emulator uses (defaults 5580/5581/8580).
func allocPorts() (ports, error) {
	cbase := envInt("AGEMU_ANDROID_CONSOLE_PORT", 5580)
	gbase := envInt("AGEMU_ANDROID_GRPC_PORT", 8580)
	used := map[int]bool{}
	for _, r := range runningEmulators() {
		used[r.Console], used[r.ADBPort], used[r.GRPCPort] = true, true, true
	}
	for i := 0; i < 16; i++ {
		c, g := cbase+2*i, gbase+i
		if used[c] || used[c+1] || used[g] {
			continue
		}
		if canListen(c) && canListen(c+1) && canListen(g) {
			return ports{c, g}, nil
		}
	}
	return ports{}, fmt.Errorf("no free emulator ports from %d / %d", cbase, gbase)
}

func logDir() string {
	h := os.Getenv("AGEMU_HOME")
	if h == "" {
		home, _ := os.UserHomeDir()
		h = filepath.Join(home, ".agemu")
	}
	return filepath.Join(h, "android")
}

// emulatorArgs builds the command line. Snapshot handling: agemu keeps one named snapshot
// (the baseline) and never auto-saves (-no-snapshot-save), so snapshots do not pile up.
func emulatorArgs(name string, p ports, a avdInfo, loadBaseline bool) []string {
	ram := a.intCfg("hw.ramSize")
	if ram <= 0 {
		ram = envInt("AGEMU_ANDROID_RAM_MB", defaultRAMMB)
	}
	args := []string{
		"-avd", name, "-port", strconv.Itoa(p.console), "-grpc", strconv.Itoa(p.grpc),
		"-no-window", "-no-audio", "-no-boot-anim", "-gpu", env("AGEMU_ANDROID_GPU", "auto"),
		"-memory", strconv.Itoa(ram), "-no-snapshot-save",
	}
	// Emulator 37 raises guest RAM to 4096 MB for API > 36 images; -lowram is the only switch
	// that lifts this lower bound. On this emulator it does not set ro.config.low_ram in the
	// guest (verified at boot, see checkGuest), so the device is a normal 2 GB phone.
	if ram < 4096 {
		args = append(args, "-lowram")
	}
	if loadBaseline && !inPlaceBootLoad() {
		args = append(args, "-snapshot", baselineSnapshot)
	} else {
		args = append(args, "-no-snapshot-load")
	}
	// Extra emulator flags, e.g. "-show-kernel" or "-feature OnDemandSnapshotLoad".
	args = append(args, strings.Fields(os.Getenv("AGEMU_ANDROID_EMULATOR_FLAGS"))...)
	return args
}

// inPlaceBootLoad: a new emulator process starts without loading a snapshot and the baseline
// is then restored in place (gRPC LoadSnapshot), as reset does. Restoring it while the process
// starts (-snapshot) corrupts guest memory on this emulator (37.1, HVF, 16 KB-page image): the
// guest kernel logs "BUG: Bad page map", oopses and reboots within seconds (3 of 3 boots, seen
// with -show-kernel). In-place loads showed no such errors. AGEMU_ANDROID_BOOT_LOAD=startup
// restores the old behaviour.
func inPlaceBootLoad() bool { return env("AGEMU_ANDROID_BOOT_LOAD", "inplace") != "startup" }

func (b *Backend) Boot(ctx context.Context, id string, opts api.BootOptions) (api.Device, error) {
	d, err := b.devFor(id)
	if err != nil {
		return api.Device{}, err
	}
	d.life.Lock()
	defer d.life.Unlock()
	a, err := loadAVD(d.name)
	if err != nil {
		return api.Device{}, fmt.Errorf("unknown device %s: %w", id, err)
	}
	if _, ok := findRunning(d.name); ok {
		if !opts.Cold && d.inStandby(ctx) {
			if err := d.resume(ctx); err != nil {
				return api.Device{}, err
			}
			d.mu.Lock()
			d.agentOK = true
			d.mu.Unlock()
			if _, err := d.refreshDisplay(ctx); err != nil {
				return api.Device{}, err
			}
			return b.device(id)
		}
		if !opts.Cold {
			if _, _, err := d.ready(ctx); err != nil {
				return api.Device{}, err
			}
			if _, err := d.refreshDisplay(ctx); err != nil {
				return api.Device{}, err
			}
			return b.device(id)
		}
		if err := d.shutdown(ctx); err != nil {
			return api.Device{}, err
		}
	}

	d.setBooting(true)
	defer d.setBooting(false)

	loadBaseline := !opts.Cold && a.hasBaseline()
	t0 := time.Now()
	if err := d.launch(ctx, a, loadBaseline); err != nil {
		return api.Device{}, err
	}
	t1 := time.Now()
	if loadBaseline && inPlaceBootLoad() {
		d.mu.Lock()
		e := d.emu
		d.mu.Unlock()
		err = d.loadBaseline(ctx, e)
		if err != nil {
			return api.Device{}, fmt.Errorf("restoring %s: %w", baselineSnapshot, err)
		}
		logf("%s: boot: baseline restored in place %v after gRPC came up", d.name, time.Since(t1).Round(time.Millisecond))
	}
	if loadBaseline {
		err = d.waitAgentOrBooted(ctx, 180*time.Second)
	} else {
		err = d.waitBooted(ctx, 180*time.Second)
	}
	if err != nil {
		return api.Device{}, err
	}
	logf("%s: boot: emulator gRPC up %v, then guest ready %v later", d.name, t1.Sub(t0).Round(time.Millisecond), time.Since(t1).Round(time.Millisecond))
	_, ag, err := d.connRaw(ctx)
	if err != nil {
		return api.Device{}, err
	}
	// After a snapshot load the agent from the snapshot should answer at once. Only an agent of
	// a different version (agemu was upgraded) makes the baseline stale and worth retaking;
	// an agent that does not answer is just restarted.
	stale := false
	if loadBaseline {
		deadline := time.Now().Add(5 * time.Second)
		for {
			pctx, cancel := context.WithTimeout(ctx, time.Second)
			p, perr := ag.ping(pctx)
			cancel()
			if perr == nil {
				stale = p.Version != agentVersion
				break
			}
			if time.Now().After(deadline) {
				logf("%s: agent from %s does not answer (%v); restarting it", d.name, baselineSnapshot, perr)
				stale = true // the snapshot was taken without a live agent
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	restarted, err := ensureAgent(ctx, ag, logf)
	if err != nil {
		return api.Device{}, err
	}
	d.mu.Lock()
	d.agentOK = true
	d.mu.Unlock()
	d.mu.Lock()
	if stale && d.retook {
		stale = false // retake at most once per daemon, never loop
	}
	if stale {
		d.retook = true
	}
	d.mu.Unlock()
	if stale {
		logf("%s: %s has no current agent; retaking it", d.name, baselineSnapshot)
	}
	if !loadBaseline || restarted {
		d.checkGuest(ctx)
	}
	if !a.hasBaseline() || stale {
		if err := d.makeBaseline(ctx); err != nil {
			return api.Device{}, fmt.Errorf("creating baseline snapshot: %w", err)
		}
	}
	if _, err := d.refreshDisplay(ctx); err != nil {
		return api.Device{}, err
	}
	d.setBooting(false)
	return b.device(id)
}

func (d *device) setBooting(v bool) {
	d.mu.Lock()
	d.booting = v
	d.mu.Unlock()
}

func (d *device) refreshDisplay(ctx context.Context) (displayInfo, error) {
	_, a, err := d.conn(ctx)
	if err != nil {
		return displayInfo{}, err
	}
	return d.display(ctx, a, 0)
}

// launch starts the emulator detached from the daemon and waits for its gRPC endpoint.
func (d *device) launch(ctx context.Context, a avdInfo, loadBaseline bool) error {
	p, err := allocPorts()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(logDir(), 0o755); err != nil {
		return err
	}
	lf, err := os.OpenFile(filepath.Join(logDir(), d.name+".log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer lf.Close()
	args := emulatorArgs(d.name, p, a, loadBaseline)
	cmd := exec.Command(sdkTool("emulator", "emulator"), args...)
	cmd.Stdout, cmd.Stderr = lf, lf
	cmd.Env = os.Environ()
	if env("AGEMU_ANDROID_SNAPSHOT_COMPRESS", "") == "1" {
		cmd.Env = append(cmd.Env, "ANDROID_SNAPSHOT_COMPRESS=1")
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // survives the daemon
	logf("%s: starting emulator %s", d.name, strings.Join(args, " "))
	if err := cmd.Start(); err != nil {
		return err
	}
	d.mu.Lock()
	d.proc = cmd
	d.mu.Unlock()
	go func() { _ = cmd.Wait() }() // reap

	deadline := time.Now().Add(60 * time.Second)
	for {
		if r, ok := findRunning(d.name); ok && r.GRPCPort != 0 {
			e, err := dialEmu(r)
			if err != nil {
				return err
			}
			if err := e.waitGRPC(ctx, time.Until(deadline)); err != nil {
				e.Close()
				return err
			}
			d.mu.Lock()
			if d.emu != nil {
				d.emu.Close()
			}
			if d.agent != nil {
				d.agent.Close()
			}
			d.run, d.emu, d.agent, d.dispAt, d.agentOK = r, e, newAgent(d.name, r), time.Time{}, false
			d.lastLoad = time.Time{} // a new emulator process has no texture loader running
			d.mu.Unlock()
			e.enableShm(filepath.Join(logDir(), d.name+".fb"), a.intCfg("hw.lcd.width"), a.intCfg("hw.lcd.height"))
			return nil
		}
		if cmd.ProcessState != nil || !pidAlive(cmd.Process.Pid) {
			return fmt.Errorf("emulator exited during startup; see %s", filepath.Join(logDir(), d.name+".log"))
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("emulator did not register its gRPC endpoint in 60s; see %s", filepath.Join(logDir(), d.name+".log"))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// waitBooted waits until adb sees the device and sys.boot_completed=1.
func (d *device) waitBooted(ctx context.Context, timeout time.Duration) error {
	d.mu.Lock()
	serial := d.run.serial()
	d.mu.Unlock()
	deadline := time.Now().Add(timeout)
	for {
		c, cancel := context.WithTimeout(ctx, 2*time.Second)
		out, err := adbShellOK(c, serial, "getprop sys.boot_completed")
		cancel()
		if err == nil && strings.TrimSpace(out) == "1" {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s did not finish booting in %v (last: %v %q)", serial, timeout, err, strings.TrimSpace(out))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

// waitAgentOrBooted waits until the agent restored from the baseline answers over TCP (no
// need to wait for adb, which re-handshakes after the snapshot load), or, when it does not
// answer, until adb sees sys.boot_completed=1.
func (d *device) waitAgentOrBooted(ctx context.Context, timeout time.Duration) error {
	d.mu.Lock()
	serial, a := d.run.serial(), d.agent
	d.mu.Unlock()
	deadline := time.Now().Add(timeout)
	for {
		if a != nil && a.tcp != nil {
			c, cancel := context.WithTimeout(ctx, 2*time.Second)
			p, err := a.ping(c)
			cancel()
			if err == nil && p.BootCompleted == "1" {
				return nil
			}
		}
		c, cancel := context.WithTimeout(ctx, time.Second)
		out, err := adbShellOK(c, serial, "getprop sys.boot_completed")
		cancel()
		if err == nil && strings.TrimSpace(out) == "1" {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s did not finish booting in %v (last: %v %q)", serial, timeout, err, strings.TrimSpace(out))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// checkGuest logs guest properties that would make the device differ from a real phone.
func (d *device) checkGuest(ctx context.Context) {
	d.mu.Lock()
	serial := d.run.serial()
	d.mu.Unlock()
	lowram, err := adbShellOK(ctx, serial, "getprop ro.config.low_ram")
	if err != nil {
		return
	}
	mem, _ := adbShellOK(ctx, serial, "head -1 /proc/meminfo")
	if strings.TrimSpace(lowram) == "true" {
		logf("%s: WARNING ro.config.low_ram=true (the emulator's -lowram flag changed guest behaviour); set AGEMU_ANDROID_RAM_MB=4096", d.name)
	}
	logf("%s: guest %s, ro.config.low_ram=%q", d.name, strings.Join(strings.Fields(mem), " "), strings.TrimSpace(lowram))
}

// makeBaseline brings a freshly cold-booted device to a settled home screen and saves
// the one snapshot agemu boots and resets from. The on-device agent is running when the
// snapshot is taken, so every snapshot boot / reset is agent-ready immediately.
func (d *device) makeBaseline(ctx context.Context) error {
	e, a, err := d.conn(ctx)
	if err != nil {
		return err
	}
	settle := time.Duration(envInt("AGEMU_ANDROID_BASELINE_SETTLE_S", 20)) * time.Second
	logf("%s: settling %v before taking %s", d.name, settle, baselineSnapshot)
	// Let boot-time work (launcher, package scans) finish so a restored device is quiet.
	time.Sleep(settle)
	// The emulator's touchscreen advertises a stylus, so Gboard treats taps on text fields as
	// stylus input and shows its floating handwriting toolbar instead of the keyboard. A phone
	// is used with a finger: turn stylus handwriting off (a user setting, `settings put`).
	d.mu.Lock()
	serial := d.run.serial()
	d.mu.Unlock()
	if _, err := adbShellOK(ctx, serial, "settings put secure stylus_handwriting_enabled 0"); err != nil {
		logf("%s: could not turn off stylus handwriting: %v", d.name, err)
	}
	d.warmUpKeyboard(ctx, e, a)
	_ = a.call(ctx, map[string]any{"cmd": "cleartasks"}, nil)
	_ = a.call(ctx, map[string]any{"cmd": "key", "code": keyCodes["home"]}, nil)
	_ = a.call(ctx, map[string]any{"cmd": "idle", "quietMs": 500, "timeoutMs": 10000}, nil)
	// The snapshot must contain a running agent, so boots from it are agent-ready at once.
	if _, err := ensureAgent(ctx, a, logf); err != nil {
		return fmt.Errorf("agent not running before snapshot: %w", err)
	}
	// No host connection or port redirection inside the snapshot: the emulator cannot load a
	// snapshot that holds the redirection's socket (see redirector.drop).
	a.Close()
	if a.rd != nil {
		a.rd.drop(ctx)
	}
	time.Sleep(500 * time.Millisecond)
	t := time.Now()
	sctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	if names, err := e.listSnapshots(sctx); err == nil {
		for _, n := range names {
			if n == baselineSnapshot {
				_ = e.deleteSnapshot(sctx, n)
			}
		}
	}
	if err := e.saveSnapshot(sctx, baselineSnapshot); err != nil {
		return err
	}
	logf("%s: saved %s in %v", d.name, baselineSnapshot, time.Since(t).Round(time.Millisecond))
	a.Close()
	return nil
}

// warmUpKeyboard shows the soft keyboard once before the baseline is taken. The emulator's
// virtio touchscreen advertises stylus support, so on first use Gboard opens a one-time
// "Try out your stylus" sheet that steals focus from the text field. Without this, that sheet
// would come back after every reset. Best effort: any failure only means no warm-up.
func (d *device) warmUpKeyboard(ctx context.Context, e *emuClient, a *agentClient) {
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	tree := func() *api.UITree {
		var raw rawTree
		if a.call(ctx, map[string]any{"cmd": "tree"}, &raw) != nil {
			return nil
		}
		return mapTree(&raw)
	}
	find := func(pred func(*api.Element) bool, timeout time.Duration) *api.Element {
		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) {
			if t := tree(); t != nil {
				var hit *api.Element
				t.Walk(func(el *api.Element) {
					if hit == nil && el.Frame.W > 0 && el.Frame.H > 0 && pred(el) {
						hit = el
					}
				})
				if hit != nil {
					return hit
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
		return nil
	}
	tapEl := func(el *api.Element) {
		di, err := a.display(ctx)
		if err != nil {
			return
		}
		m := newScreenMap(di.W, di.H, di.PhysW, di.PhysH)
		px, py := m.toPhysical(el.Frame.Center())
		_ = e.touch(ctx, px, py, 1)
		_ = e.touch(ctx, px, py, 0)
	}
	stylusSheet := func(el *api.Element) bool { return strings.Contains(strings.ToLower(el.Text), "try out your stylus") }
	if err := a.call(ctx, map[string]any{"cmd": "launch", "pkg": "com.android.settings"}, nil); err != nil {
		logf("%s: keyboard warm-up skipped: %v", d.name, err)
		return
	}
	bar := find(func(el *api.Element) bool {
		return strings.HasSuffix(el.ID, "/search_action_bar") || el.Text == "Search Settings" || el.Text == "Search settings"
	}, 8*time.Second)
	if bar == nil {
		logf("%s: keyboard warm-up skipped: no settings search bar", d.name)
		return
	}
	tapEl(bar)
	if find(func(el *api.Element) bool { return el.Role == "textfield" && el.Focused }, 8*time.Second) == nil {
		logf("%s: keyboard warm-up: no focused text field", d.name)
	}
	// Key events through the IME, not the default set-text: the sheet appears on keyboard input.
	_ = a.call(ctx, map[string]any{"cmd": "text", "text": "a", "keys": true}, nil)
	if sheet := find(stylusSheet, 6*time.Second); sheet != nil {
		if c := find(func(el *api.Element) bool { return el.Text == "Cancel" && el.Clickable }, 3*time.Second); c != nil {
			tapEl(c)
			logf("%s: dismissed Gboard's one-time stylus sheet", d.name)
		}
	}
	for i := 0; i < 3; i++ {
		_ = a.call(ctx, map[string]any{"cmd": "key", "code": keyCodes["back"]}, nil)
		time.Sleep(200 * time.Millisecond)
	}
	for _, p := range []string{"com.android.settings", "com.google.android.settings.intelligence"} {
		_ = a.call(ctx, map[string]any{"cmd": "stop", "pkg": p}, nil)
	}
}

func (b *Backend) Shutdown(ctx context.Context, id string) error {
	d, err := b.devFor(id)
	if err != nil {
		return err
	}
	d.life.Lock()
	defer d.life.Unlock()
	if !standbyEnabled() {
		return d.shutdown(ctx)
	}
	if _, ok := findRunning(d.name); !ok {
		d.drop()
		return nil
	}
	if d.inStandby(ctx) {
		// Shutting down a device that is already in standby powers it off for real.
		return d.shutdown(ctx)
	}
	return d.enterStandby(ctx)
}

// Standby ("pre-booted state"): instead of killing the emulator, shutdown restores the
// baseline snapshot in place and pauses the VM. The paused emulator uses no CPU (its guest
// RAM stays allocated on the host); the next boot only resumes it, already clean and
// agent-ready. Shutting down a device in standby powers it off. AGEMU_ANDROID_STANDBY=0
// disables standby (shutdown always powers off).
func standbyEnabled() bool { return env("AGEMU_ANDROID_STANDBY", "1") != "0" }

func (d *device) inStandby(ctx context.Context) bool {
	e, _, err := d.connRaw(ctx)
	if err != nil {
		return false
	}
	c, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	st, err := e.vmState(c)
	return err == nil && st == emupb.VmRunState_PAUSED
}

func (d *device) enterStandby(ctx context.Context) error {
	a, err := loadAVD(d.name)
	if err != nil {
		return err
	}
	if !a.hasBaseline() {
		return d.shutdown(ctx)
	}
	e, ag, err := d.ready(ctx)
	if err != nil {
		logf("%s: standby needs a live device (%v); powering off", d.name, err)
		return d.shutdown(ctx)
	}
	t := time.Now()
	if err := d.loadBaseline(ctx, e); err != nil {
		logf("%s: loading %s for standby failed (%v); powering off", d.name, baselineSnapshot, err)
		return d.shutdown(ctx)
	}
	ag.Close()
	if err := waitAgent(ctx, ag, 30*time.Second); err != nil {
		logf("%s: agent after standby load: %v; powering off", d.name, err)
		return d.shutdown(ctx)
	}
	if err := e.setVM(ctx, emupb.VmRunState_PAUSED); err != nil {
		return err
	}
	d.mu.Lock()
	d.dispAt = time.Time{}
	d.mu.Unlock()
	logf("%s: in standby (baseline restored and VM paused in %v)", d.name, time.Since(t).Round(time.Millisecond))
	return nil
}

// resume wakes a device from standby and waits until the agent answers.
func (d *device) resume(ctx context.Context) error {
	e, a, err := d.connRaw(ctx)
	if err != nil {
		return err
	}
	e.stream.invalidate()
	if err := e.setVM(ctx, emupb.VmRunState_RUNNING); err != nil {
		return err
	}
	defer d.markInput()
	return waitAgent(ctx, a, 30*time.Second)
}

func (d *device) shutdown(ctx context.Context) error {
	r, ok := findRunning(d.name)
	if !ok {
		d.drop()
		return nil
	}
	e, _, err := d.connRaw(ctx)
	if err == nil {
		c, cancel := context.WithTimeout(ctx, 3*time.Second)
		err = e.shutdown(c)
		cancel()
	}
	if err != nil {
		logf("%s: gRPC shutdown failed (%v), sending SIGTERM", d.name, err)
		_ = syscall.Kill(r.PID, syscall.SIGTERM)
	}
	deadline := time.Now().Add(30 * time.Second)
	for pidAlive(r.PID) && !d.reaped(r.PID) {
		if time.Now().After(deadline) {
			logf("%s: emulator pid %d still alive, SIGKILL", d.name, r.PID)
			_ = syscall.Kill(r.PID, syscall.SIGKILL)
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	d.drop()
	// The emulator removes its discovery file on exit; a killed one may leave it behind.
	if !pidAlive(r.PID) || d.reaped(r.PID) {
		_ = os.Remove(filepath.Join(discoveryDir(), fmt.Sprintf("pid_%d.ini", r.PID)))
	}
	return nil
}

func (d *device) reaped(pid int) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.proc != nil && d.proc.Process != nil && d.proc.Process.Pid == pid && d.proc.ProcessState != nil
}

// Reset restores the baseline snapshot in the running emulator. The screen profile that
// was active before the reset is re-applied, so an agent testing in a profile stays in it.
func (b *Backend) Reset(ctx context.Context, id string) error {
	d, err := b.devFor(id)
	if err != nil {
		return err
	}
	d.life.Lock()
	defer d.life.Unlock()
	a, err := loadAVD(d.name)
	if err != nil {
		return err
	}
	if !a.hasBaseline() {
		return fmt.Errorf("%s has no baseline snapshot yet (boot it once)", id)
	}
	e, ag, err := d.ready(ctx)
	if err != nil {
		return err
	}
	t0 := time.Now()
	prev, _ := d.display(ctx, ag, 0)
	t1 := time.Now()
	if err := d.loadBaseline(ctx, e); err != nil {
		return err
	}
	t2 := time.Now()
	ag.Close() // the connection died with the old VM state
	d.mu.Lock()
	d.dispAt = time.Time{}
	d.mu.Unlock()
	if err := waitAgent(ctx, ag, 30*time.Second); err != nil {
		return err
	}
	t3 := time.Now()
	d.markInput()
	logf("%s: reset: display %v, snapshot load %v, agent answering %v later (via %s)", d.name,
		t1.Sub(t0).Round(time.Millisecond), t2.Sub(t1).Round(time.Millisecond), t3.Sub(t2).Round(time.Millisecond), ag.transport())
	if prev.W > 0 && (prev.W != prev.PhysW || prev.H != prev.PhysH || prev.Density != prev.PhysDensity) {
		var di displayInfo
		if err := d.call(ctx, map[string]any{"cmd": "profile", "w": prev.W, "h": prev.H, "density": prev.Density, "settleMs": 2000}, &di); err != nil {
			return fmt.Errorf("re-applying profile after reset: %w", err)
		}
		d.setDisplay(di)
	}
	return nil
}

func waitAgent(ctx context.Context, a *agentClient, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		c, cancel := context.WithTimeout(ctx, time.Second)
		p, err := a.ping(c)
		cancel()
		if err == nil {
			if p.Version != agentVersion {
				_, err = ensureAgent(ctx, a, logf)
				return err
			}
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("agent did not come back after snapshot load: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// ------------------------------------------------------------------ profiles

func (b *Backend) SetProfile(ctx context.Context, id string, p api.Profile) (api.Device, error) {
	d, err := b.devFor(id)
	if err != nil {
		return api.Device{}, err
	}
	if p.Width <= 0 || p.Height <= 0 || p.Density <= 0 {
		return api.Device{}, fmt.Errorf("bad profile size %dx%d@%d", p.Width, p.Height, p.Density)
	}
	if p.Width > 8192 || p.Height > 8192 || p.Density < 72 || p.Density > 1200 {
		return api.Device{}, fmt.Errorf("profile %dx%d@%d out of range", p.Width, p.Height, p.Density)
	}
	d.life.Lock()
	defer d.life.Unlock()
	if _, _, err := d.ready(ctx); err != nil {
		return api.Device{}, err
	}
	var di displayInfo
	// The switch waits until the screen has settled, so its frames count from the start.
	d.markInput()
	err = d.call(ctx, map[string]any{"cmd": "profile", "w": p.Width, "h": p.Height, "density": p.Density, "settleMs": 3000}, &di)
	if err != nil {
		return api.Device{}, err
	}
	d.setDisplay(di)
	if di.W != p.Width || di.H != p.Height || di.Density != p.Density {
		return api.Device{}, fmt.Errorf("display is %dx%d@%d after applying %s", di.W, di.H, di.Density, p.Name)
	}
	dev, err := b.device(id)
	if err == nil && p.Name != "" && !strings.HasPrefix(p.Name, "custom-") {
		dev.Profile = p.Name
	}
	return dev, err
}

// ------------------------------------------------------------------ screenshot / UI

func (b *Backend) Screenshot(ctx context.Context, id string, o api.ScreenshotOptions) (api.Screenshot, error) {
	d, err := b.devFor(id)
	if err != nil {
		return api.Screenshot{}, err
	}
	e, a, err := d.conn(ctx)
	if err != nil {
		return api.Screenshot{}, err
	}
	// Grab the framebuffer while getting the logical display size (cached for 2 s like taps;
	// agemu's own profile / reset / boot calls refresh it immediately).
	var di displayInfo
	var derr error
	done := make(chan struct{})
	go func() { di, derr = d.display(ctx, a, 2*time.Second); close(done) }()
	// The latest posted frame from the stream (waiting for a recent input's effect), or a
	// framebuffer read when the stream does not have a current frame.
	var last time.Time
	if ns := d.lastInput.Load(); ns != 0 {
		last = time.Unix(0, ns)
	}
	f, fromStream := e.stream.get(ctx, last)
	if !fromStream {
		f, err = e.screenshot(ctx)
	}
	if trace {
		logf("%s: screenshot frame from stream=%v", d.name, fromStream)
	}
	<-done
	if err != nil {
		return api.Screenshot{}, err
	}
	if derr != nil {
		return api.Screenshot{}, derr
	}
	img := renderLogical(f, di)
	if o.Scale > 0 && o.Scale < 1 {
		img = resize(img, max(1, int(float64(img.W)*o.Scale+0.5)), max(1, int(float64(img.H)*o.Scale+0.5)))
	}
	var buf bytes.Buffer
	format := strings.ToLower(o.Format)
	switch format {
	case "", "png":
		format = "png"
		err = encodePNG(&buf, img)
	case "jpeg", "jpg":
		format = "jpeg"
		err = encodeJPEG(&buf, img)
	default:
		return api.Screenshot{}, fmt.Errorf("unsupported format %q (png|jpeg)", o.Format)
	}
	if err != nil {
		return api.Screenshot{}, err
	}
	if o.Path == "" {
		return api.Screenshot{}, errors.New("screenshot needs a path")
	}
	if err := os.WriteFile(o.Path, buf.Bytes(), 0o644); err != nil {
		return api.Screenshot{}, err
	}
	return api.Screenshot{Path: o.Path, Width: img.W, Height: img.H, Format: format, Bytes: buf.Len()}, nil
}

// renderLogical turns the framebuffer into the logical display image (the profile's pixels).
func renderLogical(f frame, di displayInfo) rgbaImage {
	if di.W <= 0 || di.H <= 0 {
		return rgbaImage{W: f.W, H: f.H, Pix: f.Pix}
	}
	m := newScreenMap(di.W, di.H, f.W, f.H)
	if m.identity() {
		return rgbaImage{W: f.W, H: f.H, Pix: f.Pix}
	}
	return resize(f.crop(m.R), di.W, di.H)
}

func (b *Backend) UITree(ctx context.Context, id string) (*api.UITree, error) {
	d, err := b.devFor(id)
	if err != nil {
		return nil, err
	}
	if _, _, err := d.conn(ctx); err != nil {
		return nil, err
	}
	var raw rawTree
	if err := d.call(ctx, map[string]any{"cmd": "tree"}, &raw); err != nil {
		return nil, err
	}
	d.mu.Lock()
	if d.disp.W != raw.W || d.disp.H != raw.H || d.disp.Density != raw.Density {
		d.dispAt = time.Time{} // display changed behind our back; refresh on next use
	}
	d.mu.Unlock()
	return mapTree(&raw), nil
}

// ------------------------------------------------------------------ input

// screen returns the current logical->framebuffer mapping (cached for a short time).
func (d *device) screen(ctx context.Context, a *agentClient) (screenMap, error) {
	di, err := d.display(ctx, a, 2*time.Second)
	if err != nil {
		return screenMap{}, err
	}
	return newScreenMap(di.W, di.H, di.PhysW, di.PhysH), nil
}

func (b *Backend) inputDev(ctx context.Context, id string) (*device, *emuClient, screenMap, error) {
	d, err := b.devFor(id)
	if err != nil {
		return nil, nil, screenMap{}, err
	}
	e, a, err := d.conn(ctx)
	if err != nil {
		return nil, nil, screenMap{}, err
	}
	m, err := d.screen(ctx, a)
	return d, e, m, err
}

func checkPoint(m screenMap, x, y int) error {
	if x < 0 || y < 0 || x >= m.LW || y >= m.LH {
		return fmt.Errorf("point %d,%d is outside the %dx%d screen", x, y, m.LW, m.LH)
	}
	return nil
}

func (b *Backend) Tap(ctx context.Context, id string, x, y int) error {
	d, e, m, err := b.inputDev(ctx, id)
	if err != nil {
		return err
	}
	defer d.markInput()
	if err := checkPoint(m, x, y); err != nil {
		return err
	}
	px, py := m.toPhysical(x, y)
	if err := e.touch(ctx, px, py, 1); err != nil {
		return err
	}
	return e.touch(ctx, px, py, 0)
}

func (b *Backend) Swipe(ctx context.Context, id string, x1, y1, x2, y2, durationMs int) error {
	d, e, m, err := b.inputDev(ctx, id)
	if err != nil {
		return err
	}
	defer d.markInput()
	if err := checkPoint(m, x1, y1); err != nil {
		return err
	}
	if err := checkPoint(m, x2, y2); err != nil {
		return err
	}
	if durationMs < 0 {
		durationMs = 0
	}
	// One move event per ~8 ms (120 Hz), like a finger on a real touchscreen.
	steps := max(1, durationMs/8)
	start := time.Now()
	total := time.Duration(durationMs) * time.Millisecond
	ax, ay := m.toPhysical(x1, y1)
	if err := e.touch(ctx, ax, ay, 1); err != nil {
		return err
	}
	for i := 1; i <= steps; i++ {
		t := float64(i) / float64(steps)
		lx := x1 + int(float64(x2-x1)*t+0.5)
		ly := y1 + int(float64(y2-y1)*t+0.5)
		px, py := m.toPhysical(lx, ly)
		if wait := time.Duration(float64(total)*t) - time.Since(start); wait > 0 {
			time.Sleep(wait)
		}
		if err := e.touch(ctx, px, py, 1); err != nil {
			return err
		}
	}
	bx, by := m.toPhysical(x2, y2)
	return e.touch(ctx, bx, by, 0)
}

// TypeText types into the focused field. The agent inserts the text at the cursor of the
// focused editable node with AccessibilityNodeInfo ACTION_SET_TEXT (what UiAutomator and
// Appium setText do): the app's TextWatchers and listeners have run when it returns. Newlines
// and tabs are sent as Enter / Tab keys. With no focused editable node it falls back to key
// events (TypeKeys).
func (b *Backend) TypeText(ctx context.Context, id string, text string) error {
	return b.typeText(ctx, id, text, false)
}

// TypeKeys types text as one key event per character through the IME path, like
// `adb shell input text`, waiting until the app has handled each key. Slower; use it to
// exercise an app's key handling or the soft keyboard.
func (b *Backend) TypeKeys(ctx context.Context, id string, text string) error {
	return b.typeText(ctx, id, text, true)
}

func (b *Backend) typeText(ctx context.Context, id string, text string, keys bool) error {
	d, err := b.devFor(id)
	if err != nil {
		return err
	}
	if _, _, err := d.conn(ctx); err != nil {
		return err
	}
	if text == "" {
		return nil
	}
	defer d.markInput()
	var res struct {
		Mode string `json:"mode"`
	}
	return d.call(ctx, map[string]any{"cmd": "text", "text": text, "keys": keys}, &res)
}

// Android KeyEvent key codes for the named keys.
var keyCodes = map[string]int{
	"home": 3, "back": 4, "enter": 66, "return": 66, "delete": 67, "backspace": 67,
	"tab": 61, "escape": 111, "esc": 111, "volumeup": 24, "volumedown": 25,
	"lock": 26, "power": 26, "menu": 82, "recents": 187, "appswitch": 187,
	"up": 19, "down": 20, "left": 21, "right": 22, "space": 62, "forwarddelete": 112, "search": 84,
}

func (b *Backend) Key(ctx context.Context, id string, key string) error {
	d, err := b.devFor(id)
	if err != nil {
		return err
	}
	code, ok := keyCodes[strings.ToLower(key)]
	if !ok {
		names := make([]string, 0, len(keyCodes))
		for k := range keyCodes {
			names = append(names, k)
		}
		sort.Strings(names)
		return fmt.Errorf("unknown key %q (have: %s)", key, strings.Join(names, ", "))
	}
	if _, _, err := d.conn(ctx); err != nil {
		return err
	}
	// Returns once InputManager has queued the key, like a tap through the emulator; follow it
	// with wait or ui to see its effect.
	defer d.markInput()
	return d.call(ctx, map[string]any{"cmd": "key", "code": code, "async": true}, nil)
}

// ------------------------------------------------------------------ apps

func (b *Backend) Install(ctx context.Context, id, path string) error {
	d, err := b.devFor(id)
	if err != nil {
		return err
	}
	if _, _, err := d.conn(ctx); err != nil {
		return err
	}
	if !strings.HasSuffix(strings.ToLower(path), ".apk") {
		return fmt.Errorf("android install needs an .apk, got %s", path)
	}
	d.mu.Lock()
	serial := d.run.serial()
	d.mu.Unlock()
	if err := waitDevice(ctx, serial, 60*time.Second); err != nil {
		return err
	}
	out, err := adbExec(ctx, serial, "install", "-r", path)
	if err != nil {
		return err
	}
	if !strings.Contains(out, "Success") {
		return fmt.Errorf("adb install: %s", strings.TrimSpace(out))
	}
	return nil
}

// Launch starts the app's launcher activity (or "pkg/activity") and waits until it is
// displayed, like `am start -W`, but from the long-lived agent.
func (b *Backend) Launch(ctx context.Context, id, app string) error {
	d, err := b.devFor(id)
	if err != nil {
		return err
	}
	if _, _, err := d.conn(ctx); err != nil {
		return err
	}
	pkg, act, _ := strings.Cut(app, "/")
	if pkg == "" {
		return errors.New("launch needs a package name")
	}
	// Launch returns once the app has drawn: its frames count from the start of the launch.
	d.markInput()
	return d.call(ctx, map[string]any{"cmd": "launch", "pkg": pkg, "activity": act}, nil)
}

func (b *Backend) Terminate(ctx context.Context, id, app string) error {
	d, err := b.devFor(id)
	if err != nil {
		return err
	}
	if _, _, err := d.conn(ctx); err != nil {
		return err
	}
	defer d.markInput()
	return d.call(ctx, map[string]any{"cmd": "stop", "pkg": app}, nil)
}

func (b *Backend) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, d := range b.devs {
		d.drop()
	}
	return nil
}
