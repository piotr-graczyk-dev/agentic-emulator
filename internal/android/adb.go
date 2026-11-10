package android

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/toolchain"
)

// Minimal client for the adb server's smart-socket protocol (the same protocol the adb
// binary speaks to its server on 127.0.0.1:5037). Talking to the server directly avoids
// starting an adb process per call and lets us open device sockets ("localabstract:...")
// without installing port forwards in the shared adb server.

func adbServerAddr() string {
	port := os.Getenv("ANDROID_ADB_SERVER_PORT")
	if port == "" {
		port = "5037"
	}
	return "127.0.0.1:" + port
}

func adbRequest(c net.Conn, req string) error {
	if _, err := fmt.Fprintf(c, "%04x%s", len(req), req); err != nil {
		return err
	}
	var st [4]byte
	if _, err := io.ReadFull(c, st[:]); err != nil {
		return err
	}
	switch string(st[:]) {
	case "OKAY":
		return nil
	case "FAIL":
		msg, err := adbReadString(c)
		if err != nil {
			return err
		}
		return fmt.Errorf("adb: %s", msg)
	}
	return fmt.Errorf("adb: bad status %q", st[:])
}

func adbReadString(c net.Conn) (string, error) {
	var n [4]byte
	if _, err := io.ReadFull(c, n[:]); err != nil {
		return "", err
	}
	l, err := strconv.ParseUint(string(n[:]), 16, 32)
	if err != nil {
		return "", err
	}
	b := make([]byte, l)
	_, err = io.ReadFull(c, b)
	return string(b), err
}

// unblockOnCancel makes reads and writes on c fail at once when ctx is cancelled (net.Conn
// honours deadlines, not contexts): without it a wedged adbd or adb server would hold a call whose
// context was cancelled, such as a boot that `shutdown` cancels, until the read returned by
// itself. stop ends the watch and returns only once the watcher is gone, so a cancellation that
// comes later can no longer expire c; call it before reusing c with another context (and reset
// the deadline after it).
func unblockOnCancel(ctx context.Context, c net.Conn) (stop func()) {
	if ctx.Done() == nil {
		return func() {}
	}
	done := make(chan struct{})
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		select {
		case <-ctx.Done():
			c.SetDeadline(time.Now())
		case <-done:
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			close(done)
			<-exited
		})
	}
}

// ctxErr returns ctx's error when it is done (the reason a read failed), else err.
func ctxErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("%w (%v)", ctx.Err(), err)
	}
	return err
}

// adbOpen opens a stream to a device service (e.g. "localabstract:agemu-agent", "shell,v2,raw:ls").
func adbOpen(ctx context.Context, serial, service string) (net.Conn, error) {
	d := net.Dialer{Timeout: 2 * time.Second}
	c, err := d.DialContext(ctx, "tcp", adbServerAddr())
	if err != nil {
		return nil, fmt.Errorf("adb server not reachable at %s (is adb installed?): %w", adbServerAddr(), err)
	}
	if dl, ok := ctx.Deadline(); ok {
		c.SetDeadline(dl)
	}
	stop := unblockOnCancel(ctx, c)
	defer stop()
	if err := adbRequest(c, "host:transport:"+serial); err != nil {
		c.Close()
		return nil, ctxErr(ctx, err)
	}
	if err := adbRequest(c, service); err != nil {
		c.Close()
		return nil, ctxErr(ctx, err)
	}
	stop()
	if ctx.Err() != nil {
		c.Close()
		return nil, ctx.Err()
	}
	c.SetDeadline(time.Time{})
	return c, nil
}

// adbHostQuery runs a host service that returns one length-prefixed string (e.g. "host:get-state").
func adbHostQuery(ctx context.Context, req string) (string, error) {
	d := net.Dialer{Timeout: 2 * time.Second}
	c, err := d.DialContext(ctx, "tcp", adbServerAddr())
	if err != nil {
		return "", err
	}
	defer c.Close()
	if dl, ok := ctx.Deadline(); ok {
		c.SetDeadline(dl)
	}
	defer unblockOnCancel(ctx, c)()
	if err := adbRequest(c, req); err != nil {
		return "", ctxErr(ctx, err)
	}
	s, err := adbReadString(c)
	return s, ctxErr(ctx, err)
}

// adbState returns "device", "offline", ... for the serial, or an error when unknown.
func adbState(ctx context.Context, serial string) (string, error) {
	return adbHostQuery(ctx, "host-serial:"+serial+":get-state")
}

type shellResult struct {
	Stdout, Stderr string
	Exit           int
}

// adbShell runs a shell command with the v2 protocol (separate stdout/stderr + exit code).
func adbShell(ctx context.Context, serial, cmd string) (shellResult, error) {
	var res shellResult
	c, err := adbOpen(ctx, serial, "shell,v2,raw:"+cmd)
	if err != nil {
		return res, err
	}
	defer c.Close()
	if dl, ok := ctx.Deadline(); ok {
		c.SetDeadline(dl)
	}
	defer unblockOnCancel(ctx, c)()
	var out, errb bytes.Buffer
	var hdr [5]byte
	for {
		if _, err := io.ReadFull(c, hdr[:]); err != nil {
			if errors.Is(err, io.EOF) {
				return res, fmt.Errorf("adb shell %q: connection closed without exit status", cmd)
			}
			return res, ctxErr(ctx, err)
		}
		n := binary.LittleEndian.Uint32(hdr[1:])
		buf := make([]byte, n)
		if _, err := io.ReadFull(c, buf); err != nil {
			return res, ctxErr(ctx, err)
		}
		switch hdr[0] {
		case 1:
			out.Write(buf)
		case 2:
			errb.Write(buf)
		case 3:
			res.Stdout, res.Stderr = out.String(), errb.String()
			if len(buf) > 0 {
				res.Exit = int(buf[0])
			}
			return res, nil
		}
	}
}

// adbShellOK runs a command and fails on a non-zero exit status.
func adbShellOK(ctx context.Context, serial, cmd string) (string, error) {
	r, err := adbShell(ctx, serial, cmd)
	if err != nil {
		return "", err
	}
	if r.Exit != 0 {
		return r.Stdout, fmt.Errorf("adb shell %q: exit %d: %s", cmd, r.Exit, strings.TrimSpace(r.Stderr+" "+r.Stdout))
	}
	return r.Stdout, nil
}

// ensureADBKey makes sure the host has an adb key before an emulator starts. The emulator
// installs the host's public key (adbkey.pub in the Android user directory) into the guest as it
// boots, and the Play Store images accept adb only from such a key. On a computer where adb never
// ran (a new Linux or Windows install, CI) there is none yet: the adb server creates it when it
// first starts, which would be after the emulator, and every adb call would then fail with
// "device unauthorized". Starting the server first creates the key; with a key present this does
// nothing.
func ensureADBKey(ctx context.Context) {
	home, _ := os.UserHomeDir()
	if adbKeyPresent(home, os.Getenv) {
		return
	}
	c, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(c, toolchain.ADB(), "start-server").CombinedOutput(); err != nil {
		logf("adb start-server (to create the adb key before the emulator starts): %v: %s", err, strings.TrimSpace(string(out)))
	}
}

// adbKeyPresent reports whether an adb public key exists where adb and the emulator look for it.
func adbKeyPresent(home string, getenv func(string) string) bool {
	var dirs []string
	for _, k := range []string{"ANDROID_USER_HOME", "ANDROID_EMULATOR_HOME"} {
		if v := getenv(k); v != "" {
			dirs = append(dirs, v)
		}
	}
	if v := getenv("ANDROID_SDK_HOME"); v != "" {
		dirs = append(dirs, filepath.Join(v, ".android"))
	}
	dirs = append(dirs, filepath.Join(home, ".android"))
	for _, d := range dirs {
		if st, err := os.Stat(filepath.Join(d, "adbkey.pub")); err == nil && !st.IsDir() {
			return true
		}
	}
	return false
}

// adbExec runs the adb binary (used for push / install, which need the sync protocol).
func adbExec(ctx context.Context, serial string, args ...string) (string, error) {
	full := append([]string{"-s", serial}, args...)
	out, err := exec.CommandContext(ctx, toolchain.ADB(), full...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("adb %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}
