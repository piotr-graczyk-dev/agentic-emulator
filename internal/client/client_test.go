package client

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/api"
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/backend"
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/daemon"
)

// bare is an Android backend with no capability interfaces.
type bare struct{ backend.Backend }

func (bare) Platform() api.Platform { return api.Android }

func TestNotImplementedSurvivesTheSocket(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c1, c2 := net.Pipe()
	go daemon.ServeConn(ctx, daemon.NewService(t.TempDir(), bare{}), c2)
	cl := New(c1)
	defer cl.Close()

	_, _, err := cl.CallRaw("push", daemon.Params{Device: "android:x", App: "com.app", Payload: []byte(`{"aps":{}}`)})
	if !errors.Is(err, api.ErrNotImplemented) {
		t.Fatalf("err = %v, want it to match api.ErrNotImplemented", err)
	}
	if !strings.HasPrefix(err.Error(), "push is not implemented on android: no faithful path") {
		t.Fatalf("message = %q", err)
	}

	_, _, err = cl.CallRaw("push", daemon.Params{Device: "android:x", App: "com.app"})
	if err == nil || errors.Is(err, api.ErrNotImplemented) {
		t.Fatalf("an argument error: err = %v, want an error that is not 'not implemented'", err)
	}

	for _, method := range []string{"logs", "crashes"} {
		_, _, err = cl.CallRaw(method, daemon.Params{Device: "android:x"})
		if !errors.Is(err, api.ErrNotImplemented) || err.Error() != method+" is not implemented on android" {
			t.Fatalf("%s: err = %v", method, err)
		}
	}
}

// crashing is an Android backend whose launched app has crashed.
type crashing struct {
	bare
	crash api.Crash
}

func (c *crashing) Launch(ctx context.Context, id, app string) error { return nil }
func (c *crashing) UITree(ctx context.Context, id string) (*api.UITree, error) {
	return &api.UITree{Root: &api.Element{Role: "window"}}, nil
}
func (c *crashing) AppState(ctx context.Context, id, app string, since time.Time) (api.AppState, error) {
	if c.crash.Time.Before(since) {
		return api.AppState{Running: true}, nil
	}
	return api.AppState{Crash: &c.crash}, nil
}

func TestTheCrashNoticeSurvivesTheSocket(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := &crashing{}
	c1, c2 := net.Pipe()
	go daemon.ServeConn(ctx, daemon.NewService(t.TempDir(), b), c2)
	cl := New(c1)
	defer cl.Close()

	if _, err := cl.Do("launch", daemon.Params{Device: "android:x", App: "com.app"}); err != nil {
		t.Fatal(err)
	}
	b.crash = api.Crash{Time: time.Now().Add(time.Second), App: "com.app", Kind: api.CrashKindCrash, Exception: "SIGSEGV (SEGV_MAPERR)"}
	r, err := cl.Do("ui", daemon.Params{Device: "android:x"})
	if err != nil || r.AppCrash == nil || r.AppCrash.Exception != "SIGSEGV (SEGV_MAPERR)" || r.AppCrash.Running || len(r.Result) == 0 {
		t.Fatalf("reply %+v, err %v", r, err)
	}
	_, err = cl.Do("wait", daemon.Params{Device: "android:x", Selector: &api.Selector{Text: "Pay"}, TimeoutMs: 100})
	var de *Error
	if !errors.Is(err, api.ErrAppCrashed) || !errors.As(err, &de) || de.Crash == nil || de.Code != daemon.CodeAppCrashed {
		t.Fatalf("err = %#v", err)
	}
	if !strings.Contains(err.Error(), "com.app crashed at") {
		t.Fatalf("message %q", err)
	}
}
