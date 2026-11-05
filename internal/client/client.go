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
	return &Client{conn: conn, r: bufio.NewReaderSize(conn, 1<<20)}, nil
}

func logPath() string { return filepath.Join(daemon.Home(), "daemon.log") }

func startDaemon() error {
	exe, err := os.Executable()
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
	cmd := exec.Command(exe, "daemon")
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

func (c *Client) CallRaw(method string, params any) (json.RawMessage, float64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.next++
	p, err := json.Marshal(params)
	if err != nil {
		return nil, 0, err
	}
	req, _ := json.Marshal(daemon.Request{ID: c.next, Method: method, Params: p})
	req = append(req, '\n')
	if _, err := c.conn.Write(req); err != nil {
		return nil, 0, err
	}
	line, err := c.r.ReadBytes('\n')
	if err != nil {
		return nil, 0, err
	}
	var resp daemon.Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, 0, err
	}
	if resp.Error != "" {
		return nil, resp.Ms, errors.New(resp.Error)
	}
	return resp.Result, resp.Ms, nil
}
