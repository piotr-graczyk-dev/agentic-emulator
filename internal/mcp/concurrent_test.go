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
func (s *slowBackend) AwaitDecision(ctx context.Context, id string) *api.Preparing {
	return s.Preparing(id)
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
	return sessionWithDial(t, dial)
}

func sessionWithDial(t *testing.T, dial func() (*client.Client, error)) (func(string), chan map[string]any) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	t.Cleanup(func() { inW.Close() })
	go func() { _ = serve(inR, outW, dial); outW.Close() }()
	resp := make(chan map[string]any, 1024)
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

// Every spelling of one device lands in one queue; a reference whose device is not known here
// (a simulator's name, no platform) is ordered against everything.
func TestDeviceKeySpellings(t *testing.T) {
	const u = "ED6C55BD-83FB-49B9-93A2-54BA0AD521DD"
	same := []string{"ios:" + u, "ios:" + strings.ToLower(u), u, " ios:" + u + " "}
	for _, s := range same {
		if k := deviceKey(s); k != "ios:"+u {
			t.Errorf("deviceKey(%q) = %q, want ios:%s", s, k, u)
		}
	}
	for _, s := range []string{"android:agemu-lab", "android:Agemu-Lab"} {
		if k := deviceKey(s); k != "android:agemu-lab" {
			t.Errorf("deviceKey(%q) = %q", s, k)
		}
	}
	for _, s := range []string{"", "ios:agemu-iphone-17-pro", "agemu-iphone-17-pro", "agemu-lab", "android:"} {
		if k := deviceKey(s); k != "" {
			t.Errorf("deviceKey(%q) = %q, want the implicit device (a barrier)", s, k)
		}
	}
	if deviceKey("android:agemu-a") == deviceKey("android:agemu-b") {
		t.Error("two AVDs share a queue")
	}
}

// After a burst of concurrent calls the pool keeps at most maxIdleConns connections; it closes
// the others.
func TestThePoolKeepsAtMostMaxIdleConns(t *testing.T) {
	p := &clientPool{}
	var peers []net.Conn
	for i := 0; i < maxIdleConns+3; i++ {
		a, b := net.Pipe()
		peers = append(peers, b)
		p.put(client.New(a), false)
	}
	if len(p.idle) != maxIdleConns {
		t.Fatalf("%d idle connections, want %d", len(p.idle), maxIdleConns)
	}
	for _, b := range peers[maxIdleConns:] {
		b.SetReadDeadline(time.Now().Add(time.Second))
		if _, err := b.Read(make([]byte, 1)); err != io.EOF {
			t.Fatalf("an extra connection was not closed: %v", err)
		}
	}
	p.close()
}
