package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"os"
	"path/filepath"
	"time"
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
	// Ms is the server-side handling time, for benchmarks.
	Ms float64 `json:"ms"`
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
			res, cerr := svc.Call(ctx, req.Method, req.Params)
			if cerr != nil {
				resp.Error = cerr.Error()
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
