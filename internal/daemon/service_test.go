package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/api"
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/backend"
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/osproc"
)

func tree() *api.UITree {
	return &api.UITree{Width: 100, Height: 200, Root: &api.Element{Ref: "e0", Role: "window", Frame: api.Rect{W: 100, H: 200}, Children: []*api.Element{
		{Ref: "e1", Role: "button", Text: "Log in", ID: "com.app:id/login", Frame: api.Rect{X: 10, Y: 20, W: 30, H: 10}, Enabled: true},
		{Ref: "e2", Role: "text", Label: "Welcome back", Frame: api.Rect{X: 0, Y: 0, W: 100, H: 10}},
		{Ref: "e3", Role: "button", Text: "Hidden", Frame: api.Rect{}},
	}}}
}

func TestFind(t *testing.T) {
	cases := []struct {
		sel  api.Selector
		want []string
	}{
		{api.Selector{Text: "Log in"}, []string{"e1"}},
		{api.Selector{Contains: "welcome"}, []string{"e2"}},
		{api.Selector{ID: "login"}, []string{"e1"}},
		{api.Selector{ID: "com.app:id/login"}, []string{"e1"}},
		{api.Selector{Role: "button"}, []string{"e1"}}, // zero-size element is skipped
		{api.Selector{Text: "Hidden"}, nil},
	}
	for _, c := range cases {
		got := Find(tree(), c.sel)
		if len(got) != len(c.want) {
			t.Fatalf("%+v: got %d matches, want %d", c.sel, len(got), len(c.want))
		}
		for i := range got {
			if got[i].Ref != c.want[i] {
				t.Fatalf("%+v: got %s want %s", c.sel, got[i].Ref, c.want[i])
			}
		}
	}
}

func TestCenter(t *testing.T) {
	x, y := api.Rect{X: 10, Y: 20, W: 30, H: 10}.Center()
	if x != 25 || y != 25 {
		t.Fatalf("center = %d,%d", x, y)
	}
}

// fakeBackend answers UITree with tree() and records taps; everything else is unimplemented.
type fakeBackend struct {
	backend.Backend
	taps int
}

func (f *fakeBackend) Platform() api.Platform { return api.Android }
func (f *fakeBackend) List(ctx context.Context) ([]api.Device, error) {
	return []api.Device{{ID: "android:x", Name: "x", Platform: api.Android, State: "booted"}}, nil
}
func (f *fakeBackend) UITree(ctx context.Context, id string) (*api.UITree, error) { return tree(), nil }
func (f *fakeBackend) Tap(ctx context.Context, id string, x, y int) error         { f.taps++; return nil }
func (f *fakeBackend) Swipe(ctx context.Context, id string, x1, y1, x2, y2, ms int) error {
	var frames []int
	return fmt.Errorf("unreachable %d", frames[x1]) // index out of range: a backend bug
}

func TestNegativeSelectorIndexIsRejected(t *testing.T) {
	fb := &fakeBackend{}
	s := NewService(t.TempDir(), fb)
	for _, raw := range []string{
		`{"device":"android:x","selector":{"text":"Log in","index":-1}}`,
		`{"device":"android:x","selector":{"text":"nothing","index":-1}}`,
	} {
		if _, err := s.Call(context.Background(), "tap", json.RawMessage(raw)); err == nil {
			t.Fatalf("%s: want an error", raw)
		}
	}
	if fb.taps != 0 {
		t.Fatalf("tapped %d times", fb.taps)
	}
	res, err := s.Call(context.Background(), "tap", json.RawMessage(`{"device":"android:x","selector":{"text":"Log in"}}`))
	if err != nil || fb.taps != 1 {
		t.Fatalf("valid tap: %v %v taps=%d", res, err, fb.taps)
	}
}

func TestSafeCallRecoversPanics(t *testing.T) {
	s := NewService(t.TempDir(), &fakeBackend{})
	// fakeBackend.Swipe panics like a backend bug would.
	_, _, err := safeCall(context.Background(), s, Request{Method: "swipe", Params: json.RawMessage(`{"device":"android:x","x":1,"y":1,"x2":2,"y2":2}`)})
	if err == nil {
		t.Fatal("want an error from a panicking backend")
	}
}

func TestSecondDaemonRefusesToStart(t *testing.T) {
	// The race: daemon A has started (holds the lock) but has not bound its socket yet, and
	// daemon B finds no socket. B must not bind; before the lock it would have.
	t.Setenv("AGEMU_HOME", shortTempDir(t))
	lock, err := os.OpenFile(filepath.Join(Home(), "agemud.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := osproc.TryLock(lock, true); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := Serve(ctx, NewService(t.TempDir())); err == nil {
		t.Fatal("a second daemon started while another held the lock")
	}
	if _, err := os.Stat(SocketPath()); err == nil {
		t.Fatal("the refused daemon created a socket")
	}

	// Once the lock is free, a daemon starts normally.
	osproc.Unlock(lock)
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, NewService(t.TempDir())) }()
	deadline := time.Now().Add(time.Second)
	for {
		if c, err := net.Dial("unix", SocketPath()); err == nil {
			c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("daemon did not start once the lock was free")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("daemon: %v", err)
	}
}

// shortTempDir keeps the unix socket path under the 104-byte macOS limit.
func shortTempDir(t *testing.T) string {
	root := "/tmp" // unix socket paths are limited to about 100 bytes; $TMPDIR on macOS is long
	if runtime.GOOS == "windows" {
		root = ""
	}
	d, err := os.MkdirTemp(root, "agemu-t")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

// settlingBackend records the order of Settle, UITree and tap calls. still is what Settle
// reports; frames, when set, are the "Log in" button's successive frames (one per tree read).
type settlingBackend struct {
	fakeBackend
	still         bool
	frames        []api.Rect
	disabledReads int // the button reads as disabled for this many tree reads
	goneAfter     int // from this many reads on (when > 0), the button is gone
	reads         int
	calls         []string
}

func (s *settlingBackend) Settle(ctx context.Context, id string) (bool, error) {
	s.calls = append(s.calls, "settle")
	return s.still, nil
}

func (s *settlingBackend) UITree(ctx context.Context, id string) (*api.UITree, error) {
	s.calls = append(s.calls, "ui")
	t := tree()
	if len(s.frames) > 0 {
		f := s.frames[min(s.reads, len(s.frames)-1)]
		t.Root.Children[0].Frame = f
	}
	if s.reads < s.disabledReads {
		t.Root.Children[0].Enabled = false
	}
	if s.goneAfter > 0 && s.reads >= s.goneAfter {
		t.Root.Children = t.Root.Children[1:]
	}
	s.reads++
	return t, nil
}

func (s *settlingBackend) Tap(ctx context.Context, id string, x, y int) error {
	s.calls = append(s.calls, fmt.Sprintf("tap %d,%d", x, y))
	return nil
}

func (s *settlingBackend) TapNow(ctx context.Context, id string, x, y int) error {
	s.calls = append(s.calls, fmt.Sprintf("tapnow %d,%d", x, y))
	return nil
}

func TestTapBySelectorOnAStillScreen(t *testing.T) {
	sb := &settlingBackend{still: true}
	svc := NewService(t.TempDir(), sb)
	if _, err := svc.Call(context.Background(), "tap", json.RawMessage(`{"device":"android:x","selector":{"text":"Log in"}}`)); err != nil {
		t.Fatal(err)
	}
	// The tap keeps the backend's settle: a transition can start or resume during the tree read.
	want := []string{"settle", "ui", "tap 25,25"}
	if fmt.Sprint(sb.calls) != fmt.Sprint(want) {
		t.Fatalf("calls = %v, want %v", sb.calls, want)
	}
	sb.calls = nil
	if _, err := svc.Call(context.Background(), "tap", json.RawMessage(`{"device":"android:x","x":5,"y":5}`)); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(sb.calls) != "[tap 5,5]" {
		t.Fatalf("a tap at coordinates: %v, want only the backend's own tap", sb.calls)
	}
}

func TestTapBySelectorWaitsForTheElementToStopMoving(t *testing.T) {
	// The screen never goes still (say, a spinner), and the button is still scrolling: its frame
	// changes once and then repeats. The tap lands on the resting frame without waiting longer.
	sb := &settlingBackend{still: false, frames: []api.Rect{{X: 10, Y: 300, W: 30, H: 10}, {X: 10, Y: 120, W: 30, H: 10}, {X: 10, Y: 120, W: 30, H: 10}}}
	svc := NewService(t.TempDir(), sb)
	start := time.Now()
	if _, err := svc.Call(context.Background(), "tap", json.RawMessage(`{"device":"android:x","selector":{"text":"Log in"}}`)); err != nil {
		t.Fatal(err)
	}
	want := []string{"settle", "ui", "ui", "ui", "tapnow 25,125"}
	if fmt.Sprint(sb.calls) != fmt.Sprint(want) {
		t.Fatalf("calls = %v, want %v", sb.calls, want)
	}
	if time.Since(start) > 200*time.Millisecond {
		t.Fatalf("took %v", time.Since(start))
	}
}

func TestTapBySelectorWaitsForTheElementToBeEnabled(t *testing.T) {
	// A "send" button the app enables only once it has processed the typed text.
	sb := &settlingBackend{still: true, disabledReads: 2}
	svc := NewService(t.TempDir(), sb)
	if _, err := svc.Call(context.Background(), "tap", json.RawMessage(`{"device":"android:x","selector":{"text":"Log in"}}`)); err != nil {
		t.Fatal(err)
	}
	want := []string{"settle", "ui", "ui", "ui", "tap 25,25"}
	if fmt.Sprint(sb.calls) != fmt.Sprint(want) {
		t.Fatalf("calls = %v, want %v", sb.calls, want)
	}
}

// elementTapper implements backend.ElementTapper, like the iOS backend.
type elementTapper struct {
	settlingBackend
}

func (e *elementTapper) TapElement(ctx context.Context, id string, el *api.Element, atRest bool) error {
	e.calls = append(e.calls, fmt.Sprintf("tapelement %q %v rest=%v", el.Text, el.Frame, atRest))
	return nil
}

func TestTapBySelectorHandsTheElementToAnElementTapper(t *testing.T) {
	et := &elementTapper{settlingBackend{still: true}}
	svc := NewService(t.TempDir(), et)
	if _, err := svc.Call(context.Background(), "tap", json.RawMessage(`{"device":"android:x","selector":{"text":"Log in"}}`)); err != nil {
		t.Fatal(err)
	}
	want := []string{"settle", "ui", `tapelement "Log in" {10 20 30 10} rest=false`}
	if fmt.Sprint(et.calls) != fmt.Sprint(want) {
		t.Fatalf("calls = %v, want %v", et.calls, want)
	}
	// The element at rest on a screen that never goes still keeps that verdict.
	et = &elementTapper{settlingBackend{still: false, frames: []api.Rect{{X: 10, Y: 120, W: 30, H: 10}}}}
	svc = NewService(t.TempDir(), et)
	if _, err := svc.Call(context.Background(), "tap", json.RawMessage(`{"device":"android:x","selector":{"text":"Log in"}}`)); err != nil {
		t.Fatal(err)
	}
	want = []string{"settle", "ui", "ui", `tapelement "Log in" {10 120 30 10} rest=true`}
	if fmt.Sprint(et.calls) != fmt.Sprint(want) {
		t.Fatalf("calls = %v, want %v", et.calls, want)
	}
	// A tap at coordinates stays the backend's plain tap.
	et.calls = nil
	if _, err := svc.Call(context.Background(), "tap", json.RawMessage(`{"device":"android:x","x":5,"y":5}`)); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(et.calls) != "[tap 5,5]" {
		t.Fatalf("a tap at coordinates: %v", et.calls)
	}
}

// launchBackend records plain launches; like the Android backend it has no LaunchWith.
type launchBackend struct {
	fakeBackend
	launched []string
}

func (l *launchBackend) Launch(ctx context.Context, id, app string) error {
	l.launched = append(l.launched, app)
	return nil
}

// optionsLauncher implements backend.Launcher, like the iOS backend.
type optionsLauncher struct {
	launchBackend
	got backend.LaunchOptions
}

func (o *optionsLauncher) LaunchWith(ctx context.Context, id, app string, opts backend.LaunchOptions) error {
	o.got = opts
	return nil
}

func TestLaunchOptionsNeedALauncher(t *testing.T) {
	lb := &launchBackend{}
	svc := NewService(t.TempDir(), lb)
	if _, err := svc.Call(context.Background(), "launch", json.RawMessage(`{"device":"android:x","app":"com.app"}`)); err != nil {
		t.Fatal(err)
	}
	for _, extra := range []string{`"args":["-x"]`, `"env":{"A":"1"}`, `"relaunch":true`} {
		_, err := svc.Call(context.Background(), "launch", json.RawMessage(`{"device":"android:x","app":"com.app",`+extra+`}`))
		if err == nil || !strings.Contains(err.Error(), "not supported on android") {
			t.Fatalf("%s: err = %v, want not supported", extra, err)
		}
	}
	if len(lb.launched) != 1 {
		t.Fatalf("launched %v, want only the plain launch", lb.launched)
	}

	ol := &optionsLauncher{}
	svc = NewService(t.TempDir(), ol)
	if _, err := svc.Call(context.Background(), "launch", json.RawMessage(`{"device":"android:x","app":"com.app","args":["--initialUrl","http://localhost:8081"],"env":{"A":"1"},"relaunch":true}`)); err != nil {
		t.Fatal(err)
	}
	if len(ol.got.Args) != 2 || ol.got.Args[1] != "http://localhost:8081" || ol.got.Env["A"] != "1" || !ol.got.Relaunch || len(ol.launched) != 0 {
		t.Fatalf("LaunchWith got %+v (plain launches %v)", ol.got, ol.launched)
	}
}

func TestActionWaitsForItsEffectInTheSameCall(t *testing.T) {
	sb := &settlingBackend{still: true}
	svc := NewService(t.TempDir(), sb)
	res, err := svc.Call(context.Background(), "tap", json.RawMessage(`{"device":"android:x","selector":{"text":"Log in"},"waitFor":{"contains":"welcome"}}`))
	if err != nil {
		t.Fatal(err)
	}
	m, ok := res.(map[string]any)
	if !ok || m["result"] == nil || m["waited"] == nil {
		t.Fatalf("result = %#v, want result and waited", res)
	}
	if el, _ := m["waited"].(*api.Element); el == nil || el.Ref != "e2" {
		t.Fatalf("waited = %#v, want e2", m["waited"])
	}
	_, err = svc.Call(context.Background(), "tap", json.RawMessage(`{"device":"android:x","x":1,"y":1,"waitFor":{"text":"never"},"waitTimeoutMs":100}`))
	if err == nil || !strings.Contains(err.Error(), "tap done, but") {
		t.Fatalf("err = %v, want the action to report the failed wait", err)
	}
}

func TestActionCanReturnTheScreenItEndsOn(t *testing.T) {
	sb := &settlingBackend{still: true}
	svc := NewService(t.TempDir(), sb)
	res, err := svc.Call(context.Background(), "tap", json.RawMessage(`{"device":"android:x","selector":{"text":"Log in"},"waitFor":{"contains":"welcome"},"returnUi":true}`))
	if err != nil {
		t.Fatal(err)
	}
	m := res.(map[string]any)
	if tr, _ := m["ui"].(*api.UITree); tr == nil || tr.Root == nil || m["waited"] == nil || m["result"] == nil {
		t.Fatalf("got %#v, want result, waited and ui", res)
	}
}

func TestTapBySelectorDoesNotTapAnElementThatWentAway(t *testing.T) {
	for name, sb := range map[string]*settlingBackend{
		"while moving":   {still: false, frames: []api.Rect{{X: 10, Y: 300, W: 30, H: 10}, {X: 10, Y: 200, W: 30, H: 10}}, goneAfter: 2},
		"while disabled": {still: true, disabledReads: 5, goneAfter: 2},
	} {
		svc := NewService(t.TempDir(), sb)
		_, err := svc.Call(context.Background(), "tap", json.RawMessage(`{"device":"android:x","selector":{"text":"Log in"}}`))
		if err == nil || !strings.Contains(err.Error(), "disappeared") {
			t.Fatalf("%s: err = %v, want an error saying it disappeared", name, err)
		}
		for _, c := range sb.calls {
			if strings.HasPrefix(c, "tap") {
				t.Fatalf("%s: tapped anyway: %v", name, sb.calls)
			}
		}
	}
}

// actionableBackend implements backend.Actionability like the Android backend: it reads the
// element with the daemon's find until two reads agree (frames are the button's successive
// frames, as in settlingBackend), or fails when fail is set.
type actionableBackend struct {
	settlingBackend
	fail error
}

func (a *actionableBackend) AwaitActionable(ctx context.Context, id string, find func() (*api.Element, error)) (*api.Element, bool, error) {
	a.calls = append(a.calls, "await")
	if a.fail != nil {
		return nil, false, a.fail
	}
	prev, err := find()
	if err != nil {
		return nil, false, err
	}
	for {
		el, err := find()
		if err != nil {
			return nil, false, err
		}
		if el.Frame == prev.Frame {
			return el, true, nil
		}
		prev = el
	}
}

func TestTapBySelectorWaitsUntilTheBackendFindsItActionable(t *testing.T) {
	ab := &actionableBackend{settlingBackend: settlingBackend{frames: []api.Rect{{X: 10, Y: 300, W: 30, H: 10}, {X: 10, Y: 120, W: 30, H: 10}, {X: 10, Y: 120, W: 30, H: 10}}}}
	svc := NewService(t.TempDir(), ab)
	res, err := svc.Call(context.Background(), "tap", json.RawMessage(`{"device":"android:x","selector":{"text":"Log in"}}`))
	if err != nil {
		t.Fatal(err)
	}
	// The backend's own wait replaces Settle and the daemon's frame check; the tap is the plain
	// Tap at the resting frame.
	want := []string{"await", "ui", "ui", "ui", "tap 25,125"}
	if fmt.Sprint(ab.calls) != fmt.Sprint(want) {
		t.Fatalf("calls = %v, want %v", ab.calls, want)
	}
	if m := res.(map[string]any); m["y"] != 125 {
		t.Fatalf("result = %v, want y 125", m)
	}

	ab = &actionableBackend{fail: errors.New(`no element matches {"text":"Log in"} (it disappeared while it was moving)`)}
	svc = NewService(t.TempDir(), ab)
	if _, err := svc.Call(context.Background(), "tap", json.RawMessage(`{"device":"android:x","selector":{"text":"Log in"}}`)); err == nil || !strings.Contains(err.Error(), "disappeared") {
		t.Fatalf("err = %v, want the backend's error", err)
	}
	if fmt.Sprint(ab.calls) != "[await]" {
		t.Fatalf("calls = %v, want no tap", ab.calls)
	}
}

// stuckFrames never delivers a frame: WaitFrame blocks until its context ends.
type stuckFrames struct{ fakeBackend }

func (s *stuckFrames) UITree(ctx context.Context, id string) (*api.UITree, error) { return tree(), nil }
func (s *stuckFrames) WaitFrame(ctx context.Context, id string, mark uint64, max time.Duration) (uint64, error) {
	if mark == 0 {
		return 7, nil
	}
	<-ctx.Done()
	return 0, ctx.Err()
}

func TestWaitEndsOnTimeWhenFramesStall(t *testing.T) {
	svc := NewService(t.TempDir(), &stuckFrames{})
	start := time.Now()
	_, err := svc.Call(context.Background(), "wait", json.RawMessage(`{"device":"android:x","selector":{"text":"never"},"timeoutMs":900}`))
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("err = %v, want a timeout", err)
	}
	if el := time.Since(start); el > 4*time.Second {
		t.Fatalf("wait took %v with a 900 ms timeout", el)
	}
}

// The exit method stops Serve the way SIGTERM does: it is how `agemu daemon stop` ends the daemon
// on Windows. The client gets its reply before the daemon goes.
func TestExitMethodStopsServe(t *testing.T) {
	t.Setenv("AGEMU_HOME", shortTempDir(t))
	done := make(chan error, 1)
	go func() { done <- Serve(context.Background(), NewService(t.TempDir())) }()
	var c net.Conn
	for end := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		var err error
		if c, err = net.Dial("unix", SocketPath()); err == nil {
			break
		}
		if time.Now().After(end) {
			t.Fatalf("daemon did not listen: %v", err)
		}
	}
	defer c.Close()
	if _, err := c.Write([]byte(`{"id":7,"method":"exit"}` + "\n")); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(c).ReadBytes('\n')
	if err != nil {
		t.Fatalf("no reply to exit: %v", err)
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil || resp.ID != 7 || resp.Error != "" {
		t.Fatalf("exit reply %s (%v)", line, err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve after exit: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve still runs after exit")
	}
	// The lock is free again: another daemon may start.
	lock, err := os.OpenFile(LockPath(), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := osproc.TryLock(lock, true); err != nil {
		t.Fatalf("lock still held after exit: %v", err)
	}
}

// Without Serve (a connection served by ServeConn), exit is refused instead of doing nothing.
func TestExitMethodWithoutServe(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	go ServeConn(context.Background(), NewService(t.TempDir()), b)
	if _, err := a.Write([]byte(`{"id":1,"method":"exit"}` + "\n")); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(a).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil || resp.Error == "" {
		t.Fatalf("exit without Serve: %s", line)
	}
}
