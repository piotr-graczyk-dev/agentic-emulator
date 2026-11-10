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
	"time"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/osproc"
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

// dial opens a connection to the agent: TCP when available, adb otherwise. The TCP attempt gets
// at most half of ctx's remaining time: a forwarded connection the guest never answers (its
// network not up yet on a slow host) would otherwise use all of it, and adb none.
func (a *agentClient) dial(ctx context.Context) (net.Conn, *bufio.Reader, string, error) {
	var tcpErr error
	if a.tcp != nil && a.token != "" {
		tctx := ctx
		if dl, ok := ctx.Deadline(); ok {
			var cancel context.CancelFunc
			tctx, cancel = context.WithDeadline(ctx, time.Now().Add(time.Until(dl)/2))
			defer cancel()
		}
		for attempt := 0; attempt < 2; attempt++ {
			hp, err := a.tcp(tctx, attempt > 0)
			if err != nil {
				tcpErr = err
				break
			}
			c, r, err := dialAgentTCP(tctx, hp, a.token)
			if err == nil {
				return c, r, "tcp", nil
			}
			tcpErr = err
			if !osproc.IsConnRefused(err) {
				break
			}
		}
	}
	c, err := adbOpen(ctx, a.serial, "localabstract:"+agentSocket)
	if err != nil {
		if tcpErr != nil {
			return nil, nil, "", fmt.Errorf("%w (and over TCP: %v)", err, tcpErr)
		}
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
// snapshot loads, but the adb stream to it does not). The call ends with ctx: a cancelled
// context (a wait that learned the app crashed, a client that went away) interrupts the read,
// and the connection is dropped (the agent's late answer would be taken for the next call's);
// the next call dials anew.
func (a *agentClient) call(ctx context.Context, req map[string]any, out any) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
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
		conn := a.conn
		stop := context.AfterFunc(ctx, func() { conn.SetDeadline(time.Unix(1, 0)) })
		if _, err := a.conn.Write(b); err != nil {
			stop()
			lastErr = err
			a.closeLocked()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			continue
		}
		line, err := a.r.ReadBytes('\n')
		if !stop() && err == nil {
			// ctx ended just as the answer came: the answer is complete, but the connection's
			// deadline is in the past now.
			a.closeLocked()
		}
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
	started, err := adbShell(ctx, a.serial, start)
	if err != nil {
		return true, fmt.Errorf("starting agent: %w", err)
	}
	deadline := time.Now().Add(slowHost(60 * time.Second)) // app_process + UiAutomation connect can be slow on a loaded host
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
			msg := fmt.Sprintf("agent did not start: %v; log: %s", err, lastLines(log.Stdout, 8))
			if strings.TrimSpace(log.Stdout) == "" { // it never ran: say what the device shows
				diag, derr := adbShell(ctx, a.serial, "ls -l "+agentDevicePath+" /data/local/tmp/agemu-agent.log; pidof agemu-agent; getprop sys.boot_completed")
				msg += fmt.Sprintf("; start command: exit %d %q; device: %q %q %v", started.Exit,
					strings.TrimSpace(started.Stdout+started.Stderr), strings.TrimSpace(diag.Stdout), strings.TrimSpace(diag.Stderr), derr)
				// Run it in the foreground for a while to see what it says, and what the system logged.
				fg, _ := adbShell(ctx, a.serial, fmt.Sprintf("%sCLASSPATH=%s timeout 20 app_process /system/bin --nice-name=agemu-agent agemu.Agent %s %d 2>&1 | tail -n 15; echo \"exit $?\"; logcat -d -t 2000 | grep -i -E 'agemu|app_process|AndroidRuntime|lowmemorykiller|lmkd|DEBUG' | tail -n 25", tcpArgs, agentDevicePath, agentSocket, agentGuestPort))
				msg += fmt.Sprintf("; in the foreground: %q", strings.TrimSpace(fg.Stdout+fg.Stderr))
			}
			return true, errors.New(msg)
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
// It lives in the AVD directory, next to the snapshot that embeds it, so every daemon
// (whatever its $AGEMU_HOME) agrees with the snapshot. A token from the old location
// ($AGEMU_HOME/android) is moved there once.
func agentToken(avd string) string {
	read := func(p string) string {
		if b, err := os.ReadFile(p); err == nil && len(strings.TrimSpace(string(b))) >= 32 {
			return strings.TrimSpace(string(b))
		}
		return ""
	}
	legacy := filepath.Join(logDir(), avd+".agent-token")
	a, err := loadAVD(avd)
	if err != nil {
		return read(legacy)
	}
	path := filepath.Join(a.Dir, "agemu-agent-token")
	if tok := read(path); tok != "" {
		return tok
	}
	if tok := read(legacy); tok != "" {
		if os.WriteFile(path, []byte(tok+"\n"), 0o600) == nil {
			_ = os.Remove(legacy)
		}
		return tok
	}
	var r [16]byte
	if _, err := rand.Read(r[:]); err != nil {
		return ""
	}
	tok := hex.EncodeToString(r[:])
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
