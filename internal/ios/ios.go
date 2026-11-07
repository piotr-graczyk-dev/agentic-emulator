// Package ios is the iOS Simulator backend. It drives the real iOS Simulator runtime
// through CoreSimulator, via the long-lived agemu-ios-helper process (ios-helper/), which
// reads the framebuffer, injects HID events and reads the accessibility tree in-process.
//
// Device ids are "ios:<UDID>". By default agemu lists and acts on simulators whose name
// starts with "agemu-" only, so it never touches simulators that other tools or people
// use. AGEMU_IOS_ALL_DEVICES=1 lists every simulator; AGEMU_IOS_ALLOW_FOREIGN=1 also
// allows acting on non-agemu simulators.
package ios

import (
	"context"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/api"
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/backend"
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/profiles"
)

// NamePrefix marks simulators that agemu manages.
const NamePrefix = "agemu-"

// simDevice is the helper's description of one simulator.
type simDevice struct {
	UDID           string  `json:"udid"`
	Name           string  `json:"name"`
	State          string  `json:"state"`
	StateCode      int     `json:"stateCode"` // 0 creating, 1 shutdown, 2 booting, 3 booted, 4 shutting down
	DeviceType     string  `json:"deviceType"`
	DeviceTypeName string  `json:"deviceTypeName"`
	Runtime        string  `json:"runtime"`
	RuntimeVersion string  `json:"runtimeVersion"`
	Width          int     `json:"width"`
	Height         int     `json:"height"`
	Scale          float64 `json:"scale"`
	DataPath       string  `json:"dataPath"` // the device's data directory on the host
}

const (
	stateShutdown = 1
	stateBooting  = 2
	stateBooted   = 3
)

type Backend struct {
	mu sync.Mutex
	h  *helper
	// lastUse orders booted agemu devices for the max-booted policy.
	lastUse map[string]time.Time
	// apps maps udid -> pid -> bundle id, filled by Launch and by `launchctl list` (only used
	// when accessibility does not report the frontmost app's bundle id).
	apps map[string]map[int]string
	// systemAppCache holds the built-in apps per runtime identifier.
	systemAppCache map[string][]string
	// lean maps udid -> the lean setting last applied to that device; leaning marks devices
	// being reconciled and leanAgain a request that arrived meanwhile; wallGen numbers each
	// device's cold boots for the wallpaper passes (see lean.go).
	lean, leaning, leanAgain map[string]bool
	wallGen                  map[string]int
	// anim maps udid -> the animation coefficient last applied to that device (see
	// animations.go; 0 is normal speed).
	anim map[string]float64
	// bg is cancelled by Close; bgWG tracks the background work that uses it.
	bg     context.Context
	stopBg context.CancelFunc
	bgWG   sync.WaitGroup
	closed bool
}

// New returns at once. The helper process is started in the background, so the first iOS
// command does not pay for loading CoreSimulator (about 0.3 s on an idle Mac, several
// seconds on a loaded one). AGEMU_IOS_PRESTART=0 starts it on first use instead.
func New() *Backend {
	b := &Backend{lastUse: map[string]time.Time{}, apps: map[string]map[int]string{}, systemAppCache: map[string][]string{},
		lean: map[string]bool{}, leaning: map[string]bool{}, leanAgain: map[string]bool{}, wallGen: map[string]int{},
		anim: map[string]float64{}}
	b.bg, b.stopBg = context.WithCancel(context.Background())
	if os.Getenv("AGEMU_IOS_PRESTART") != "0" {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			start := time.Now()
			if err := b.call(ctx, "list", nil, nil); err != nil {
				log.Printf("ios: helper prestart: %v", err)
				return
			}
			log.Printf("ios: helper ready in %.0f ms", ms(time.Since(start)))
		}()
	}
	return b
}

func (b *Backend) Platform() api.Platform { return api.IOS }

func (b *Backend) helper() (*helper, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, fmt.Errorf("the iOS backend is shutting down")
	}
	if b.h != nil && b.h.alive() {
		return b.h, nil
	}
	h, err := startHelper()
	if err != nil {
		return nil, err
	}
	b.h = h
	return h, nil
}

func (b *Backend) call(ctx context.Context, cmd string, params map[string]any, out any) error {
	h, err := b.helper()
	if err != nil {
		return err
	}
	return h.call(ctx, cmd, params, out)
}

func (b *Backend) Close() error {
	b.mu.Lock()
	b.closed = true // no new helper and no new background work from here on
	h := b.h
	b.h = nil
	b.mu.Unlock()
	b.stopBg()
	// Close the helper before waiting: a background call blocked writing to it fails then,
	// instead of keeping Close waiting.
	if h != nil {
		h.close()
	}
	b.bgWG.Wait()
	return nil
}

func udidOf(id string) (string, error) {
	u, ok := strings.CutPrefix(id, string(api.IOS)+":")
	if !ok || u == "" {
		return "", fmt.Errorf("not an iOS device id: %q", id)
	}
	return u, nil
}

func managed(name string) bool { return strings.HasPrefix(name, NamePrefix) }

func envOn(name string) bool {
	v := strings.ToLower(os.Getenv(name))
	return v == "1" || v == "true" || v == "yes"
}

// target resolves a device id and refuses non-agemu simulators unless explicitly allowed.
func (b *Backend) target(ctx context.Context, id string) (simDevice, error) {
	udid, err := udidOf(id)
	if err != nil {
		return simDevice{}, err
	}
	var d simDevice
	if err := b.call(ctx, "describe", map[string]any{"udid": udid}, &d); err != nil {
		return simDevice{}, err
	}
	if !managed(d.Name) && !envOn("AGEMU_IOS_ALLOW_FOREIGN") {
		return simDevice{}, fmt.Errorf("refusing to act on simulator %q (%s): agemu only drives simulators named %s* (set AGEMU_IOS_ALLOW_FOREIGN=1 to override)", d.Name, d.UDID, NamePrefix)
	}
	b.mu.Lock()
	b.lastUse[d.UDID] = time.Now()
	b.mu.Unlock()
	return d, nil
}

func profileFor(deviceType string) string {
	for _, p := range profiles.All(api.IOS) {
		if p.DeviceType == deviceType {
			return p.Name
		}
	}
	return ""
}

func normalizeState(s string) string {
	switch strings.ToLower(s) {
	case "booted":
		return "booted"
	case "shutdown":
		return "shutdown"
	case "booting":
		return "booting"
	case "shutting down":
		return "shutting-down"
	case "creating":
		return "creating"
	}
	return strings.ToLower(s)
}

func (d simDevice) api() api.Device {
	out := api.Device{
		ID:       string(api.IOS) + ":" + d.UDID,
		Name:     d.Name,
		Platform: api.IOS,
		State:    normalizeState(d.State),
		Profile:  profileFor(d.DeviceType),
	}
	if d.Width > 0 {
		out.Screen = &api.Screen{Width: d.Width, Height: d.Height, Density: int(d.Scale*160 + 0.5)}
	}
	if d.RuntimeVersion != "" {
		out.OSVersion = "iOS " + d.RuntimeVersion
	}
	return out
}

func (b *Backend) listSims(ctx context.Context) ([]simDevice, error) {
	var ds []simDevice
	if err := b.call(ctx, "list", nil, &ds); err != nil {
		return nil, err
	}
	return ds, nil
}

// List returns agemu-* simulators (all simulators with AGEMU_IOS_ALL_DEVICES=1).
func (b *Backend) List(ctx context.Context) ([]api.Device, error) {
	ds, err := b.listSims(ctx)
	if err != nil {
		return nil, err
	}
	all := envOn("AGEMU_IOS_ALL_DEVICES")
	out := []api.Device{}
	for _, d := range ds {
		if all || managed(d.Name) {
			out = append(out, d.api())
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (b *Backend) Create(ctx context.Context, req api.CreateRequest) (api.Device, error) {
	prof := profiles.Default(api.IOS)
	if req.Profile != "" {
		p, err := profiles.Get(req.Profile)
		if err != nil {
			return api.Device{}, err
		}
		if p.Platform != api.IOS {
			return api.Device{}, fmt.Errorf("profile %s is for %s", p.Name, p.Platform)
		}
		prof = p
	}
	name := req.Name
	if name == "" {
		name = NamePrefix + prof.Name
	}
	if !managed(name) {
		name = NamePrefix + name
	}
	d, err := b.createSim(ctx, name, prof.DeviceType)
	if err != nil {
		return api.Device{}, err
	}
	return d.api(), nil
}

// createSim returns the simulator called name, creating it (simctl create) if missing.
func (b *Backend) createSim(ctx context.Context, name, deviceType string) (simDevice, error) {
	ds, err := b.listSims(ctx)
	if err != nil {
		return simDevice{}, err
	}
	for _, d := range ds {
		if d.Name == name {
			if d.DeviceType != deviceType {
				return simDevice{}, fmt.Errorf("simulator %s exists with device type %s, not %s", name, d.DeviceType, deviceType)
			}
			return d, nil
		}
	}
	runtime, err := newestRuntime(ctx)
	if err != nil {
		return simDevice{}, err
	}
	out, err := simctl(ctx, "create", name, deviceType, runtime)
	if err != nil {
		return simDevice{}, err
	}
	udid := strings.TrimSpace(out)
	// The helper's device set learns about the new device asynchronously.
	deadline := time.Now().Add(15 * time.Second)
	for {
		var d simDevice
		if err := b.call(ctx, "describe", map[string]any{"udid": udid}, &d); err == nil {
			return d, nil
		} else if time.Now().After(deadline) {
			return simDevice{}, fmt.Errorf("created %s (%s) but it did not appear: %w", name, udid, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Boot boots the simulator headless (no Simulator.app) if needed and returns once it is
// agent-ready: booted, SpringBoard answering accessibility, framebuffer readable, HID
// connected. iOS Simulators have no snapshots, so Cold only matters for an already booted
// device: it is shut down and booted again.
func (b *Backend) Boot(ctx context.Context, id string, opts api.BootOptions) (api.Device, error) {
	d, err := b.target(ctx, id)
	if err != nil {
		return api.Device{}, err
	}
	if opts.Cold && d.StateCode == stateBooted {
		if err := b.shutdown(ctx, d.UDID); err != nil {
			return api.Device{}, err
		}
		d.StateCode = stateShutdown
	}
	ready, err := b.bootMakingRoom(ctx, d.UDID, d.StateCode == stateBooted)
	if err != nil {
		return api.Device{}, err
	}
	return ready.api(), nil
}

// overlapSwitch: may the old simulator shut down while the new one boots? CoreSimulator
// refused a boot ("Invalid argument") issued while another simulator was shutting down, and
// that device's next boot came up without a display; so the boot call is always issued first,
// and the overlap (the old device shutting down during the rest of the new boot) is used only
// when memory pressure is normal. AGEMU_IOS_OVERLAP_SWITCH=1/0 forces it on/off.
func overlapSwitch() bool {
	switch strings.ToLower(os.Getenv("AGEMU_IOS_OVERLAP_SWITCH")) {
	case "1", "true", "yes":
		return true
	case "0", "false", "no":
		return false
	}
	return !memoryPressured()
}

// bootMakingRoom boots udid (if needed) until interactive while shutting down the least
// recently used agemu simulators beyond the booted limit: first, or, when overlapSwitch
// allows it, right after the new device's boot call returned.
func (b *Backend) bootMakingRoom(ctx context.Context, udid string, booted bool) (simDevice, error) {
	victims, err := b.evictionVictims(ctx, udid)
	if err != nil {
		return simDevice{}, err
	}
	shutdownAll := func() error {
		var wg sync.WaitGroup
		errs := make([]error, len(victims))
		for i, v := range victims {
			wg.Add(1)
			go func(i int, v simDevice) {
				defer wg.Done()
				if err := b.shutdown(ctx, v.UDID); err != nil {
					errs[i] = fmt.Errorf("shutting down %s to stay within %d booted agemu simulators: %w", v.Name, effectiveMaxBooted(), err)
				}
			}(i, v)
		}
		wg.Wait()
		for _, e := range errs {
			if e != nil {
				return e
			}
		}
		return nil
	}
	if len(victims) > 0 {
		names := make([]string, len(victims))
		for i, v := range victims {
			names[i] = v.Name
		}
		log.Printf("ios: shutting down %s (limit %d booted, memory: %s)", strings.Join(names, ", "), effectiveMaxBooted(), memoryState())
	}
	if len(victims) == 0 || booted || !overlapSwitch() {
		if err := shutdownAll(); err != nil {
			return simDevice{}, err
		}
		return b.bootReady(ctx, udid)
	}
	done := make(chan error, 1)
	started := false
	d, err := b.bootReadyThen(ctx, udid, func() {
		if !started {
			started = true
			go func() { done <- shutdownAll() }()
		}
	})
	// The boot can fail before its boot call returns (describe or boot failed); then the
	// shutdown never started and there is nothing to wait for.
	if started {
		if serr := <-done; err == nil && serr != nil {
			err = serr
		}
	}
	return d, err
}

// readyResult is the helper's "ready" answer: when each readiness milestone was reached (ms
// since the ready call started) and the device.
type readyResult struct {
	BootedMs        float64 `json:"bootedMs"`
	BootFinishedMs  float64 `json:"bootFinishedMs"`
	FramebufferMs   float64 `json:"framebufferMs"`
	HIDMs           float64 `json:"hidMs"`
	AccessibilityMs float64 `json:"accessibilityMs"`
	ReadyMs         float64 `json:"readyMs"`
	Accessibility   string  `json:"accessibility"` // "ready", or "warming" (still starting)
	BridgeRestarts  int     `json:"bridgeRestarts"`
	Timeline        []struct {
		Ms     float64 `json:"ms"`
		Event  string  `json:"event"`
		Detail string  `json:"detail"`
	} `json:"timeline"`
	Device simDevice `json:"device"`
}

// waitAccessibilityOnBoot: by default boot waits until SpringBoard's UI tree is readable, so
// the home screen is drawn when boot returns. Without that wait (AGEMU_IOS_BOOT_WAIT_AX=0) boot
// returns once the device is interactive (boot finished, framebuffer readable, HID connected),
// 1.5-5.5 s earlier, but a screenshot taken right away can still show the boot spinner.
func waitAccessibilityOnBoot() bool {
	switch strings.ToLower(os.Getenv("AGEMU_IOS_BOOT_WAIT_AX")) {
	case "0", "false", "no":
		return false
	}
	return true
}

// bootReady boots the simulator (headless, persisted) if needed and waits until it is
// interactive (see waitAccessibilityOnBoot). Cold boots log their readiness timeline to the
// daemon log.
func (b *Backend) bootReady(ctx context.Context, udid string) (simDevice, error) {
	return b.bootReadyThen(ctx, udid, nil)
}

// bootReadyThen is bootReady with a hook run once the boot call has returned (or at once when
// the device is already booted). A boot that comes up broken (no display, SpringBoard
// crashing) is shut down and booted again once.
func (b *Backend) bootReadyThen(ctx context.Context, udid string, afterBootCall func()) (simDevice, error) {
	d, err := b.bootReadyOnce(ctx, udid, afterBootCall)
	if err == nil || !strings.Contains(err.Error(), brokenBootMarker) {
		return d, err
	}
	log.Printf("ios boot %s: %v; shutting it down and booting it again", udid, err)
	if serr := b.shutdown(ctx, udid); serr != nil {
		return simDevice{}, fmt.Errorf("%w (and shutting it down failed: %v)", err, serr)
	}
	return b.bootReadyOnce(ctx, udid, nil)
}

// brokenBootMarker is part of the helper's error for a boot without a display.
const brokenBootMarker = "came up without a display"

func (b *Backend) bootReadyOnce(ctx context.Context, udid string, afterBootCall func()) (simDevice, error) {
	start := time.Now()
	var d simDevice
	if err := b.call(ctx, "describe", map[string]any{"udid": udid}, &d); err != nil {
		return simDevice{}, err
	}
	cold := d.StateCode != stateBooted
	var bootCall time.Duration
	if cold {
		if err := b.call(ctx, "boot", map[string]any{"udid": udid}, &d); err != nil {
			return simDevice{}, err
		}
		bootCall = time.Since(start)
	}
	if afterBootCall != nil {
		afterBootCall()
	}
	// The first boot of a new or erased simulator migrates data and can take minutes on a
	// busy host; later boots take seconds.
	timeout := 10 * time.Minute
	if dl, ok := ctx.Deadline(); ok {
		timeout = time.Until(dl)
	}
	rctx, cancel := context.WithTimeout(ctx, timeout+5*time.Second)
	defer cancel()
	var r readyResult
	params := map[string]any{"udid": udid, "timeoutMs": timeout.Milliseconds(), "waitAccessibility": waitAccessibilityOnBoot()}
	if err := b.call(rctx, "ready", params, &r); err != nil {
		return simDevice{}, err
	}
	if cold || r.BridgeRestarts > 0 {
		off := ms(bootCall)
		at := func(v float64) string {
			if v < 0 {
				return "-"
			}
			return fmt.Sprintf("%.0f", off+v)
		}
		what := "agent-ready"
		if r.Accessibility == "warming" {
			what = "interactive (accessibility still warming up)"
		}
		log.Printf("ios boot %s (%s): %s in %.0f ms; ms since boot started: boot call returned %.0f, booted %s, boot finished %s, framebuffer %s, HID %s, accessibility %s; bridge restarts %d",
			r.Device.Name, udid, what, ms(time.Since(start)), off, at(r.BootedMs), at(r.BootFinishedMs), at(r.FramebufferMs), at(r.HIDMs), at(r.AccessibilityMs), r.BridgeRestarts)
		var tl []string
		for _, e := range r.Timeline {
			s := fmt.Sprintf("%.0f %s", off+e.Ms, e.Event)
			if e.Detail != "" {
				s += " (" + e.Detail + ")"
			}
			tl = append(tl, s)
		}
		log.Printf("ios boot %s timeline: %s", udid, strings.Join(tl, ", "))
	}
	dev := r.Device
	if dev.UDID == "" {
		var err error
		if dev, err = b.describeSim(ctx, udid); err != nil {
			return simDevice{}, err
		}
	}
	b.leanDevice(dev, cold)
	b.animateDevice(ctx, dev, cold)
	return dev, nil
}

func (b *Backend) describeSim(ctx context.Context, udid string) (simDevice, error) {
	var d simDevice
	err := b.call(ctx, "describe", map[string]any{"udid": udid}, &d)
	return d, err
}

func (b *Backend) Shutdown(ctx context.Context, id string) error {
	d, err := b.target(ctx, id)
	if err != nil {
		return err
	}
	return b.shutdown(ctx, d.UDID)
}

func (b *Backend) shutdown(ctx context.Context, udid string) error {
	if err := b.call(ctx, "shutdown", map[string]any{"udid": udid}, nil); err != nil {
		return err
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		var d simDevice
		if err := b.call(ctx, "describe", map[string]any{"udid": udid}, &d); err != nil {
			return err
		}
		if d.StateCode == stateShutdown {
			b.mu.Lock()
			delete(b.apps, udid)
			b.mu.Unlock()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("simulator %s still %s after shutdown", udid, d.State)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// effectiveMaxBooted is how many agemu simulators may run at once: AGEMU_IOS_MAX_BOOTED if
// set; otherwise 2, or 1 while memory is short (pressure warn/critical or under half
// available): a booted simulator takes 2.5-4 GB (lean, see lean.go; more with an app), and a
// second one slows everything, including the boot of the first.
func effectiveMaxBooted() int {
	var n int
	if _, err := fmt.Sscanf(os.Getenv("AGEMU_IOS_MAX_BOOTED"), "%d", &n); err == nil && n > 0 {
		return n
	}
	if memoryPressured() {
		return 1
	}
	return 2
}

// evictionVictims returns the least recently used booted agemu simulators (never keep, never a
// non-agemu simulator) to shut down so that keep plus the others fit within
// effectiveMaxBooted().
func (b *Backend) evictionVictims(ctx context.Context, keep string) ([]simDevice, error) {
	ds, err := b.listSims(ctx)
	if err != nil {
		return nil, err
	}
	var others []simDevice
	for _, d := range ds {
		if managed(d.Name) && d.UDID != keep && (d.StateCode == stateBooted || d.StateCode == stateBooting) {
			others = append(others, d)
		}
	}
	allowed := effectiveMaxBooted() - 1
	if len(others) <= allowed {
		return nil, nil
	}
	b.mu.Lock()
	sort.Slice(others, func(i, j int) bool { return b.lastUse[others[i].UDID].Before(b.lastUse[others[j].UDID]) })
	b.mu.Unlock()
	return others[:len(others)-allowed], nil
}

// SetProfile switches to the agemu simulator of the profile's device type (agemu-<profile>),
// creating and booting it if needed. iOS screens are fixed per device type, so a profile
// switch is a device switch: the returned device has a different id.
func (b *Backend) SetProfile(ctx context.Context, id string, p api.Profile) (api.Device, error) {
	cur, err := b.target(ctx, id)
	if err != nil {
		return api.Device{}, err
	}
	if p.DeviceType == "" {
		return api.Device{}, fmt.Errorf("profile %s has no simulator device type", p.Name)
	}
	d := cur
	if cur.DeviceType != p.DeviceType {
		if d, err = b.findOrCreateForProfile(ctx, p); err != nil {
			return api.Device{}, err
		}
	}
	b.mu.Lock()
	b.lastUse[d.UDID] = time.Now()
	b.mu.Unlock()
	ready, err := b.bootMakingRoom(ctx, d.UDID, d.StateCode == stateBooted)
	if err != nil {
		return api.Device{}, err
	}
	return ready.api(), nil
}

func (b *Backend) findOrCreateForProfile(ctx context.Context, p api.Profile) (simDevice, error) {
	ds, err := b.listSims(ctx)
	if err != nil {
		return simDevice{}, err
	}
	name := NamePrefix + p.Name
	var sameType *simDevice
	for i, d := range ds {
		if d.Name == name {
			if d.DeviceType != p.DeviceType {
				return simDevice{}, fmt.Errorf("simulator %s is a %s, not the %s that profile %s needs; delete or rename it", name, d.DeviceType, p.DeviceType, p.Name)
			}
			return d, nil
		}
		if managed(d.Name) && d.DeviceType == p.DeviceType && sameType == nil {
			sameType = &ds[i]
		}
	}
	if sameType != nil {
		return *sameType, nil
	}
	return b.createSim(ctx, name, p.DeviceType)
}

func (b *Backend) Screenshot(ctx context.Context, id string, o api.ScreenshotOptions) (api.Screenshot, error) {
	d, err := b.target(ctx, id)
	if err != nil {
		return api.Screenshot{}, err
	}
	params := map[string]any{"udid": d.UDID, "path": o.Path, "format": o.Format}
	if o.Scale > 0 && o.Scale < 1 {
		params["scale"] = o.Scale
	}
	var s api.Screenshot
	if err := b.call(ctx, "screenshot", params, &s); err != nil {
		return api.Screenshot{}, err
	}
	return s, nil
}

func (b *Backend) UITree(ctx context.Context, id string) (*api.UITree, error) {
	d, err := b.target(ctx, id)
	if err != nil {
		return nil, err
	}
	t, err := b.readTree(ctx, d.UDID)
	if err != nil {
		return nil, err
	}
	app := t.BundleID
	if app == "" {
		app = b.appForPid(ctx, d.UDID, t.Pid)
	}
	return convertTree(t, app), nil
}

// readTree reads the frontmost app's accessibility tree within the caller's deadline (20 s
// when there is none).
func (b *Backend) readTree(ctx context.Context, udid string) (*axTree, error) {
	budget := 20 * time.Second
	if dl, ok := ctx.Deadline(); ok {
		budget = time.Until(dl) - 200*time.Millisecond
		if budget < 100*time.Millisecond {
			budget = 100 * time.Millisecond
		}
	}
	var t axTree
	if err := b.call(ctx, "ui", map[string]any{"udid": udid, "timeoutMs": budget.Milliseconds()}, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// appForPid names the frontmost app when accessibility did not. Pids of apps launched through
// agemu are known; others are looked up once with `launchctl list` inside the simulator.
func (b *Backend) appForPid(ctx context.Context, udid string, pid int) string {
	b.mu.Lock()
	if app, ok := b.apps[udid][pid]; ok {
		b.mu.Unlock()
		return app
	}
	b.mu.Unlock()
	var running []struct {
		BundleID string `json:"bundleId"`
		Pid      int    `json:"pid"`
	}
	if err := b.call(ctx, "running", map[string]any{"udid": udid, "springboard": true}, &running); err != nil {
		return ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	m := map[int]string{}
	for _, r := range running {
		m[r.Pid] = r.BundleID
	}
	b.apps[udid] = m
	if _, ok := m[pid]; !ok {
		m[pid] = "" // unknown: do not look it up again
	}
	return m[pid]
}

func (b *Backend) Tap(ctx context.Context, id string, x, y int) error {
	d, err := b.target(ctx, id)
	if err != nil {
		return err
	}
	if x < 0 || y < 0 || (d.Width > 0 && (x >= d.Width || y >= d.Height)) {
		return fmt.Errorf("tap (%d,%d) is outside the %dx%d screen", x, y, d.Width, d.Height)
	}
	return b.call(ctx, "tap", map[string]any{"udid": d.UDID, "x": x, "y": y}, nil)
}

func (b *Backend) Swipe(ctx context.Context, id string, x1, y1, x2, y2, durationMs int) error {
	d, err := b.target(ctx, id)
	if err != nil {
		return err
	}
	return b.call(ctx, "swipe", map[string]any{"udid": d.UDID, "x1": x1, "y1": y1, "x2": x2, "y2": y2, "durationMs": durationMs}, nil)
}

// typeable reports whether the US hardware keyboard can type s (printable ASCII, tab, newline).
func typeable(s string) bool {
	for _, r := range s {
		if r == '\n' || r == '\t' {
			continue
		}
		if r < 0x20 || r > 0x7e {
			return false
		}
	}
	return true
}

// Settle waits until the screen has had no big change for 48 ms, at most 1.5 s, and reports
// whether it got there. Big means more than 0.4% of a sparse pixel grid: scrolling (including
// momentum) and transitions count, a blinking cursor, a spinner or a countdown do not, so
// those no longer hold every tap for the full timeout.
func (b *Backend) Settle(ctx context.Context, id string) (bool, error) {
	d, err := b.target(ctx, id)
	if err != nil {
		return false, err
	}
	var r struct {
		Settled bool `json:"settled"`
	}
	err = b.call(ctx, "settle", map[string]any{"udid": d.UDID, "quietMs": 48, "maxMs": 1500, "big": true}, &r)
	return r.Settled, err
}

// WaitFrame blocks until the screen shows a frame newer than mark and has then had no big
// change for 16 ms (at most max); mark 0 returns the current frame counter at once.
func (b *Backend) WaitFrame(ctx context.Context, id string, mark uint64, max time.Duration) (uint64, error) {
	d, err := b.target(ctx, id)
	if err != nil {
		return 0, err
	}
	var r struct {
		Mark uint64 `json:"mark"`
	}
	req := map[string]any{"udid": d.UDID, "since": mark, "maxMs": max.Milliseconds(), "quietMs": 16}
	if mark == 0 {
		req["since"] = -1
	}
	err = b.call(ctx, "nextframe", req, &r)
	return r.Mark, err
}

// TapNow taps without the helper's wait for a still screen (the daemon confirmed the target is
// at rest).
func (b *Backend) TapNow(ctx context.Context, id string, x, y int) error {
	d, err := b.target(ctx, id)
	if err != nil {
		return err
	}
	if x < 0 || y < 0 || (d.Width > 0 && (x >= d.Width || y >= d.Height)) {
		return fmt.Errorf("tap (%d,%d) is outside the %dx%d screen", x, y, d.Width, d.Height)
	}
	return b.call(ctx, "tap", map[string]any{"udid": d.UDID, "x": x, "y": y, "settleMaxMs": 0}, nil)
}

// TypeText inserts text at the cursor of the focused field. More than one character is pasted
// (simulator pasteboard + Cmd-V), so the app receives the whole text in one change, the way
// Android's set-text path works. Key events were not reliable in React Native controlled
// inputs (see typeText in the helper): characters were lost or doubled, and a lost character
// in an e-mail field signed in to a different account. TypeKeys sends key events. Both return
// once the app has drawn the change.
func (b *Backend) TypeText(ctx context.Context, id, text string) error {
	d, err := b.target(ctx, id)
	if err != nil {
		return err
	}
	if len([]rune(text)) == 1 && typeable(text) {
		return b.call(ctx, "type", map[string]any{"udid": d.UDID, "text": text}, nil)
	}
	return b.paste(ctx, d.UDID, text)
}

// TypeKeys types one hardware key event per character (US layout, paced); characters the US
// keyboard cannot produce make it paste the whole text instead.
func (b *Backend) TypeKeys(ctx context.Context, id, text string) error {
	d, err := b.target(ctx, id)
	if err != nil {
		return err
	}
	if typeable(text) {
		return b.call(ctx, "type", map[string]any{"udid": d.UDID, "text": text}, nil)
	}
	return b.paste(ctx, d.UDID, text)
}

func (b *Backend) paste(ctx context.Context, udid, text string) error {
	if text == "" {
		return nil
	}
	return pasteVerified(
		func() error { return simctlStdin(ctx, text, "pbcopy", udid) },
		func() error { return b.call(ctx, "key", map[string]any{"udid": udid, "key": "paste"}, nil) },
		func() (string, bool) { return b.focusedFieldValue(ctx, udid) },
		func() { log.Printf("ios type on %s: the focused field did not change; pasting again", udid) },
	)
}

// pasteVerified sets the pasteboard (setPasteboard), pastes (paste) and checks that the
// focused field changed, using read (ok=false: no literal text field in focus). The field is
// read before the paste, while the pasteboard is set, so a lost paste can be told apart from
// one the field transformed (a formatted phone number, upper-casing): only a field that did
// not change at all gets a second paste. The first read finishes before the paste is sent, or
// a late read could see the pasted text and trigger a duplicate.
func pasteVerified(setPasteboard, paste func() error, read func() (string, bool), retrying func()) error {
	type fieldState struct {
		value string
		ok    bool
	}
	before := make(chan fieldState, 1)
	go func() {
		v, ok := read()
		before <- fieldState{v, ok}
	}()
	if err := setPasteboard(); err != nil {
		<-before
		return err
	}
	pre := <-before
	if err := paste(); err != nil {
		return err
	}
	if !pre.ok {
		return nil
	}
	for _, pause := range []time.Duration{0, 150 * time.Millisecond} {
		time.Sleep(pause)
		if v, ok := read(); !ok || v != pre.value {
			return nil // the field changed: the paste arrived (possibly transformed)
		}
	}
	retrying()
	return paste()
}

// focusedFieldValue returns the text of the focused text field. ok is false when there is no
// focused text field, or when its value is not its literal text (a secure field).
func (b *Backend) focusedFieldValue(ctx context.Context, udid string) (value string, ok bool) {
	tctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	t, err := b.readTree(tctx, udid)
	if err != nil {
		return "", false
	}
	found := false
	convertTree(t, "").Walk(func(e *api.Element) {
		if found || !e.Focused || e.Role != "textfield" {
			return
		}
		found = true
		if strings.Contains(strings.ToLower(e.Type), "secure") {
			return
		}
		value, ok = e.Text+"\x00"+e.Value, true
	})
	return value, ok
}

// Key presses a named key. iOS has no back key: "back" taps the navigation bar's back
// button (UIKit gives it the identifier "BackButton") when the screen has one, and otherwise
// swipes from the left edge (the interactive pop gesture). The tap is the robust path: a
// gesture's timing can stretch on a loaded host and stop being recognized as a swipe.
func (b *Backend) Key(ctx context.Context, id, key string) error {
	d, err := b.target(ctx, id)
	if err != nil {
		return err
	}
	if key == "back" {
		tctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		t, err := b.readTree(tctx, d.UDID)
		cancel()
		if err == nil {
			tree := convertTree(t, "")
			var back *api.Element
			tree.Walk(func(e *api.Element) {
				if back == nil && e.ID == "BackButton" && e.Enabled && e.Frame.W > 0 && e.Frame.H > 0 {
					back = e
				}
			})
			if back != nil {
				x, y := back.Frame.Center()
				return b.call(ctx, "tap", map[string]any{"udid": d.UDID, "x": x, "y": y}, nil)
			}
		}
	}
	return b.call(ctx, "key", map[string]any{"udid": d.UDID, "key": key}, nil)
}

func (b *Backend) Install(ctx context.Context, id, path string) error {
	d, err := b.target(ctx, id)
	if err != nil {
		return err
	}
	if !strings.HasSuffix(strings.TrimSuffix(path, "/"), ".app") {
		return fmt.Errorf("install needs a simulator .app bundle, got %s", path)
	}
	return b.call(ctx, "install", map[string]any{"udid": d.UDID, "path": path}, nil)
}

// Launch starts the app (or brings it to the foreground) and returns once its process is
// running, like `simctl launch`. Use `wait` for the first screen.
func (b *Backend) Launch(ctx context.Context, id, app string) error {
	return b.LaunchWith(ctx, id, app, backend.LaunchOptions{})
}

// LaunchWith launches app with process arguments and environment, terminating a running
// instance first when o.Relaunch is set.
func (b *Backend) LaunchWith(ctx context.Context, id, app string, o backend.LaunchOptions) error {
	d, err := b.target(ctx, id)
	if err != nil {
		return err
	}
	// A device this daemon has not booted gets AGEMU_IOS_ANIMATIONS before its first launch.
	b.animateDevice(ctx, d, false)
	var r struct {
		Pid int `json:"pid"`
	}
	// Right after the app was terminated (reset, terminate) its process may still be exiting,
	// and CoreSimulator then answers "failed to launch": retry twice.
	var lerr error
	for attempt, wait := range []time.Duration{0, 300 * time.Millisecond, 700 * time.Millisecond} {
		if wait > 0 {
			time.Sleep(wait)
		}
		req := map[string]any{"udid": d.UDID, "bundleId": app}
		if len(o.Args) > 0 {
			req["args"] = o.Args
		}
		if len(o.Env) > 0 {
			req["env"] = o.Env
		}
		if o.Relaunch && attempt == 0 {
			req["terminateExisting"] = true
		}
		if lerr = b.call(ctx, "launch", req, &r); lerr == nil {
			if attempt > 0 {
				log.Printf("ios launch %s on %s: succeeded on attempt %d", app, d.UDID, attempt+1)
			}
			break
		}
		if !strings.Contains(lerr.Error(), "failed to launch") || !b.installed(ctx, d.UDID, app) {
			return lerr
		}
	}
	if lerr != nil {
		return lerr
	}
	b.mu.Lock()
	if b.apps[d.UDID] == nil {
		b.apps[d.UDID] = map[int]string{}
	}
	b.apps[d.UDID][r.Pid] = app
	b.mu.Unlock()
	return nil
}

// installed reports whether app is installed (a built-in app or a user app), so a launch of a
// missing app fails at once instead of being retried.
func (b *Backend) installed(ctx context.Context, udid, app string) bool {
	var user []string
	if err := b.call(ctx, "userapps", map[string]any{"udid": udid}, &user); err == nil {
		for _, u := range user {
			if u == app {
				return true
			}
		}
	}
	d, err := b.describeSim(ctx, udid)
	if err != nil {
		return true
	}
	system, err := b.systemApps(ctx, d)
	if err != nil {
		return true
	}
	for _, s := range system {
		if s == app {
			return true
		}
	}
	return false
}

func (b *Backend) Terminate(ctx context.Context, id, app string) error {
	d, err := b.target(ctx, id)
	if err != nil {
		return err
	}
	return b.call(ctx, "terminate", map[string]any{"udid": d.UDID, "bundleId": app}, nil)
}
