package ios

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// HelperName is the helper executable built from ios-helper/.
const HelperName = "agemu-ios-helper"

// HelperPath finds the helper: $AGEMU_IOS_HELPER, else next to the running executable.
func HelperPath() (string, error) {
	if p := os.Getenv("AGEMU_IOS_HELPER"); p != "" {
		return p, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	p := filepath.Join(filepath.Dir(exe), HelperName)
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("%s not found next to %s (build it with `make ios-helper`, or set AGEMU_IOS_HELPER)", HelperName, exe)
	}
	return p, nil
}

type helperResponse struct {
	ID     int64           `json:"id"`
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Error  string          `json:"error"`
	Ms     float64         `json:"ms"`
}

// helper is one running agemu-ios-helper process. Requests are multiplexed by id, so
// concurrent callers share the process.
type helper struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	writeMu sync.Mutex

	mu      sync.Mutex
	next    int64
	pending map[int64]chan helperResponse
	done    chan struct{}
	err     error
}

func startHelper() (*helper, error) {
	path, err := HelperPath()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(path)
	cmd.Stderr = os.Stderr // the daemon's log
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting %s: %w", path, err)
	}
	h := &helper{cmd: cmd, stdin: stdin, pending: map[int64]chan helperResponse{}, done: make(chan struct{})}
	go h.read(stdout)
	go func() {
		werr := cmd.Wait()
		h.fail(fmt.Errorf("%s exited: %v", HelperName, werr))
	}()
	return h, nil
}

func (h *helper) read(r io.Reader) {
	br := bufio.NewReaderSize(r, 1<<20)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			var resp helperResponse
			if jerr := json.Unmarshal(line, &resp); jerr != nil {
				log.Printf("ios helper: bad response line: %v", jerr)
			} else {
				h.mu.Lock()
				ch := h.pending[resp.ID]
				delete(h.pending, resp.ID)
				h.mu.Unlock()
				if ch != nil {
					ch <- resp
				}
			}
		}
		if err != nil {
			h.fail(fmt.Errorf("%s stdout closed: %v", HelperName, err))
			return
		}
	}
}

// fail marks the helper dead and fails every pending call.
func (h *helper) fail(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.err != nil {
		return
	}
	h.err = err
	close(h.done)
	for id, ch := range h.pending {
		ch <- helperResponse{ID: id, Error: err.Error()}
		delete(h.pending, id)
	}
}

func (h *helper) alive() bool {
	select {
	case <-h.done:
		return false
	default:
		return true
	}
}

func (h *helper) close() {
	_ = h.stdin.Close()
	select {
	case <-h.done:
	case <-time.After(2 * time.Second):
		_ = h.cmd.Process.Kill()
	}
}

// call sends one request. params may be nil. out (optional) receives the result.
func (h *helper) call(ctx context.Context, cmd string, params map[string]any, out any) error {
	req := map[string]any{}
	for k, v := range params {
		req[k] = v
	}
	h.mu.Lock()
	if h.err != nil {
		err := h.err
		h.mu.Unlock()
		return err
	}
	h.next++
	id := h.next
	ch := make(chan helperResponse, 1)
	h.pending[id] = ch
	h.mu.Unlock()

	req["id"] = id
	req["cmd"] = cmd
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	h.writeMu.Lock()
	_, err = h.stdin.Write(b)
	h.writeMu.Unlock()
	if err != nil {
		h.fail(fmt.Errorf("writing to %s: %w", HelperName, err))
		return err
	}
	select {
	case resp := <-ch:
		if !resp.OK {
			if resp.Error == "" {
				resp.Error = "unknown helper error"
			}
			return errors.New(resp.Error)
		}
		if out != nil {
			return json.Unmarshal(resp.Result, out)
		}
		return nil
	case <-ctx.Done():
		h.mu.Lock()
		delete(h.pending, id)
		h.mu.Unlock()
		return fmt.Errorf("ios helper %s: %w", cmd, ctx.Err())
	}
}
