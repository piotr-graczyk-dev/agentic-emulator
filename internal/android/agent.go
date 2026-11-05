package android

import (
	"bufio"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The on-device agent (android-agent/, built by android-agent/build.sh).
//
//go:embed assets/agemu-agent.jar
var agentJar []byte

var agentVersion = func() string { s := sha256.Sum256(agentJar); return hex.EncodeToString(s[:]) }()

const (
	agentDevicePath = "/data/local/tmp/agemu-agent.jar"
	agentSocket     = "agemu-agent"
)

// agentClient keeps one connection to the agent and serializes requests on it.
type agentClient struct {
	serial string
	mu     sync.Mutex
	conn   net.Conn
	r      *bufio.Reader
	next   int64
}

type agentResp struct {
	ID     int64           `json:"id"`
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Error  string          `json:"error"`
}

func (a *agentClient) closeLocked() {
	if a.conn != nil {
		a.conn.Close()
		a.conn, a.r = nil, nil
	}
}

func (a *agentClient) Close() {
	a.mu.Lock()
	a.closeLocked()
	a.mu.Unlock()
}

// call sends one request. A broken connection is re-dialled once (the agent survives
// snapshot loads, but the adb stream to it does not).
func (a *agentClient) call(ctx context.Context, req map[string]any, out any) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if a.conn == nil {
			c, err := adbOpen(ctx, a.serial, "localabstract:"+agentSocket)
			if err != nil {
				return &agentDialError{err}
			}
			a.conn, a.r = c, bufio.NewReaderSize(c, 1<<20)
		}
		a.next++
		req["id"] = a.next
		b, _ := json.Marshal(req)
		b = append(b, '\n')
		if dl, ok := ctx.Deadline(); ok {
			a.conn.SetDeadline(dl)
		} else {
			a.conn.SetDeadline(time.Now().Add(60 * time.Second))
		}
		if _, err := a.conn.Write(b); err != nil {
			lastErr = err
			a.closeLocked()
			continue
		}
		line, err := a.r.ReadBytes('\n')
		if err != nil {
			lastErr = err
			a.closeLocked()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			continue
		}
		var resp agentResp
		if err := json.Unmarshal(line, &resp); err != nil {
			a.closeLocked()
			return fmt.Errorf("agent: bad response: %w", err)
		}
		if !resp.OK {
			return fmt.Errorf("agent: %s", resp.Error)
		}
		_ = errNoAgent
		if out != nil {
			return json.Unmarshal(resp.Result, out)
		}
		return nil
	}
	return fmt.Errorf("agent: connection failed: %w", lastErr)
}

type agentPing struct {
	Version       string `json:"version"`
	PID           int    `json:"pid"`
	BootCompleted string `json:"bootCompleted"`
}

type displayInfo struct {
	W           int `json:"w"`
	H           int `json:"h"`
	Density     int `json:"density"`
	Rotation    int `json:"rotation"`
	PhysW       int `json:"physW"`
	PhysH       int `json:"physH"`
	PhysDensity int `json:"physDensity"`
}

func (a *agentClient) ping(ctx context.Context) (agentPing, error) {
	var p agentPing
	err := a.call(ctx, map[string]any{"cmd": "ping"}, &p)
	return p, err
}

func (a *agentClient) display(ctx context.Context) (displayInfo, error) {
	var d displayInfo
	err := a.call(ctx, map[string]any{"cmd": "display"}, &d)
	return d, err
}

// ensureAgent makes sure the current agent version is running on the device and answers.
// Returns whether it had to (re)start the agent.
func ensureAgent(ctx context.Context, a *agentClient, logf func(string, ...any)) (bool, error) {
	pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	p, err := a.ping(pctx)
	cancel()
	if err == nil && p.Version == agentVersion {
		return false, nil
	}
	if err == nil {
		logf("agent version %.12s on device, want %.12s: restarting", p.Version, agentVersion)
		_ = a.call(ctx, map[string]any{"cmd": "exit"}, nil)
		a.Close()
	}
	if err := installAgent(ctx, a.serial); err != nil {
		return true, err
	}
	// --nice-name makes the process easy to find (pidof agemu-agent).
	start := fmt.Sprintf("for p in $(pidof agemu-agent); do kill $p; done; CLASSPATH=%s nohup app_process /system/bin --nice-name=agemu-agent agemu.Agent %s >/data/local/tmp/agemu-agent.log 2>&1 &", agentDevicePath, agentSocket)
	if _, err := adbShell(ctx, a.serial, start); err != nil {
		return true, fmt.Errorf("starting agent: %w", err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		pctx, cancel := context.WithTimeout(ctx, time.Second)
		p, err = a.ping(pctx)
		cancel()
		if err == nil && p.Version == agentVersion {
			return true, nil
		}
		if time.Now().After(deadline) {
			log, _ := adbShell(ctx, a.serial, "cat /data/local/tmp/agemu-agent.log")
			if err == nil {
				err = fmt.Errorf("version %s", p.Version)
			}
			return true, fmt.Errorf("agent did not start: %v; log: %s", err, lastLines(log.Stdout, 8))
		}
		select {
		case <-ctx.Done():
			return true, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// installAgent pushes the embedded jar when the device copy differs.
func installAgent(ctx context.Context, serial string) error {
	r, err := adbShell(ctx, serial, "sha256sum "+agentDevicePath+" 2>/dev/null")
	if err == nil && strings.HasPrefix(r.Stdout, agentVersion) {
		return nil
	}
	tmp := filepath.Join(os.TempDir(), "agemu-agent-"+agentVersion[:12]+".jar")
	if err := os.WriteFile(tmp, agentJar, 0o644); err != nil {
		return err
	}
	defer os.Remove(tmp)
	if _, err := adbExec(ctx, serial, "push", tmp, agentDevicePath); err != nil {
		return err
	}
	return nil
}

var errNoAgent = errors.New("agent not connected")

// agentDialError means the agent's socket could not be opened: the request was not sent,
// so it is safe to restart the agent and retry.
type agentDialError struct{ err error }

func (e *agentDialError) Error() string { return "agent unreachable: " + e.err.Error() }
func (e *agentDialError) Unwrap() error { return e.err }
