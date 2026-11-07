package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/api"
)

// Request / Response are newline-delimited JSON over the daemon's unix socket.
type Request struct {
	ID     int64           `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

type Response struct {
	ID     int64           `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
	// Code classifies Error for programs: CodeNotImplemented, or empty.
	Code string `json:"code,omitempty"`
	// Ms is the server-side handling time, for benchmarks.
	Ms float64 `json:"ms"`
}

// CodeNotImplemented marks an error that matches api.ErrNotImplemented: the capability has no
// implementation on the device's platform, so retrying or changing the arguments cannot help.
const CodeNotImplemented = "not_implemented"

// errorCode returns the Response code for err.
func errorCode(err error) string {
	if errors.Is(err, api.ErrNotImplemented) {
		return CodeNotImplemented
	}
	return ""
}

// Home returns the agemu state directory ($AGEMU_HOME or ~/.agemu).
func Home() string {
	if h := os.Getenv("AGEMU_HOME"); h != "" {
		return h
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".agemu")
}

func SocketPath() string { return filepath.Join(Home(), "agemu.sock") }

// Serve listens on the unix socket until ctx is cancelled.
func Serve(ctx context.Context, svc *Service) error {
	if err := os.MkdirAll(Home(), 0o755); err != nil {
		return err
	}
	// Two agemu processes can both find no socket and each start a daemon. The second must
	// not unlink the first one's socket, or two daemons would drive the same devices.
	lock, err := os.OpenFile(filepath.Join(Home(), "agemud.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("another agemud already runs for " + Home())
	}
	sock := SocketPath()
	if c, err := net.DialTimeout("unix", sock, 200*time.Millisecond); err == nil {
		c.Close()
		return errors.New("daemon already running at " + sock)
	}
	_ = os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	log.Printf("agemu daemon listening on %s (pid %d)", sock, os.Getpid())
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go handle(ctx, svc, conn)
	}
}

// safeCall runs one request and turns a panic into an error, so one bad request cannot take
// down the daemon that every agent shares.
func safeCall(ctx context.Context, svc *Service, req Request) (res any, err error) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("panic in %s: %v\n%s", req.Method, r, debug.Stack())
			res, err = nil, fmt.Errorf("internal error in %s: %v", req.Method, r)
		}
	}()
	return svc.Call(ctx, req.Method, req.Params)
}

// ServeConn serves requests on one connection until it closes (Serve does this for every
// accepted connection; tests use it on a net.Pipe).
func ServeConn(ctx context.Context, svc *Service, conn net.Conn) { handle(ctx, svc, conn) }

func handle(ctx context.Context, svc *Service, conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReaderSize(conn, 1<<20)
	w := bufio.NewWriter(conn)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) == 0 && err != nil {
			return
		}
		var req Request
		var resp Response
		start := time.Now()
		if jerr := json.Unmarshal(line, &req); jerr != nil {
			resp.Error = "bad request: " + jerr.Error()
		} else {
			resp.ID = req.ID
			res, cerr := safeCall(ctx, svc, req)
			if cerr != nil {
				resp.Error = cerr.Error()
				resp.Code = errorCode(cerr)
			} else {
				resp.Result, _ = json.Marshal(res)
			}
		}
		resp.Ms = float64(time.Since(start).Microseconds()) / 1000
		b, _ := json.Marshal(resp)
		w.Write(b)
		w.WriteByte('\n')
		if err := w.Flush(); err != nil {
			return
		}
		if err != nil {
			return
		}
	}
}
