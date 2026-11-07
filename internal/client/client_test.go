package client

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

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
}
