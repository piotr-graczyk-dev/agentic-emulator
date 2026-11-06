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
	"time"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/api"
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/backend"
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/profiles"
)

// Params is the union of all request parameters. Each method reads the fields it needs.
type Params struct {
	Device   string       `json:"device,omitempty"`
	Platform api.Platform `json:"platform,omitempty"`
	Name     string       `json:"name,omitempty"`
	Profile  string       `json:"profile,omitempty"`
	Cold     bool         `json:"cold,omitempty"`

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
}

// Service is safe for concurrent use.
type Service struct {
	backends map[api.Platform]backend.Backend
	tmpDir   string
	mu       sync.Mutex
	locks    map[string]*sync.Mutex
}

func NewService(tmpDir string, bs ...backend.Backend) *Service {
	s := &Service{backends: map[api.Platform]backend.Backend{}, tmpDir: tmpDir, locks: map[string]*sync.Mutex{}}
	for _, b := range bs {
		s.backends[b.Platform()] = b
	}
	return s
}

func (s *Service) Close() {
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
		return ref, nil
	}
	devs, err := s.listAll(ctx)
	if err != nil {
		return "", err
	}
	if ref == "" {
		var booted []string
		for _, d := range devs {
			if d.State == "booted" {
				booted = append(booted, d.ID)
			}
		}
		switch len(booted) {
		case 1:
			return booted[0], nil
		case 0:
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

// Call dispatches one method.
func (s *Service) Call(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	var p Params
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, fmt.Errorf("bad params: %w", err)
		}
	}
	if p.Selector != nil && p.Selector.Index < 0 {
		return nil, fmt.Errorf("selector index must be >= 0, got %d", p.Selector.Index)
	}
	switch method {
	case "ping":
		return map[string]any{"ok": true, "pid": os.Getpid()}, nil
	case "devices":
		return s.listAll(ctx)
	case "profiles":
		return profiles.All(p.Platform), nil
	case "create":
		if p.Platform == "" {
			return nil, fmt.Errorf("create needs platform (android|ios)")
		}
		b, ok := s.backends[p.Platform]
		if !ok {
			return nil, fmt.Errorf("platform %q not available", p.Platform)
		}
		return b.Create(ctx, api.CreateRequest{Platform: p.Platform, Name: p.Name, Profile: p.Profile})
	}

	// Everything below targets one device.
	id, err := s.resolve(ctx, p.Device)
	if err != nil {
		return nil, err
	}
	b, err := s.backendFor(id)
	if err != nil {
		return nil, err
	}

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
	}

	l := s.deviceLock(id)
	l.Lock()
	defer l.Unlock()

	res, err := s.act(ctx, b, id, method, p)
	compound := p.WaitFor != nil && !p.WaitFor.Empty()
	if err != nil || (!compound && !p.ReturnUI) {
		return res, err
	}
	// The action succeeded; wait for its effect and/or read the screen it ends on in the same
	// call (one round trip, and one agent tool call, per step).
	out := map[string]any{"result": res}
	if compound {
		waited, werr := s.wait(ctx, b, id, Params{Selector: p.WaitFor, Gone: p.WaitGone, TimeoutMs: p.WaitTimeoutMs})
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
	var err error

	switch method {
	case "boot":
		return b.Boot(ctx, id, api.BootOptions{Cold: p.Cold})
	case "shutdown":
		return ok(b.Shutdown(ctx, id))
	case "reset":
		return ok(b.Reset(ctx, id))
	case "set_profile":
		var prof api.Profile
		if p.Profile != "" {
			prof, err = profiles.Get(p.Profile)
			if err != nil {
				return nil, err
			}
		} else if p.Width > 0 && p.Height > 0 && p.Density > 0 {
			prof = profiles.Custom(p.Width, p.Height, p.Density)
		} else {
			return nil, fmt.Errorf("set_profile needs profile or width+height+density")
		}
		if prof.Platform != b.Platform() {
			return nil, fmt.Errorf("profile %s is for %s, device is %s", prof.Name, prof.Platform, b.Platform())
		}
		return b.SetProfile(ctx, id, prof)
	case "tap":
		x, y, el, atRest, err := s.point(ctx, b, id, p)
		if err != nil {
			return nil, err
		}
		if st, ok := b.(backend.Settler); ok && atRest {
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
	return nil, fmt.Errorf("unknown method %q", method)
}

// keyTyper is implemented by backends whose TypeText does not send key events (Android sets
// the field's text); TypeKeys types one key event per character instead.
type keyTyper interface {
	TypeKeys(ctx context.Context, id, text string) error
}

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
// element was confirmed at rest (by a still screen or by two equal reads of its frame), so the
// tap needs no further wait for a still screen.
func (s *Service) point(ctx context.Context, b backend.Backend, id string, p Params) (x, y int, el *api.Element, atRest bool, err error) {
	if p.X != nil && p.Y != nil {
		return *p.X, *p.Y, nil, false, nil
	}
	if p.Selector == nil || p.Selector.Empty() {
		return 0, 0, nil, false, fmt.Errorf("tap needs x,y or a selector")
	}
	// A frame read while the content still scrolls is stale by the time the tap lands (a
	// "log out" button tapped right after a swipe was hit 200 px above its resting place).
	st, settler := b.(backend.Settler)
	still := false
	if settler {
		still, _ = st.Settle(ctx, id) // best effort: an unsettled screen still gets its tap
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
	if el, err = find(); err != nil {
		return 0, 0, nil, false, err
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
			if again, ferr := find(); ferr == nil {
				el = again
			}
		}
	}
	atRest = settler && still
	if settler && !still {
		// Something on screen keeps changing (a spinner, a blinking cursor): waiting for a
		// still screen would only time out. The element is at rest once its frame repeats.
		deadline := time.Now().Add(restCheckFor)
		for time.Now().Before(deadline) {
			again, ferr := find()
			if ferr != nil {
				break
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
			if mark, ferr = fw.WaitFrame(ctx, id, 0, 0); ferr != nil || mark == 0 {
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
		rctx, cancel := context.WithDeadline(ctx, rdl)
		t, err := b.UITree(rctx, id)
		cancel()
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
			if _, ferr := fw.WaitFrame(ctx, id, mark, 250*time.Millisecond); ferr == nil {
				continue
			}
			frames = false
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
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
