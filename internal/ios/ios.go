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
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/api"
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
	// apps maps udid -> pid -> bundle id, filled by Launch and by `launchctl list`.
	apps map[string]map[int]string
	// springboard caches SpringBoard's pid per udid.
	springboard map[string]int
}

// New is cheap: the helper process starts on first use.
func New() *Backend {
	return &Backend{lastUse: map[string]time.Time{}, apps: map[string]map[int]string{}, springboard: map[string]int{}}
}

func (b *Backend) Platform() api.Platform { return api.IOS }

func (b *Backend) helper() (*helper, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
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
	h := b.h
	b.h = nil
	b.mu.Unlock()
	if h != nil {
		h.close()
	}
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
	}
	if err := b.bootReady(ctx, d.UDID); err != nil {
		return api.Device{}, err
	}
	if err := b.enforceMaxBooted(ctx, d.UDID); err != nil {
		return api.Device{}, err
	}
	return b.describe(ctx, d.UDID)
}

func (b *Backend) describe(ctx context.Context, udid string) (api.Device, error) {
	var d simDevice
	if err := b.call(ctx, "describe", map[string]any{"udid": udid}, &d); err != nil {
		return api.Device{}, err
	}
	return d.api(), nil
}

func (b *Backend) bootReady(ctx context.Context, udid string) error {
	var d simDevice
	if err := b.call(ctx, "boot", map[string]any{"udid": udid}, &d); err != nil {
		return err
	}
	timeout := 180 * time.Second
	if dl, ok := ctx.Deadline(); ok {
		timeout = time.Until(dl)
	}
	if err := b.call(ctx, "ready", map[string]any{"udid": udid, "timeoutMs": timeout.Milliseconds()}, nil); err != nil {
		return err
	}
	b.mu.Lock()
	delete(b.springboard, udid)
	b.mu.Unlock()
	return nil
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
			delete(b.springboard, udid)
			b.mu.Unlock()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("simulator %s still %s after shutdown", udid, d.State)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// maxBooted is how many agemu simulators may run at once (AGEMU_IOS_MAX_BOOTED, default 2).
// A booted simulator costs 1-2 GB of disk and memory.
func maxBooted() int {
	var n int
	if _, err := fmt.Sscanf(os.Getenv("AGEMU_IOS_MAX_BOOTED"), "%d", &n); err == nil && n > 0 {
		return n
	}
	return 2
}

// enforceMaxBooted shuts down the least recently used booted agemu simulators (never keep,
// never a non-agemu simulator) until at most maxBooted() are booted.
func (b *Backend) enforceMaxBooted(ctx context.Context, keep string) error {
	ds, err := b.listSims(ctx)
	if err != nil {
		return err
	}
	var booted []simDevice
	for _, d := range ds {
		if managed(d.Name) && (d.StateCode == stateBooted || d.StateCode == stateBooting) {
			booted = append(booted, d)
		}
	}
	limit := maxBooted()
	if len(booted) <= limit {
		return nil
	}
	b.mu.Lock()
	sort.Slice(booted, func(i, j int) bool { return b.lastUse[booted[i].UDID].Before(b.lastUse[booted[j].UDID]) })
	b.mu.Unlock()
	count := len(booted)
	for _, d := range booted {
		if count <= limit {
			break
		}
		if d.UDID == keep {
			continue
		}
		if err := b.shutdown(ctx, d.UDID); err != nil {
			return fmt.Errorf("shutting down %s to stay within AGEMU_IOS_MAX_BOOTED=%d: %w", d.Name, limit, err)
		}
		count--
	}
	return nil
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
	udid := cur.UDID
	if cur.DeviceType != p.DeviceType {
		d, err := b.findOrCreateForProfile(ctx, p)
		if err != nil {
			return api.Device{}, err
		}
		udid = d.UDID
	}
	b.mu.Lock()
	b.lastUse[udid] = time.Now()
	b.mu.Unlock()
	if err := b.bootReady(ctx, udid); err != nil {
		return api.Device{}, err
	}
	if err := b.enforceMaxBooted(ctx, udid); err != nil {
		return api.Device{}, err
	}
	return b.describe(ctx, udid)
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
	var t axTree
	if err := b.call(ctx, "ui", map[string]any{"udid": d.UDID}, &t); err != nil {
		return nil, err
	}
	return convertTree(&t, b.appForPid(ctx, d.UDID, t.Pid)), nil
}

// appForPid names the frontmost app. Pids of apps launched through agemu are known; others
// are looked up once with `launchctl list` inside the simulator.
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

// TypeText types into the focused field with HID keyboard events. Text the US keyboard
// cannot produce (emoji, accents, other scripts) is pasted: put on the simulator pasteboard
// and inserted with Cmd-V.
func (b *Backend) TypeText(ctx context.Context, id, text string) error {
	d, err := b.target(ctx, id)
	if err != nil {
		return err
	}
	if typeable(text) {
		return b.call(ctx, "type", map[string]any{"udid": d.UDID, "text": text}, nil)
	}
	if err := simctlStdin(ctx, text, "pbcopy", d.UDID); err != nil {
		return err
	}
	return b.call(ctx, "key", map[string]any{"udid": d.UDID, "key": "paste"}, nil)
}

// Key presses a named key. "back" is a left-edge swipe (the navigation pop gesture), since
// iOS has no back key.
func (b *Backend) Key(ctx context.Context, id, key string) error {
	d, err := b.target(ctx, id)
	if err != nil {
		return err
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
	d, err := b.target(ctx, id)
	if err != nil {
		return err
	}
	var r struct {
		Pid int `json:"pid"`
	}
	if err := b.call(ctx, "launch", map[string]any{"udid": d.UDID, "bundleId": app}, &r); err != nil {
		return err
	}
	b.mu.Lock()
	if b.apps[d.UDID] == nil {
		b.apps[d.UDID] = map[int]string{}
	}
	b.apps[d.UDID][r.Pid] = app
	b.mu.Unlock()
	return nil
}

func (b *Backend) Terminate(ctx context.Context, id, app string) error {
	d, err := b.target(ctx, id)
	if err != nil {
		return err
	}
	return b.call(ctx, "terminate", map[string]any{"udid": d.UDID, "bundleId": app}, nil)
}
