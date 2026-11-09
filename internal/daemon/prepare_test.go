package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/api"
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/backend"
)

// prepBackend is a Preparer whose device is preparing until done is set.
type prepBackend struct {
	fakeBackend
	preparing bool
	booted    int
	shutdowns int
	opts      []api.BootOptions
}

var _ backend.Preparer = (*prepBackend)(nil)

func (p *prepBackend) Preparing(id string) *api.Preparing {
	if !p.preparing {
		return nil
	}
	return &api.Preparing{Phase: "settling", ElapsedS: 75, MaxS: 840}
}

func (p *prepBackend) AwaitDecision(ctx context.Context, id string) *api.Preparing {
	return p.Preparing(id)
}

func (p *prepBackend) List(ctx context.Context) ([]api.Device, error) {
	st := "booted"
	if p.preparing {
		st = api.StatePreparing
	}
	return []api.Device{{ID: "android:x", Name: "x", Platform: api.Android, State: st, Preparing: p.Preparing("android:x")}}, nil
}

func (p *prepBackend) Boot(ctx context.Context, id string, o api.BootOptions) (api.Device, error) {
	p.booted++
	p.opts = append(p.opts, o)
	st := "booted"
	if p.preparing {
		st = api.StatePreparing
	}
	return api.Device{ID: id, State: st}, nil
}

func (p *prepBackend) Shutdown(ctx context.Context, id string) error {
	p.shutdowns++
	p.preparing = false
	return nil
}

func (p *prepBackend) Logs(ctx context.Context, id string, q api.LogQuery) (api.LogBatch, error) {
	return api.LogBatch{}, nil
}

func TestCallsOnAPreparingDeviceFailAtOnce(t *testing.T) {
	pb := &prepBackend{preparing: true}
	s := NewService(t.TempDir(), pb)
	for _, m := range []string{"ui", "tap", "screenshot", "reset", "launch", "park"} {
		raw := json.RawMessage(`{"device":"android:x","x":1,"y":1,"app":"a.b"}`)
		start := time.Now()
		_, err := s.Call(context.Background(), m, raw)
		var pe *api.PreparingError
		if !errors.As(err, &pe) || !errors.Is(err, api.ErrPreparing) {
			t.Fatalf("%s: got %v, want a PreparingError", m, err)
		}
		if pe.Phase != "settling" || pe.Elapsed != 75 || pe.MaxS != 840 {
			t.Fatalf("%s: %+v", m, pe)
		}
		if errorCode(err) != CodePreparing {
			t.Fatalf("%s: code %q", m, errorCode(err))
		}
		if time.Since(start) > time.Second {
			t.Fatalf("%s took %v", m, time.Since(start))
		}
	}
	if pb.taps != 0 {
		t.Fatal("a tap reached the backend")
	}
	// devices, boot (state), logs and shutdown go through.
	if _, err := s.Call(context.Background(), "devices", nil); err != nil {
		t.Fatal(err)
	}
	res, err := s.Call(context.Background(), "boot", json.RawMessage(`{"device":"android:x","noWait":true}`))
	if err != nil || res.(api.Device).State != api.StatePreparing {
		t.Fatalf("boot: %v %v", res, err)
	}
	if len(pb.opts) != 1 || !pb.opts[0].NoWait {
		t.Fatalf("boot options %+v: noWait must reach the backend", pb.opts)
	}
	if _, err := s.Call(context.Background(), "logs", json.RawMessage(`{"device":"android:x"}`)); err != nil {
		t.Fatalf("logs: %v", err)
	}
	if _, err := s.Call(context.Background(), "shutdown", json.RawMessage(`{"device":"android:x"}`)); err != nil || pb.shutdowns != 1 {
		t.Fatalf("shutdown: %v (%d)", err, pb.shutdowns)
	}
	// Once it is no longer preparing, calls work again.
	if _, err := s.Call(context.Background(), "ui", json.RawMessage(`{"device":"android:x"}`)); err != nil {
		t.Fatalf("ui after preparing: %v", err)
	}
}

func TestShutdownOfAPreparingDeviceSkipsTheDeviceLock(t *testing.T) {
	pb := &prepBackend{preparing: true}
	s := NewService(t.TempDir(), pb)
	l := s.deviceLock("android:x")
	l.Lock() // a boot without NoWait holds it for minutes
	defer l.Unlock()
	done := make(chan error, 1)
	go func() {
		_, err := s.Call(context.Background(), "shutdown", json.RawMessage(`{"device":"android:x"}`))
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown waited for the device lock of the preparing boot")
	}
}

func TestPreparingCodeSurvivesTheSocket(t *testing.T) {
	pb := &prepBackend{preparing: true}
	s := NewService(t.TempDir(), pb)
	a, b := net.Pipe()
	go ServeConn(context.Background(), s, a)
	defer b.Close()
	req, _ := json.Marshal(Request{ID: 1, Method: "tap", Params: json.RawMessage(`{"device":"android:x","x":1,"y":1}`)})
	go b.Write(append(req, '\n'))
	buf := make([]byte, 4096)
	n, err := b.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	var resp Response
	if err := json.Unmarshal(buf[:n], &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Code != CodePreparing || resp.Error == "" {
		t.Fatalf("response %+v", resp)
	}
}

func TestAnImplicitDeviceCallDuringAPreparationSaysSo(t *testing.T) {
	pb := &prepBackend{preparing: true}
	s := NewService(t.TempDir(), pb)
	_, err := s.Call(context.Background(), "ui", json.RawMessage(`{}`)) // no device: the only one is preparing
	if !errors.Is(err, api.ErrPreparing) {
		t.Fatalf("got %v, want the preparing error", err)
	}
}
