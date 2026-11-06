package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/api"
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/backend"
)

func tree() *api.UITree {
	return &api.UITree{Width: 100, Height: 200, Root: &api.Element{Ref: "e0", Role: "window", Frame: api.Rect{W: 100, H: 200}, Children: []*api.Element{
		{Ref: "e1", Role: "button", Text: "Log in", ID: "com.app:id/login", Frame: api.Rect{X: 10, Y: 20, W: 30, H: 10}},
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
	_, err := safeCall(context.Background(), s, Request{Method: "swipe", Params: json.RawMessage(`{"device":"android:x","x":1,"y":1,"x2":2,"y2":2}`)})
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
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
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
	syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
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
	d, err := os.MkdirTemp("/tmp", "agemu-t")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}
