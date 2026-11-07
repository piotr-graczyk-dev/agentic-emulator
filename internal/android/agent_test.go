package android

import (
	"bufio"
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

// A cancelled context ends a call that waits for the agent's answer (a UI tree read held up by
// a dying app's window), not only its deadline; the connection is then dropped, so the agent's
// late answer is never read as the next call's.
func TestAgentCallEndsWhenItsContextIsCancelled(t *testing.T) {
	client, agent := net.Pipe()
	defer agent.Close()
	go func() { // the agent reads the request and never answers
		bufio.NewReader(agent).ReadBytes('\n')
	}()
	a := &agentClient{conn: client, r: bufio.NewReader(client), via: "tcp"}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	err := a.call(ctx, map[string]any{"cmd": "tree"}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("the call ended %v after it began; the context was cancelled at 100 ms", el)
	}
	if a.conn != nil {
		t.Error("the connection with a request in flight was kept")
	}
	// A context that is already done sends nothing.
	if err := a.call(ctx, map[string]any{"cmd": "tree"}, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("done context: err = %v", err)
	}
}
