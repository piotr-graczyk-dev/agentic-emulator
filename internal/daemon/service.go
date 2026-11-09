// Package daemon implements the agemu service: it routes platform-neutral
// requests to the right backend and adds agent conveniences (selectors, waits).
package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/api"
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/backend"
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/profiles"
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/toolchain"
)

// Params is the union of all request parameters. Each method reads the fields it needs.
type Params struct {
	Device   string       `json:"device,omitempty"`
	Platform api.Platform `json:"platform,omitempty"`
	Name     string       `json:"name,omitempty"`
	Profile  string       `json:"profile,omitempty"`
	Cold     bool         `json:"cold,omitempty"`
	NoWait   bool         `json:"noWait,omitempty"` // boot: return once it is preparing (api.BootOptions)

	// set_profile custom size (Android)
	Width   int `json:"width,omitempty"`
	Height  int `json:"height,omitempty"`
	Density int `json:"density,omitempty"`

	// screenshot
	Path   string  `json:"path,omitempty"`
	Format string  `json:"format,omitempty"`
	Scale  float64 `json:"scale,omitempty"`

	// input
	X          *int   `json:"x,omitempty"`
	Y          *int   `json:"y,omitempty"`
	X2         int    `json:"x2,omitempty"`
	Y2         int    `json:"y2,omitempty"`
	DurationMs int    `json:"durationMs,omitempty"`
	Text       string `json:"text,omitempty"`
	Key        string `json:"key,omitempty"`
	// Keys types with one key event per character (the IME path) instead of the platform's
	// fast text entry, on backends that have both (Android).
	Keys bool `json:"keys,omitempty"`

	// apps
	App      string            `json:"app,omitempty"`
	Args     []string          `json:"args,omitempty"`     // launch: process arguments
	Env      map[string]string `json:"env,omitempty"`      // launch: process environment
	Relaunch bool              `json:"relaunch,omitempty"` // launch: terminate a running instance first

	// actions (tap, swipe, type, key, launch) can wait for the next screen in the same call
	WaitFor       *api.Selector `json:"waitFor,omitempty"`
	WaitGone      bool          `json:"waitGone,omitempty"`
	WaitTimeoutMs int           `json:"waitTimeoutMs,omitempty"`
	// ReturnUI makes an action return the UI tree of the screen it ends on.
	ReturnUI bool `json:"returnUi,omitempty"`

	// selectors / waits
	Selector  *api.Selector `json:"selector,omitempty"`
	TimeoutMs int           `json:"timeoutMs,omitempty"`
	Gone      bool          `json:"gone,omitempty"`

	// test capabilities (capabilities.go, docs/CAPABILITIES.md); App and Text are shared
	URL         string          `json:"url,omitempty"`         // open_url
	Action      string          `json:"action,omitempty"`      // permission: grant|revoke|reset; location: set|clear; clipboard: get|set
	Permissions []string        `json:"permissions,omitempty"` // permission: neutral or raw names
	Latitude    *float64        `json:"latitude,omitempty"`    // location set
	Longitude   *float64        `json:"longitude,omitempty"`   // location set
	Mode        string          `json:"mode,omitempty"`        // appearance: light|dark; empty reads it
	Payload     json.RawMessage `json:"payload,omitempty"`     // push: JSON object with "aps"

	// logs and crashes (logs.go, docs/CAPABILITIES.md); App is shared
	Since    string `json:"since,omitempty"`    // a time, a duration back from now, "launch" or a logs cursor
	Level    string `json:"level,omitempty"`    // logs: minimum level
	Contains string `json:"contains,omitempty"` // logs: case-insensitive substring of the tag or the message
	Regex    string `json:"regex,omitempty"`    // logs: RE2 pattern matched against the tag or the message
	Limit    int    `json:"limit,omitempty"`    // logs, crashes: at most this many, the newest
	MaxBytes int    `json:"maxBytes,omitempty"` // logs: at most about this many bytes of lines

	start time.Time // when the call arrived (set by the daemon)
}

// Service is safe for concurrent use.
type Service struct {
	backends map[api.Platform]backend.Backend
	tmpDir   string
	mu       sync.Mutex
	locks    map[string]*sync.Mutex
	states   map[string]*devState // logs, crashes and the crash notice (logs.go)
	// ap is the auto-parker (autopark.go; nil when off); activity counts each device's calls for it.
	ap       *autoParker
	actMu    sync.Mutex
	activity map[string]*devActivity
	// switching counts the profile switches in flight per platform (autopark.go).
	switching map[api.Platform]int
}

func NewService(tmpDir string, bs ...backend.Backend) *Service {
	s := &Service{backends: map[api.Platform]backend.Backend{}, tmpDir: tmpDir, locks: map[string]*sync.Mutex{}, states: map[string]*devState{}, activity: map[string]*devActivity{}, switching: map[api.Platform]int{}}
	for _, b := range bs {
		s.backends[b.Platform()] = b
	}
	if d := autoParkFromEnv(); d > 0 {
		s.startAutoPark(d, autoParkTick(d))
	}
	return s
}

// Close stops the auto-parker and resumes the devices it parked, then closes the backends.
func (s *Service) Close() {
	s.stopAutoPark()
	for _, b := range s.backends {
		_ = b.Close()
	}
}

// deviceLock serializes mutating operations on a single device so concurrent
// agents cannot interleave half-finished gestures.
func (s *Service) deviceLock(id string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.locks[id]
	if !ok {
		l = &sync.Mutex{}
		s.locks[id] = l
	}
	return l
}

func (s *Service) backendFor(id string) (backend.Backend, error) {
	plat, _, ok := strings.Cut(id, ":")
	if !ok {
		return nil, fmt.Errorf("device id %q has no platform prefix", id)
	}
	b, ok := s.backends[api.Platform(plat)]
	if !ok {
		return nil, fmt.Errorf("platform %q not available", plat)
	}
	return b, nil
}

func (s *Service) listAll(ctx context.Context) ([]api.Device, error) {
	all := []api.Device{}
	var errs []string
	for _, p := range []api.Platform{api.Android, api.IOS} {
		b, ok := s.backends[p]
		if !ok {
			continue
		}
		ds, err := b.List(ctx)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", p, err))
			continue
		}
		all = append(all, ds...)
	}
	if len(all) == 0 && len(errs) > 0 {
		return nil, fmt.Errorf("listing devices: %s", strings.Join(errs, "; "))
	}
	return all, nil
}

// resolve turns a user-supplied device reference (full id, bare name, or empty)
// into a full device id. Empty means "the only booted device".
func (s *Service) resolve(ctx context.Context, ref string) (string, error) {
	if strings.HasPrefix(ref, string(api.Android)+":") || strings.HasPrefix(ref, string(api.IOS)+":") {
		return s.canonical(ref), nil
	}
	devs, err := s.listAll(ctx)
	if err != nil {
		return "", err
	}
	if ref == "" {
		var booted []string
		for _, d := range devs {
			// A parked device is booted, stopped in place: the call resumes it (see wake).
			if d.State == "booted" || d.State == api.StateParked {
				booted = append(booted, d.ID)
			}
		}
		switch len(booted) {
		case 1:
			return booted[0], nil
		case 0:
			// A device that is preparing its baseline is the one meant: the call then fails with
			// the preparing error (poll devices) instead of "no booted device".
			var preparing []string
			for _, d := range devs {
				if d.State == api.StatePreparing {
					preparing = append(preparing, d.ID)
				}
			}
			if len(preparing) == 1 {
				return preparing[0], nil
			}
			if len(preparing) > 1 {
				return "", fmt.Errorf("no booted device; %s are preparing their baselines (poll `devices`)", strings.Join(preparing, ", "))
			}
			return "", fmt.Errorf("no booted device; boot one with `agemu boot <device>`")
		default:
			return "", fmt.Errorf("several devices are booted, pass one with -d: %s", strings.Join(booted, ", "))
		}
	}
	for _, d := range devs {
		if d.Name == ref || strings.TrimPrefix(d.ID, string(d.Platform)+":") == ref {
			return d.ID, nil
		}
	}
	return "", fmt.Errorf("unknown device %q", ref)
}

// canonical returns the one spelling of a device id its backend lists (backend.IDNormalizer),
// so that every key the daemon keeps per device (device lock, logs state, auto-park activity)
// and every id it passes to a backend is the same for one device.
func (s *Service) canonical(id string) string {
	b, err := s.backendFor(id)
	if err != nil {
		return id
	}
	if n, ok := b.(backend.IDNormalizer); ok {
		return n.CanonicalID(id)
	}
	return id
}

// Call dispatches one method. It drops the crash notice; Handle returns it.
func (s *Service) Call(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	res, _, err := s.Handle(ctx, method, raw)
	return res, err
}

// Handle dispatches one method. crash, when set, tells the agent that the app it launched
// through agemu crashed (logs.go): once in a result, and in every error while the app is down.
func (s *Service) Handle(ctx context.Context, method string, raw json.RawMessage) (res any, crash *api.AppCrash, err error) {
	start := time.Now()
	var p Params
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, nil, fmt.Errorf("bad params: %w", err)
		}
	}
	p.start = start
	if p.Selector != nil && p.Selector.Index < 0 {
		return nil, nil, fmt.Errorf("selector index must be >= 0, got %d", p.Selector.Index)
	}
	switch method {
	case "ping":
		ping := Ping{OK: true, PID: os.Getpid(), Version: Version, Exe: exePath, Started: started, AndroidSDK: toolchain.AndroidSDK()}
		for _, plat := range []api.Platform{api.Android, api.IOS} {
			b, ok := s.backends[plat]
			if !ok {
				continue
			}
			ping.Platforms = append(ping.Platforms, plat)
			if pl, ok := b.(backend.PreparingLister); ok {
				ping.Preparing = append(ping.Preparing, pl.PreparingIDs()...)
			}
		}
		return ping, nil, nil
	case "devices":
		res, err = s.listAll(ctx)
		return res, nil, err
	case "profiles":
		return profiles.All(p.Platform), nil, nil
	case "create":
		if p.Platform == "" {
			return nil, nil, fmt.Errorf("create needs platform (android|ios)")
		}
		b, ok := s.backends[p.Platform]
		if !ok {
			return nil, nil, fmt.Errorf("platform %q not available", p.Platform)
		}
		res, err = b.Create(ctx, api.CreateRequest{Platform: p.Platform, Name: p.Name, Profile: p.Profile})
		return res, nil, err
	}

	// Everything below targets one device.
	id, err := s.resolve(ctx, p.Device)
	if err != nil {
		return nil, nil, err
	}
	b, err := s.backendFor(id)
	if err != nil {
		return nil, nil, err
	}
	// The call counts as activity on the device until it returns (autopark.go): no auto-park
	// while it runs, and a park in progress finishes before the wake below.
	if err := s.beginCall(ctx, id, method); err != nil {
		return nil, nil, err
	}
	defer s.endCall(id, method)
	// Arguments are checked before the device lock and the backend: a bad call fails at once.
	if err := checkCapability(method, &p); err != nil {
		return nil, nil, err
	}
	if err := s.checkObserve(id, method, &p); err != nil {
		return nil, nil, err
	}
	// A device preparing its baseline answers at once (before any resume: a preparing device is
	// never parked, and the call must not wait behind the preparation).
	if err := checkPreparing(ctx, b, id, method); err != nil {
		return nil, nil, err
	}
	if method == "set_profile" {
		if _, err := switchProfile(b.Platform(), p); err != nil {
			return nil, nil, err
		}
		// An iOS profile switch moves to another simulator, which may be one this daemon
		// auto-parked: resume those first. An Android switch stays on this AVD, which wake
		// below resumes.
		if b.Platform() == api.IOS {
			if err := s.resumeAutoParked(ctx, api.IOS); err != nil {
				return nil, nil, err
			}
		}
	}
	if err := s.wake(ctx, b, id, method); err != nil {
		return nil, nil, err
	}

	res, err = s.onDevice(ctx, b, id, method, p)
	if d, isDev := res.(api.Device); isDev && err == nil && d.ID != id {
		s.touch(d.ID) // an iOS profile switch moves to another simulator
	}
	switch method {
	case "boot", "shutdown", "reset", "crashes", "park": // they end the watch or report crashes themselves; a parked app reports nothing
		return res, nil, err
	}
	return s.report(ctx, b, id, res, err)
}

// checkPreparing answers a call on a device that is preparing its baseline at once, instead of
// letting it wait for minutes behind the boot (backend.Preparer). Boot (which returns the state or
// waits), shutdown (which cancels the preparation) and the reads of what was captured go through.
func checkPreparing(ctx context.Context, b backend.Backend, id, method string) error {
	pr, ok := b.(backend.Preparer)
	if !ok {
		return nil
	}
	switch method {
	case "boot", "shutdown", "logs", "crashes":
		return nil
	}
	p := pr.AwaitDecision(ctx, id)
	if p == nil {
		return nil
	}
	return &api.PreparingError{Device: id, Phase: p.Phase, Elapsed: p.ElapsedS, MaxS: p.MaxS}
}

// preparingNow is checkPreparing without the wait (after the device lock: a boot that held it
// may have become a preparation meanwhile).
func preparingNow(b backend.Backend, id string) error {
	pr, ok := b.(backend.Preparer)
	if !ok {
		return nil
	}
	if p := pr.Preparing(id); p != nil {
		return &api.PreparingError{Device: id, Phase: p.Phase, Elapsed: p.ElapsedS, MaxS: p.MaxS}
	}
	return nil
}

// wake resumes a parked device (backend.Parker) before a call that needs it running: every call
// on a device but park and resume themselves and the reads of what was captured (logs,
// crashes). An agent's next call on a device parked while it was idle just works, the first one
// taking the resume's time.
func (s *Service) wake(ctx context.Context, b backend.Backend, id, method string) error {
	pk, ok := b.(backend.Parker)
	if !ok {
		return nil
	}
	switch method {
	case "park", "resume", "logs", "crashes":
		return nil
	}
	if !pk.Parked(id) {
		return nil
	}
	l := s.deviceLock(id)
	l.Lock()
	defer l.Unlock()
	if _, err := pk.Resume(ctx, id); err != nil {
		return fmt.Errorf("resuming the parked device before %s: %w", method, err)
	}
	s.disown(id)
	return nil
}

// switchProfile is the profile a set_profile call asks for, checked against the device's platform.
func switchProfile(plat api.Platform, p Params) (api.Profile, error) {
	var prof api.Profile
	switch {
	case p.Profile != "":
		var err error
		if prof, err = profiles.Get(p.Profile); err != nil {
			return api.Profile{}, err
		}
	case p.Width > 0 && p.Height > 0 && p.Density > 0:
		prof = profiles.Custom(p.Width, p.Height, p.Density)
	default:
		return api.Profile{}, fmt.Errorf("set_profile needs profile or width+height+density")
	}
	if prof.Platform != plat {
		return api.Profile{}, fmt.Errorf("profile %s is for %s, device is %s", prof.Name, prof.Platform, plat)
	}
	return prof, nil
}

// onDevice runs one method on a resolved device whose arguments were checked.
func (s *Service) onDevice(ctx context.Context, b backend.Backend, id, method string, p Params) (any, error) {
	switch method {
	case "ui":
		return b.UITree(ctx, id)
	case "find":
		if p.Selector == nil || p.Selector.Empty() {
			return nil, fmt.Errorf("find needs a selector")
		}
		t, err := b.UITree(ctx, id)
		if err != nil {
			return nil, err
		}
		return Find(t, *p.Selector), nil
	case "screenshot":
		opts := api.ScreenshotOptions{Path: p.Path, Format: p.Format, Scale: p.Scale}
		if opts.Format == "" {
			opts.Format = "png"
		}
		if opts.Path == "" {
			ext := opts.Format
			if ext == "jpeg" {
				ext = "jpg"
			}
			opts.Path = filepath.Join(s.tmpDir, fmt.Sprintf("shot-%d.%s", time.Now().UnixNano(), ext))
		}
		return b.Screenshot(ctx, id, opts)
	case "wait":
		return s.wait(ctx, b, id, p)
	case "logs": // reads of what the backend captured: no device lock
		return s.logs(ctx, b, id, p)
	case "crashes":
		return s.crashes(ctx, b, id, p)
	}

	if method == "shutdown" {
		// A boot that is preparing a baseline can hold the device lock for minutes (a boot
		// without NoWait); the backend cancels the preparation and orders the rest itself.
		if pr, ok := b.(backend.Preparer); ok && pr.Preparing(id) != nil {
			return ok2(b.Shutdown(ctx, id))
		}
	}
	l := s.deviceLock(id)
	l.Lock()
	defer l.Unlock()
	if method != "boot" && method != "shutdown" {
		if err := preparingNow(b, id); err != nil {
			return nil, err
		}
	}

	if method == "launch" {
		s.launching(id, appOf(p.App), p.start) // a crash during the launch counts
	}
	res, err := s.act(ctx, b, id, method, p)
	if err == nil {
		s.track(id, method, p, res)
	}
	compound := p.WaitFor != nil && !p.WaitFor.Empty()
	if err != nil || (!compound && !p.ReturnUI) {
		return res, err
	}
	// The action succeeded; wait for its effect and/or read the screen it ends on in the same
	// call (one round trip, and one agent tool call, per step).
	out := map[string]any{"result": res}
	if compound {
		waited, werr := s.wait(ctx, b, id, Params{Selector: p.WaitFor, Gone: p.WaitGone, TimeoutMs: p.WaitTimeoutMs, start: p.start})
		if werr != nil {
			return nil, fmt.Errorf("%s done, but: %w", method, werr)
		}
		out["waited"] = waited
	}
	if p.ReturnUI {
		t, terr := b.UITree(ctx, id)
		if terr != nil {
			return nil, fmt.Errorf("%s done, but reading the screen failed: %w", method, terr)
		}
		out["ui"] = t
	}
	return out, nil
}

// act runs one device action (the caller holds the device lock).
func (s *Service) act(ctx context.Context, b backend.Backend, id, method string, p Params) (any, error) {
	switch method {
	case "boot":
		return b.Boot(ctx, id, api.BootOptions{Cold: p.Cold, NoWait: p.NoWait})
	case "shutdown":
		return ok(b.Shutdown(ctx, id))
	case "reset":
		return ok(b.Reset(ctx, id))
	case "park", "resume":
		pk, isParker := b.(backend.Parker)
		if !isParker {
			return nil, api.NotImplemented(b.Platform(), method, "this backend cannot park devices")
		}
		if method == "park" {
			return ok(pk.Park(ctx, id))
		}
		resumed, err := pk.Resume(ctx, id)
		if err != nil {
			return nil, err
		}
		return map[string]any{"ok": true, "resumed": resumed}, nil
	case "set_profile":
		prof, err := switchProfile(b.Platform(), p)
		if err != nil {
			return nil, err
		}
		return b.SetProfile(ctx, id, prof)
	case "tap":
		x, y, el, atRest, err := s.point(ctx, b, id, p)
		if err != nil {
			return nil, err
		}
		if et, ok := b.(backend.ElementTapper); ok && el != nil {
			err = et.TapElement(ctx, id, el, atRest)
		} else if st, ok := b.(backend.Settler); ok && atRest {
			err = st.TapNow(ctx, id, x, y)
		} else {
			err = b.Tap(ctx, id, x, y)
		}
		if err != nil {
			return nil, err
		}
		return map[string]any{"x": x, "y": y, "element": el}, nil
	case "swipe":
		if p.X == nil || p.Y == nil {
			return nil, fmt.Errorf("swipe needs x,y,x2,y2")
		}
		d := p.DurationMs
		if d <= 0 {
			d = 300
		}
		return ok(b.Swipe(ctx, id, *p.X, *p.Y, p.X2, p.Y2, d))
	case "type":
		if kt, isKT := b.(keyTyper); isKT && p.Keys {
			return ok(kt.TypeKeys(ctx, id, p.Text))
		}
		return ok(b.TypeText(ctx, id, p.Text))
	case "key":
		return ok(b.Key(ctx, id, strings.ToLower(p.Key)))
	case "install":
		return ok(b.Install(ctx, id, p.Path))
	case "launch":
		if len(p.Args) > 0 || len(p.Env) > 0 || p.Relaunch {
			l, isLauncher := b.(backend.Launcher)
			if !isLauncher {
				return nil, fmt.Errorf("launch arguments, environment and relaunch are not supported on %s yet", b.Platform())
			}
			return ok(l.LaunchWith(ctx, id, p.App, backend.LaunchOptions{Args: p.Args, Env: p.Env, Relaunch: p.Relaunch}))
		}
		return ok(b.Launch(ctx, id, p.App))
	case "terminate":
		return ok(b.Terminate(ctx, id, p.App))
	}
	if run, isCap := capabilities[method]; isCap {
		return run(ctx, b, id, p)
	}
	return nil, fmt.Errorf("unknown method %q", method)
}

// keyTyper is implemented by backends whose TypeText does not send key events (Android sets
// the field's text); TypeKeys types one key event per character instead.
type keyTyper interface {
	TypeKeys(ctx context.Context, id, text string) error
}

func ok2(err error) (any, error) { return ok(err) }

func ok(err error) (any, error) {
	if err != nil {
		return nil, err
	}
	return map[string]any{"ok": true}, nil
}

// restCheckFor bounds how long point re-reads the tree waiting for a moving element to stop;
// enabledWaitFor how long it waits for a disabled element to become enabled.
const (
	restCheckFor   = 1500 * time.Millisecond
	enabledWaitFor = 3 * time.Second
)

// point resolves tap coordinates from explicit x/y or a selector. atRest reports that the
// screen kept changing and the element was confirmed at rest by two equal reads of its frame:
// the tap then goes out at once, since another wait for a still screen would only time out. On
// a still screen the tap keeps the backend's own settle. It returns at once if the screen is
// still still, and it holds the tap if a transition started or resumed during the tree read
// (UIKit ignores a tap during a transition).
func (s *Service) point(ctx context.Context, b backend.Backend, id string, p Params) (x, y int, el *api.Element, atRest bool, err error) {
	if p.X != nil && p.Y != nil {
		return *p.X, *p.Y, nil, false, nil
	}
	if p.Selector == nil || p.Selector.Empty() {
		return 0, 0, nil, false, fmt.Errorf("tap needs x,y or a selector")
	}
	find := func() (*api.Element, error) {
		t, err := b.UITree(ctx, id)
		if err != nil {
			return nil, err
		}
		m := Find(t, *p.Selector)
		if len(m) <= p.Selector.Index {
			return nil, fmt.Errorf("no element matches %s", describe(*p.Selector))
		}
		return m[p.Selector.Index], nil
	}
	// A frame read while the content still scrolls is stale by the time the tap lands (a
	// "log out" button tapped right after a swipe was hit 200 px above its resting place).
	st, settler := b.(backend.Settler)
	still := false
	if ac, ok := b.(backend.Actionability); ok {
		// The backend waits for transitions and for the element to stop moving (bounded; when
		// it gives up, the tap goes to the latest read, as without the wait).
		if el, _, err = ac.AwaitActionable(ctx, id, find); err != nil {
			return 0, 0, nil, false, err
		}
		settler = false
	} else {
		if settler {
			still, _ = st.Settle(ctx, id) // best effort: an unsettled screen still gets its tap
		}
		if el, err = find(); err != nil {
			return 0, 0, nil, false, err
		}
	}
	// Actionability: a disabled element ignores the tap (a "send" button enabled by text the
	// app has not processed yet). Give it a moment to become enabled; tap it anyway after that.
	if !el.Enabled {
		deadline := time.Now().Add(enabledWaitFor)
		for !el.Enabled && time.Now().Before(deadline) {
			select {
			case <-ctx.Done():
				return 0, 0, nil, false, ctx.Err()
			case <-time.After(50 * time.Millisecond):
			}
			again, ferr := find()
			if ferr != nil {
				return 0, 0, nil, false, fmt.Errorf("%w (it disappeared while disabled)", ferr)
			}
			el = again
		}
	}
	if settler && !still {
		// Something on screen keeps changing (a spinner, a blinking cursor): waiting for a
		// still screen would only time out. The element is at rest once its frame repeats.
		deadline := time.Now().Add(restCheckFor)
		for time.Now().Before(deadline) {
			if ctx.Err() != nil {
				return 0, 0, nil, false, ctx.Err()
			}
			again, ferr := find()
			if ferr != nil {
				// The element went away while it moved (a transition replaced the screen):
				// its last frame may now belong to another control, so do not tap it.
				return 0, 0, nil, false, fmt.Errorf("%w (it disappeared while it was moving)", ferr)
			}
			if again.Frame == el.Frame {
				el, atRest = again, true
				break
			}
			el = again
		}
	}
	x, y = el.Frame.Center()
	return x, y, el, atRest, nil
}

func (s *Service) wait(ctx context.Context, b backend.Backend, id string, p Params) (any, error) {
	if p.Selector == nil || p.Selector.Empty() {
		return nil, fmt.Errorf("wait needs a selector")
	}
	timeout := time.Duration(p.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	deadline := time.Now().Add(timeout)
	var lastErr error
	fw, frames := b.(backend.FrameWaiter)
	if os.Getenv("AGEMU_WAIT_MODE") == "poll" { // timer polling only, for comparisons
		frames = false
	}
	var prevMark uint64
	lastChange := time.Now()
	for {
		// The frame mark is taken before the read, so a change that shows during the read
		// triggers the next read at once.
		var mark uint64
		if frames {
			var ferr error
			fctx, cancel := context.WithDeadline(ctx, frameDeadline(deadline))
			mark, ferr = fw.WaitFrame(fctx, id, 0, 0)
			cancel()
			if ferr != nil || mark == 0 {
				frames = false // no frame counter (yet): poll on a timer
			} else if mark != prevMark {
				prevMark, lastChange = mark, time.Now()
			}
		}
		// Each read is bounded by the wait's own deadline (at least 1 s), so a slow device
		// cannot stretch a wait far past its timeout.
		rdl := deadline
		if floor := time.Now().Add(time.Second); rdl.Before(floor) {
			rdl = floor
		}
		// A wait for an element while the launched app is down after a crash the agent has not
		// heard of yet (during this call, or since its last call) would only time out: one more
		// read, bounded short (on Android the first read after a crash waited out the
		// accessibility timeout on the dying app's window, 5 s), then the crash ends the wait. A
		// crash that comes during a read cuts that read short the same way.
		var crashed *api.AppCrash
		if !p.Gone {
			if crashed = s.crashedUntold(ctx, b, id, p.start); crashed != nil {
				rdl = time.Now().Add(crashedRead)
			}
		}
		t, during, err := s.readWatchingCrash(ctx, b, id, p.start, rdl, !p.Gone && crashed == nil)
		if crashed == nil {
			crashed = during
		}
		lastErr = err
		if err == nil {
			m := Find(t, *p.Selector)
			if p.Gone && len(m) == 0 {
				return map[string]any{"ok": true}, nil
			}
			if !p.Gone && len(m) > 0 {
				return m[0], nil
			}
		}
		if !p.Gone && crashed == nil && time.Now().After(deadline) {
			crashed = s.crashedUntold(ctx, b, id, p.start) // a crash during the last read
		}
		if crashed != nil {
			return nil, &api.AppCrashedError{Crash: *crashed, Err: fmt.Errorf("stopped waiting for %s to appear", describe(*p.Selector))}
		}
		if time.Now().After(deadline) {
			state := "appear"
			if p.Gone {
				state = "disappear"
			}
			if lastErr != nil {
				return nil, fmt.Errorf("timeout after %v waiting for %s to %s (last UI read failed: %v)", timeout, describe(*p.Selector), state, lastErr)
			}
			return nil, fmt.Errorf("timeout after %v waiting for %s to %s", timeout, describe(*p.Selector), state)
		}
		// While the screen is changing (and for 600 ms after), poll every 50 ms: the
		// accessibility tree of a React Native app catches up with a frame some time after it
		// is drawn. On a still screen, sleep until the next frame instead (at most 250 ms,
		// since the tree can change without one), which saves reads while waiting for, say,
		// the network.
		if frames && time.Since(lastChange) > 600*time.Millisecond {
			fctx, cancel := context.WithDeadline(ctx, frameDeadline(deadline))
			_, ferr := fw.WaitFrame(fctx, id, mark, 250*time.Millisecond)
			cancel()
			if ferr == nil {
				continue
			}
			frames = false // fall back to timer polling; the wait's own deadline still applies
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// crashedRead bounds the last UI tree read of a wait once the watched app is known to have
// crashed: the element may still show (a system window), but a read held up by the dying app
// must not hold the answer. crashPoll is how often a wait asks the watcher (in memory) while a
// read runs.
const (
	crashedRead = 300 * time.Millisecond
	crashPoll   = 25 * time.Millisecond
)

// readWatchingCrash reads the UI tree within rdl. With watch set it asks the watcher every
// crashPoll while the read runs; once the watched app's untold crash shows, the read gets
// crashedRead more, and the crash is returned with whatever the read gave.
func (s *Service) readWatchingCrash(ctx context.Context, b backend.Backend, id string, start, rdl time.Time, watch bool) (*api.UITree, *api.AppCrash, error) {
	rctx, cancel := context.WithDeadline(ctx, rdl)
	defer cancel()
	if !watch {
		t, err := b.UITree(rctx, id)
		return t, nil, err
	}
	var crashed atomic.Pointer[api.AppCrash]
	done := make(chan struct{})
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		tick := time.NewTicker(crashPoll)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
			}
			if c := s.crashedUntold(rctx, b, id, start); c != nil {
				crashed.Store(c)
				t := time.NewTimer(crashedRead)
				defer t.Stop()
				select {
				case <-done:
				case <-t.C:
					cancel()
				}
				return
			}
		}
	}()
	t, err := b.UITree(rctx, id)
	close(done)
	<-watched
	return t, crashed.Load(), err
}

// frameDeadline bounds one frame request: never past the wait's deadline (plus the 1 s a last
// tree read gets), and never more than 2 s, so a stalled framebuffer cannot hold a wait.
func frameDeadline(waitDeadline time.Time) time.Time {
	d := time.Now().Add(2 * time.Second)
	if limit := waitDeadline.Add(time.Second); limit.Before(d) {
		d = limit
	}
	return d
}

func describe(s api.Selector) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// Find returns elements matching the selector, in document order, without children.
func Find(t *api.UITree, sel api.Selector) []*api.Element {
	var out []*api.Element
	needle := strings.ToLower(sel.Contains)
	t.Walk(func(e *api.Element) {
		if sel.Text != "" && e.Text != sel.Text && e.Label != sel.Text {
			return
		}
		if sel.Contains != "" && !strings.Contains(strings.ToLower(e.Text), needle) && !strings.Contains(strings.ToLower(e.Label), needle) {
			return
		}
		if sel.ID != "" && e.ID != sel.ID && !strings.HasSuffix(e.ID, "/"+sel.ID) {
			return
		}
		if sel.Role != "" && e.Role != sel.Role {
			return
		}
		if e.Frame.W <= 0 || e.Frame.H <= 0 {
			return
		}
		c := *e
		c.Children = nil
		out = append(out, &c)
	})
	return out
}
