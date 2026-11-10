// Package client talks to the agemu daemon over its unix socket and starts
// the daemon on demand.
package client

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/api"
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/daemon"
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/osproc"
)

type Client struct {
	mu   sync.Mutex
	conn net.Conn
	r    *bufio.Reader
	next int64
	// redial opens a new connection after the daemon closed this one (it stopped or restarted,
	// e.g. after an upgrade). nil for a client on a fixed connection.
	redial func() (net.Conn, error)
	broken bool // the connection failed mid-request; the next call redials first
}

// Dial connects to the daemon, starting it if it is not running. The client reconnects, and
// restarts the daemon, when the daemon goes away between calls.
func Dial() (*Client, error) {
	conn, err := dialDaemon()
	if err != nil {
		return nil, err
	}
	c := New(conn)
	c.redial = dialDaemon
	return c, nil
}

func dialDaemon() (net.Conn, error) {
	sock := daemon.SocketPath()
	conn, err := net.DialTimeout("unix", sock, time.Second)
	if err == nil {
		return conn, nil
	}
	if err := startDaemon(); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		conn, err = net.DialTimeout("unix", sock, time.Second)
		if err == nil {
			return conn, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("daemon did not start (see %s): %w", logPath(), err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// DialRunning connects to a daemon that already runs, without starting one; ok is false when
// none answers.
func DialRunning() (c *Client, ok bool) {
	conn, err := net.DialTimeout("unix", daemon.SocketPath(), time.Second)
	if err != nil {
		return nil, false
	}
	return New(conn), true
}

// LogPath is the daemon's log file.
func LogPath() string { return logPath() }

// New returns a client on an open connection to a daemon (tests pass one end of a net.Pipe
// served by daemon.ServeConn). It does not reconnect.
func New(conn net.Conn) *Client {
	return &Client{conn: conn, r: bufio.NewReaderSize(conn, 1<<20)}
}

func logPath() string { return filepath.Join(daemon.Home(), "daemon.log") }

// DaemonFile is agemud's file name on this OS (agemud.exe on Windows).
var DaemonFile = func() string {
	if runtime.GOOS == "windows" {
		return "agemud.exe"
	}
	return "agemud"
}()

// DaemonPath locates the agemud binary: $AGEMU_DAEMON, else next to the running
// executable, else on $PATH.
func DaemonPath() (string, error) {
	if p := os.Getenv("AGEMU_DAEMON"); p != "" {
		return p, nil
	}
	if exe, err := os.Executable(); err == nil {
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			exe = real
		}
		p := filepath.Join(filepath.Dir(exe), DaemonFile)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	if p, err := exec.LookPath("agemud"); err == nil {
		return p, nil
	}
	return "", errors.New("agemud not found next to agemu or on PATH (set AGEMU_DAEMON)")
}

func startDaemon() error {
	exe, err := DaemonPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(daemon.Home(), 0o755); err != nil {
		return err
	}
	lf, err := os.OpenFile(logPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer lf.Close()
	cmd := exec.Command(exe)
	cmd.Stdout, cmd.Stderr = lf, lf
	if err := osproc.StartDetached(cmd); err != nil {
		return err
	}
	return cmd.Process.Release()
}

func (c *Client) Close() error { return c.conn.Close() }

// SetDeadline bounds every call from now on (zero: no bound).
func (c *Client) SetDeadline(t time.Time) error { return c.conn.SetDeadline(t) }

// reconnect replaces a connection the daemon closed with a new one (starting the daemon again
// if needed).
func (c *Client) reconnect() error {
	conn, err := c.redial()
	if err != nil {
		c.broken = true
		return fmt.Errorf("lost the connection to agemud and could not reconnect: %w", err)
	}
	_ = c.conn.Close()
	c.conn, c.r, c.broken = conn, bufio.NewReaderSize(conn, 1<<20), false
	return nil
}

// Call performs one request and decodes the result into out (may be nil).
// It returns the server-side handling time in ms.
func (c *Client) Call(method string, params any, out any) (float64, error) {
	raw, ms, err := c.CallRaw(method, params)
	if err != nil {
		return ms, err
	}
	if out != nil {
		return ms, json.Unmarshal(raw, out)
	}
	return ms, nil
}

// CallRaw performs one request and returns its raw result and the server-side time in ms.
func (c *Client) CallRaw(method string, params any) (json.RawMessage, float64, error) {
	r, err := c.Do(method, params)
	return r.Result, r.Ms, err
}

// Reply is the daemon's answer to one request.
type Reply struct {
	Result json.RawMessage
	Ms     float64 // server-side handling time
	// AppCrash, when set, says that the app the agent launched crashed since the last call
	// (docs/CAPABILITIES.md, "The crash notice"). An error carries it in Error.Crash.
	AppCrash *api.AppCrash
}

// Do performs one request. A daemon error is an *Error.
func (c *Client) Do(method string, params any) (Reply, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.next++
	p, err := json.Marshal(params)
	if err != nil {
		return Reply{}, err
	}
	req, _ := json.Marshal(daemon.Request{ID: c.next, Method: method, Params: p})
	req = append(req, '\n')
	if c.broken {
		if err := c.reconnect(); err != nil {
			return Reply{}, err
		}
	}
	if _, err := c.conn.Write(req); err != nil {
		if c.redial == nil {
			// Not sent, and this client cannot reconnect: the caller may send it again on a new
			// connection (the MCP server does, once).
			return Reply{}, &SendError{Err: err}
		}
		// The daemon closed this connection before the request went out (it stopped or
		// restarted), so nothing ran: send it again on a new connection. This is the only resend
		// for a client from Dial; a failure here is a plain error, not a SendError, so no caller
		// sends it a third time.
		if err := c.reconnect(); err != nil {
			return Reply{}, err
		}
		if _, err := c.conn.Write(req); err != nil {
			c.broken = true
			return Reply{}, err
		}
	}
	line, err := c.r.ReadBytes('\n')
	if err != nil {
		if c.redial == nil {
			return Reply{}, err
		}
		c.broken = true
		return Reply{}, fmt.Errorf("lost the connection to agemud while it handled %s: the daemon stopped or restarted, so the call may or may not have taken effect; check before repeating it (%w)", method, err)
	}
	var resp daemon.Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return Reply{}, err
	}
	if resp.Error != "" {
		return Reply{Ms: resp.Ms}, &Error{Message: resp.Error, Code: resp.Code, Crash: resp.AppCrash}
	}
	return Reply{Result: resp.Result, Ms: resp.Ms, AppCrash: resp.AppCrash}, nil
}

// SendError is a request that could not be written to the daemon (the connection is broken, e.g.
// the daemon was restarted). The daemon did not get it, so it is safe to send it again on a new
// connection.
type SendError struct{ Err error }

func (e *SendError) Error() string { return "sending to the agemu daemon: " + e.Err.Error() }
func (e *SendError) Unwrap() error { return e.Err }

// Error is an error the daemon answered. It matches (errors.Is) api.ErrNotImplemented or
// api.ErrAppCrashed when the daemon marked it so: the capability has no implementation on the
// device's platform, or the call failed while the app the agent launched was down after a crash.
type Error struct {
	Message string
	Code    string        // daemon.CodeNotImplemented, daemon.CodeAppCrashed, daemon.CodePreparing or ""
	Crash   *api.AppCrash // the crash, with code app_crashed
}

func (e *Error) Error() string { return e.Message }

func (e *Error) Unwrap() error {
	switch e.Code {
	case daemon.CodeNotImplemented:
		return api.ErrNotImplemented
	case daemon.CodeAppCrashed:
		return api.ErrAppCrashed
	case daemon.CodePreparing:
		return api.ErrPreparing
	}
	return nil
}
