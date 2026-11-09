package mcp

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/api"
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/backend"
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/client"
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/daemon"
)

// orderBackend records typed text per device; UITree on device "x" blocks until release.
type orderBackend struct {
	backend.Backend
	mu      sync.Mutex
	typed   map[string][]string
	release chan struct{}
}

func (o *orderBackend) Platform() api.Platform { return api.Android }
func (o *orderBackend) List(ctx context.Context) ([]api.Device, error) {
	return []api.Device{{ID: "android:x", Name: "x", Platform: api.Android, State: "booted"},
		{ID: "android:y", Name: "y", Platform: api.Android, State: "shutdown"}}, nil
}
func (o *orderBackend) TypeText(ctx context.Context, id, text string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.typed == nil {
		o.typed = map[string][]string{}
	}
	o.typed[id] = append(o.typed[id], text)
	return nil
}
func (o *orderBackend) UITree(ctx context.Context, id string) (*api.UITree, error) {
	if id == "android:x" && o.release != nil {
		<-o.release
	}
	return &api.UITree{}, nil
}

func orderSession(t *testing.T, b backend.Backend) (func(string), chan map[string]any) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	svc := daemon.NewService(t.TempDir(), b)
	dial := func() (*client.Client, error) {
		c1, c2 := net.Pipe()
		go daemon.ServeConn(ctx, svc, c2)
		return client.New(c1), nil
	}
	return sessionWithDial(t, dial)
}

func typeReq(id int, dev, text string) string {
	devArg := ""
	if dev != "" {
		devArg = fmt.Sprintf(`"device":%q,`, dev)
	}
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"type_text","arguments":{%s"text":%q}}}`, id, devArg, text)
}

// The re-check's test: pairs of type_text sent back to back on one device must run in the
// order sent (before the fix 4-21 of 300 pairs ran reversed).
func TestCallsOnOneDeviceRunInArrivalOrder(t *testing.T) {
	for run := 0; run < 5; run++ {
		for _, dev := range []string{"android:x", "x", ""} { // explicit, bare name, implicit
			b := &orderBackend{}
			send, resp := orderSession(t, b)
			const rounds = 300
			for i := 0; i < rounds; i++ {
				send(typeReq(2*i+1, dev, fmt.Sprintf("a%d", i)))
				send(typeReq(2*i+2, dev, fmt.Sprintf("b%d", i)))
				<-resp
				<-resp
			}
			b.mu.Lock()
			got := b.typed["android:x"]
			b.mu.Unlock()
			if len(got) != 2*rounds {
				t.Fatalf("device %q: typed %d, want %d", dev, len(got), 2*rounds)
			}
			for i := 0; i < len(got); i += 2 {
				if !strings.HasPrefix(got[i], "a") || !strings.HasPrefix(got[i+1], "b") {
					t.Fatalf("run %d, device %q: pair %d ran as %s, %s", run, dev, i/2, got[i], got[i+1])
				}
			}
		}
	}
}

func TestPipelinedCallsKeepTheirOrder(t *testing.T) {
	b := &orderBackend{}
	send, resp := orderSession(t, b)
	const n = 200
	for i := 0; i < n; i++ { // no waiting between sends at all
		send(typeReq(i+1, "android:x", fmt.Sprintf("%03d", i)))
	}
	for i := 0; i < n; i++ {
		<-resp
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for i, s := range b.typed["android:x"] {
		if s != fmt.Sprintf("%03d", i) {
			t.Fatalf("call %d ran as %s", i, s)
		}
	}
}

func TestALongCallBlocksOnlyItsDevice(t *testing.T) {
	b := &orderBackend{release: make(chan struct{})}
	send, resp := orderSession(t, b)
	send(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"ui_tree","arguments":{"device":"android:x"}}}`)
	time.Sleep(30 * time.Millisecond)
	send(typeReq(2, "android:x", "after-ui")) // same device: waits for the ui_tree
	send(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"devices","arguments":{}}}`)
	send(typeReq(4, "android:y", "other-device"))
	got := map[float64]bool{}
	for len(got) < 2 {
		select {
		case m := <-resp:
			id, _ := m["id"].(float64)
			if id == 1 || id == 2 {
				t.Fatalf("id %v answered while the ui_tree on the same device blocks", id)
			}
			got[id] = true
		case <-time.After(3 * time.Second):
			t.Fatalf("devices and the other device's call must answer during a long call; got %v", got)
		}
	}
	close(b.release)
	for len(got) < 4 {
		select {
		case m := <-resp:
			id, _ := m["id"].(float64)
			got[id] = true
		case <-time.After(3 * time.Second):
			t.Fatalf("after the release: %v", got)
		}
	}
}

func TestAnImplicitDeviceCallIsABarrier(t *testing.T) {
	var o deviceOrder
	w1, d1 := o.enter("x")
	w2, d2 := o.enter("y")
	w3, d3 := o.enter("") // the only booted device: may be x or y
	barrier := o.barrier
	w4, d4 := o.enter("x")
	if len(w1) != 0 || len(w2) != 0 {
		t.Fatal("the first calls per device wait for nothing")
	}
	if len(w3) != 2 {
		t.Fatalf("the implicit call waits for both earlier device calls, got %d", len(w3))
	}
	if len(w4) != 1 || w4[0] != barrier {
		t.Fatal("a later device call waits for the implicit one")
	}
	d1()
	d2()
	d3()
	d4()
	if len(o.last) != 0 || o.barrier != nil {
		t.Fatalf("state not cleaned up: %v %v", o.last, o.barrier)
	}
}
