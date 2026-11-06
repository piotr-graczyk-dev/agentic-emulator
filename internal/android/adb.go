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
	"strconv"
	"strings"
	"time"
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
	if err := adbRequest(c, "host:transport:"+serial); err != nil {
		c.Close()
		return nil, err
	}
	if err := adbRequest(c, service); err != nil {
		c.Close()
		return nil, err
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
	if err := adbRequest(c, req); err != nil {
		return "", err
	}
	return adbReadString(c)
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
	var out, errb bytes.Buffer
	var hdr [5]byte
	for {
		if _, err := io.ReadFull(c, hdr[:]); err != nil {
			if errors.Is(err, io.EOF) {
				return res, fmt.Errorf("adb shell %q: connection closed without exit status", cmd)
			}
			return res, err
		}
		n := binary.LittleEndian.Uint32(hdr[1:])
		buf := make([]byte, n)
		if _, err := io.ReadFull(c, buf); err != nil {
			return res, err
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

// adbExec runs the adb binary (used for push / install, which need the sync protocol).
func adbExec(ctx context.Context, serial string, args ...string) (string, error) {
	full := append([]string{"-s", serial}, args...)
	out, err := exec.CommandContext(ctx, sdkTool("platform-tools", "adb"), full...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("adb %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}
