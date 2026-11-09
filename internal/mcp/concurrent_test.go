package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/api"
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/backend"
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/client"
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/daemon"
)

// slowBackend blocks UITree until release is closed, and reports its device as preparing
// (backend.Preparer) when preparing is set.
type slowBackend struct {
	backend.Backend
	release   chan struct{}
	preparing bool
	mu        sync.Mutex
	bootOpts  []api.BootOptions
}

func (s *slowBackend) Platform() api.Platform { return api.Android }
func (s *slowBackend) List(ctx context.Context) ([]api.Device, error) {
	return []api.Device{{ID: "android:x", Name: "x", Platform: api.Android, State: "booted"}}, nil
}
func (s *slowBackend) UITree(ctx context.Context, id string) (*api.UITree, error) {
	<-s.release
	return &api.UITree{}, nil
}
func (s *slowBackend) Preparing(id string) *api.Preparing {
	if !s.preparing {
		return nil
	}
	return &api.Preparing{Phase: "settling", ElapsedS: 12, MaxS: 870}
}
func (s *slowBackend) Boot(ctx context.Context, id string, o api.BootOptions) (api.Device, error) {
	s.mu.Lock()
	s.bootOpts = append(s.bootOpts, o)
	s.mu.Unlock()
	return api.Device{ID: id, State: api.StatePreparing, Preparing: s.Preparing(id)}, nil
}

// mcpSession runs serve against a daemon service on pipes and returns a line writer and a
// reader of response lines.
func mcpSession(t *testing.T, b backend.Backend) (func(string), chan map[string]any) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	svc := daemon.NewService(t.TempDir(), b)
	dial := func() (*client.Client, error) {
		c1, c2 := net.Pipe()
		go daemon.ServeConn(ctx, svc, c2)
		return client.New(c1), nil
	}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	t.Cleanup(func() { inW.Close() })
	go func() { _ = serve(inR, outW, dial); outW.Close() }()
	resp := make(chan map[string]any, 16)
	go func() {
		sc := bufio.NewScanner(outR)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			var m map[string]any
			if json.Unmarshal(sc.Bytes(), &m) == nil {
				resp <- m
			}
		}
		close(resp)
	}()
	send := func(line string) { _, _ = inW.Write([]byte(line + "\n")) }
	return send, resp
}

func TestToolCallsDoNotWaitForEachOther(t *testing.T) {
	sb := &slowBackend{release: make(chan struct{})}
	send, resp := mcpSession(t, sb)
	send(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"ui_tree","arguments":{"device":"android:x"}}}`)
	time.Sleep(50 * time.Millisecond)
	send(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"devices","arguments":{}}}`)
	select {
	case m := <-resp:
		if id, _ := m["id"].(float64); id != 2 {
			t.Fatalf("first answer is for id %v, want 2 (devices while ui_tree blocks)", m["id"])
		}
	case <-time.After(3 * time.Second):
		t.Fatal("devices was not answered while another call was in flight")
	}
	close(sb.release)
	select {
	case m := <-resp:
		if id, _ := m["id"].(float64); id != 1 {
			t.Fatalf("got id %v, want 1", m["id"])
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the slow call was never answered")
	}
}

func TestEveryRequestIsAnsweredOnceAsOneLine(t *testing.T) {
	sb := &slowBackend{release: make(chan struct{})}
	close(sb.release)
	send, resp := mcpSession(t, sb)
	const n = 40
	for i := 1; i <= n; i++ {
		send(`{"jsonrpc":"2.0","id":` + strconv.Itoa(i) + `,"method":"tools/call","params":{"name":"ui_tree","arguments":{"device":"android:x"}}}`)
	}
	seen := map[float64]bool{}
	for len(seen) < n {
		select {
		case m, ok := <-resp:
			if !ok {
				t.Fatalf("stream closed after %d answers", len(seen))
			}
			id, _ := m["id"].(float64)
			if seen[id] {
				t.Fatalf("id %v answered twice", id)
			}
			seen[id] = true
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of %d answered", len(seen), n)
		}
	}
}

func TestBootOfAPreparingDeviceReturnsAtOnceWithAHint(t *testing.T) {
	sb := &slowBackend{release: make(chan struct{}), preparing: true}
	send, resp := mcpSession(t, sb)
	send(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"boot","arguments":{"device":"android:x"}}}`)
	m := <-resp
	b, _ := json.Marshal(m)
	for _, want := range []string{`preparing`, `Poll`, `devices`, `settling`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("boot result %s lacks %q", b, want)
		}
	}
	sb.mu.Lock()
	defer sb.mu.Unlock()
	if len(sb.bootOpts) != 1 || !sb.bootOpts[0].NoWait {
		t.Fatalf("boot options %+v: the MCP boot must not block", sb.bootOpts)
	}
	// Another call on the preparing device fails at once, with the reason.
	send(`{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"ui_tree","arguments":{"device":"android:x"}}}`)
	select {
	case m := <-resp:
		b, _ := json.Marshal(m)
		if !strings.Contains(string(b), `"isError":true`) || !strings.Contains(string(b), "preparing its baseline") {
			t.Fatalf("ui_tree on a preparing device: %s", b)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a call on a preparing device blocked")
	}
}

func TestACallIsResentAfterTheDaemonRestarted(t *testing.T) {
	sb := &slowBackend{release: make(chan struct{})}
	close(sb.release)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc := daemon.NewService(t.TempDir(), sb)
	dials := 0
	dial := func() (*client.Client, error) {
		dials++
		c1, c2 := net.Pipe()
		if dials == 1 {
			c2.Close() // the daemon behind this connection is gone
		} else {
			go daemon.ServeConn(ctx, svc, c2)
		}
		return client.New(c1), nil
	}
	pool := &clientPool{dial: dial}
	res, rerr := handle(rpcReq{ID: json.RawMessage("1"), Method: "tools/call", Params: json.RawMessage(`{"name":"devices","arguments":{}}`)}, pool)
	if rerr != nil {
		t.Fatal(rerr)
	}
	b, _ := json.Marshal(res)
	if strings.Contains(string(b), `"isError":true`) || !strings.Contains(string(b), "android:x") {
		t.Fatalf("devices after a daemon restart: %s", b)
	}
	if dials != 2 {
		t.Fatalf("dials %d, want 2 (the broken connection, then a new one)", dials)
	}
}
