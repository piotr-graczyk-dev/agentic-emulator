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
	"sync"
	"syscall"
	"time"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/api"
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/daemon"
)

type Client struct {
	mu   sync.Mutex
	conn net.Conn
	r    *bufio.Reader
	next int64
}

// Dial connects to the daemon, starting it if it is not running.
func Dial() (*Client, error) {
	sock := daemon.SocketPath()
	conn, err := net.DialTimeout("unix", sock, time.Second)
	if err != nil {
		if err := startDaemon(); err != nil {
			return nil, err
		}
		deadline := time.Now().Add(10 * time.Second)
		for {
			conn, err = net.DialTimeout("unix", sock, time.Second)
			if err == nil {
				break
			}
			if time.Now().After(deadline) {
				return nil, fmt.Errorf("daemon did not start (see %s): %w", logPath(), err)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	return New(conn), nil
}

// New returns a client on an open connection to a daemon (Dial uses it; tests pass one end of
// a net.Pipe served by daemon.ServeConn).
func New(conn net.Conn) *Client {
	return &Client{conn: conn, r: bufio.NewReaderSize(conn, 1<<20)}
}

func logPath() string { return filepath.Join(daemon.Home(), "daemon.log") }

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
		p := filepath.Join(filepath.Dir(exe), "agemud")
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
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

func (c *Client) Close() error { return c.conn.Close() }

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
	if _, err := c.conn.Write(req); err != nil {
		return Reply{}, err
	}
	line, err := c.r.ReadBytes('\n')
	if err != nil {
		return Reply{}, err
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
