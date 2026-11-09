package mcp

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/backend"
)

// parkBackend is a Parker that records typed text and fails (records) any call that reaches the
// device while it is parked or being parked.
type parkBackend struct {
	orderBackend
	pmu              sync.Mutex
	parked, stopping bool
	parks, resumes   int
	bad              []string
	parkStarted      chan struct{}
}

var _ backend.Parker = (*parkBackend)(nil)

func (p *parkBackend) Park(ctx context.Context, id string) error {
	p.pmu.Lock()
	p.stopping = true
	first := p.parks == 0
	p.pmu.Unlock()
	if first {
		close(p.parkStarted)
	}
	time.Sleep(200 * time.Millisecond) // the calls below arrive during the park
	p.pmu.Lock()
	p.stopping, p.parked = false, true
	p.parks++
	p.pmu.Unlock()
	return nil
}

func (p *parkBackend) Parked(id string) bool {
	p.pmu.Lock()
	defer p.pmu.Unlock()
	return p.parked
}

func (p *parkBackend) Resume(ctx context.Context, id string) (bool, error) {
	p.pmu.Lock()
	defer p.pmu.Unlock()
	if p.stopping {
		p.bad = append(p.bad, "resume during the park")
	}
	if !p.parked {
		return false, nil
	}
	p.parked = false
	p.resumes++
	return true, nil
}

func (p *parkBackend) TypeText(ctx context.Context, id, text string) error {
	p.pmu.Lock()
	if p.parked || p.stopping {
		p.bad = append(p.bad, fmt.Sprintf("type %s on a parked device (stopping %v)", text, p.stopping))
	}
	p.pmu.Unlock()
	return p.orderBackend.TypeText(ctx, id, text)
}

// The per-device FIFO of the MCP server and auto-park's resume on the first call: calls sent while
// the daemon parks the device all wait for the park, the first one resumes it (once), and they run
// in the order sent, none on the stopped device. Auto-park's shortest threshold is 10 s.
func TestAutoParkResumeKeepsTheFIFOOrder(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for a 10 s auto-park")
	}
	t.Setenv("AGEMU_AUTO_PARK", "10s")
	b := &parkBackend{parkStarted: make(chan struct{})}
	send, resp := orderSession(t, b)
	send(typeReq(1, "android:x", "first")) // the daemon serves the device: auto-park counts it
	<-resp
	select {
	case <-b.parkStarted:
	case <-time.After(15 * time.Second):
		t.Fatal("not auto-parked")
	}
	const n = 20
	for i := 0; i < n; i++ {
		send(typeReq(100+i, "android:x", fmt.Sprintf("t%02d", i)))
	}
	for i := 0; i < n; i++ {
		select {
		case m := <-resp:
			if r, _ := m["result"].(map[string]any); r == nil || r["isError"] == true {
				t.Fatalf("call failed: %v", m)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("answer %d of %d missing", i, n)
		}
	}
	b.mu.Lock()
	got := strings.Join(b.typed["android:x"], ",")
	b.mu.Unlock()
	want := "first"
	for i := 0; i < n; i++ {
		want += fmt.Sprintf(",t%02d", i)
	}
	if got != want {
		t.Fatalf("typed %s\nwant  %s", got, want)
	}
	b.pmu.Lock()
	defer b.pmu.Unlock()
	if b.parks != 1 || b.resumes != 1 || len(b.bad) > 0 {
		t.Fatalf("parks %d resumes %d bad %v", b.parks, b.resumes, b.bad)
	}
}
