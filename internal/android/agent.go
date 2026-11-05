package android

import (
	"bufio"
	"context"
	"crypto/rand"
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
	"syscall"
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
//
// Transport: TCP through an emulator console port redirection when tcp is set (works the
// moment a restored snapshot resumes; see console.go), otherwise the adb server's
// "localabstract:agemu-agent" stream. A refused TCP connection (a snapshot load dropped the
// redirection) re-creates the redirection once; any other TCP failure falls back to adb.
type agentClient struct {
	serial string
	// tcp returns the host port of the redirection to the agent's guest TCP port, re-creating
	// it when refresh is set. nil: adb only.
	tcp   func(ctx context.Context, refresh bool) (int, error)
	rd    *redirector // owner of tcp (nil: adb only)
	token string      // shared secret the agent's TCP listener requires
	mu    sync.Mutex
	conn  net.Conn
	r     *bufio.Reader
	next  int64
	via   string // "tcp" or "adb": transport of the current connection
}

// transport reports how the current connection reaches the agent ("tcp", "adb" or "").
func (a *agentClient) transport() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.via
}

// dial opens a connection to the agent: TCP when available, adb otherwise.
func (a *agentClient) dial(ctx context.Context) (net.Conn, *bufio.Reader, string, error) {
	if a.tcp != nil && a.token != "" {
		for attempt := 0; attempt < 2; attempt++ {
			hp, err := a.tcp(ctx, attempt > 0)
			if err != nil {
				break
			}
			c, r, err := dialAgentTCP(ctx, hp, a.token)
			if err == nil {
				return c, r, "tcp", nil
			}
			if !errors.Is(err, syscall.ECONNREFUSED) {
				break
			}
		}
	}
	c, err := adbOpen(ctx, a.serial, "localabstract:"+agentSocket)
	if err != nil {
		return nil, nil, "", err
	}
	return c, bufio.NewReaderSize(c, 1<<20), "adb", nil
}

func dialAgentTCP(ctx context.Context, port int, token string) (net.Conn, *bufio.Reader, error) {
	d := net.Dialer{Timeout: time.Second}
	c, err := d.DialContext(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, nil, err
	}
	dl := time.Now().Add(2 * time.Second)
	if cd, ok := ctx.Deadline(); ok && cd.Before(dl) {
		dl = cd
	}
	c.SetDeadline(dl)
	b, _ := json.Marshal(map[string]string{"auth": token})
	r := bufio.NewReaderSize(c, 1<<20)
	if _, err := c.Write(append(b, '\n')); err != nil {
		c.Close()
		return nil, nil, err
	}
	line, err := r.ReadBytes('\n')
	if err != nil {
		c.Close()
		return nil, nil, fmt.Errorf("agent tcp handshake: %w", err) // e.g. the agent has no TCP listener
	}
	var resp agentResp
	if json.Unmarshal(line, &resp) != nil || !resp.OK {
		c.Close()
		return nil, nil, fmt.Errorf("agent tcp handshake: %s", strings.TrimSpace(string(line)))
	}
	c.SetDeadline(time.Time{})
	return c, r, nil
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
			c, r, via, err := a.dial(ctx)
			if err != nil {
				return &agentDialError{err}
			}
			a.conn, a.r, a.via = c, r, via
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
	TCP           int    `json:"tcp"` // guest TCP port the agent listens on (0: none)
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
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
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
	if err := waitDevice(ctx, a.serial, 60*time.Second); err != nil {
		return true, err
	}
	if err := installAgent(ctx, a.serial); err != nil {
		return true, err
	}
	// --nice-name makes the process easy to find (pidof agemu-agent). The token reaches the
	// agent through its environment, which other guest apps cannot read.
	tcpArgs := ""
	if a.token != "" {
		tcpArgs = fmt.Sprintf("AGEMU_TOKEN=%s ", a.token)
	}
	start := fmt.Sprintf("for p in $(pidof agemu-agent); do kill $p; done; %sCLASSPATH=%s nohup app_process /system/bin --nice-name=agemu-agent agemu.Agent %s %d >/data/local/tmp/agemu-agent.log 2>&1 &", tcpArgs, agentDevicePath, agentSocket, agentGuestPort)
	if _, err := adbShell(ctx, a.serial, start); err != nil {
		return true, fmt.Errorf("starting agent: %w", err)
	}
	deadline := time.Now().Add(60 * time.Second) // app_process + UiAutomation connect can be slow on a loaded host
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

// agentToken returns the per-AVD secret for the agent's TCP listener, creating it once. It is
// stable so that the agent inside the baseline snapshot and a restarted agent accept it alike.
func agentToken(avd string) string {
	path := filepath.Join(logDir(), avd+".agent-token")
	if b, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(b))) >= 32 {
		return strings.TrimSpace(string(b))
	}
	var r [16]byte
	if _, err := rand.Read(r[:]); err != nil {
		return ""
	}
	tok := hex.EncodeToString(r[:])
	if err := os.MkdirAll(logDir(), 0o755); err != nil {
		return ""
	}
	if err := os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
		return ""
	}
	return tok
}

// waitDevice waits until the adb server reports the device online (it re-handshakes after
// every snapshot load, which agemu itself does not wait for).
func waitDevice(ctx context.Context, serial string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		c, cancel := context.WithTimeout(ctx, 2*time.Second)
		st, err := adbState(c, serial)
		cancel()
		if err == nil && st == "device" {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s not online in adb after %v (state %q, %v)", serial, timeout, st, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// agentDialError means the agent's socket could not be opened: the request was not sent,
// so it is safe to restart the agent and retry.
type agentDialError struct{ err error }

func (e *agentDialError) Error() string { return "agent unreachable: " + e.err.Error() }
func (e *agentDialError) Unwrap() error { return e.err }
