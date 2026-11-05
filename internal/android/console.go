package android

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Minimal client for the emulator console (telnet protocol on the console port, the one
// `adb emu` talks to). agemu uses it for one thing: a port redirection from the host's
// loopback to the guest, so the host reaches the on-device agent over TCP without adb.
// Unlike adb, which re-handshakes after every snapshot load, the redirection lives in the
// emulator process and the guest's listening socket is part of the snapshot: a restored
// guest answers the moment its vCPUs run again.

// agentGuestPort is the guest TCP port the agent listens on (next to its abstract socket).
const agentGuestPort = 39271

func consoleToken() (string, error) {
	home, _ := os.UserHomeDir()
	b, err := os.ReadFile(filepath.Join(home, ".emulator_console_auth_token"))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// consoleRun sends commands to the emulator console and returns each reply (without "OK")
// and its error ("KO: ..."). A failing command does not stop the following ones.
func consoleRun(ctx context.Context, port int, cmds ...string) ([]string, []error, error) {
	d := net.Dialer{Timeout: 2 * time.Second}
	c, err := d.DialContext(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, nil, err
	}
	defer c.Close()
	dl := time.Now().Add(5 * time.Second)
	if cd, ok := ctx.Deadline(); ok && cd.Before(dl) {
		dl = cd
	}
	c.SetDeadline(dl)
	r := bufio.NewReader(c)
	read := func() (string, error, error) { // reply, command error, I/O error
		var b strings.Builder
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return b.String(), nil, err
			}
			t := strings.TrimRight(line, "\r\n")
			if t == "OK" {
				return b.String(), nil, nil
			}
			if strings.HasPrefix(t, "KO") {
				return b.String(), fmt.Errorf("console: %s", t), nil
			}
			b.WriteString(t)
			b.WriteByte('\n')
		}
	}
	if _, _, err := read(); err != nil { // banner
		return nil, nil, err
	}
	if tok, err := consoleToken(); err == nil && tok != "" {
		if _, err := fmt.Fprintf(c, "auth %s\n", tok); err != nil {
			return nil, nil, err
		}
		if _, cerr, err := read(); err != nil || cerr != nil {
			return nil, nil, errors.Join(err, cerr)
		}
	}
	out, errs := make([]string, len(cmds)), make([]error, len(cmds))
	for i, cmd := range cmds {
		if _, err := fmt.Fprintf(c, "%s\n", cmd); err != nil {
			return out, errs, err
		}
		var ioErr error
		out[i], errs[i], ioErr = read()
		if ioErr != nil {
			return out, errs, ioErr
		}
	}
	return out, errs, nil
}

// ensureRedir (re)creates the redirection 127.0.0.1:<host port> -> guest:<guestPort> and
// returns the host port. A snapshot load drops the host listener while `redir list` still
// shows the entry, so an existing entry is deleted and added again. prev is the host port
// used before (0: reuse the listed one or pick a free port).
func ensureRedir(ctx context.Context, console, guestPort, prev int) (int, error) {
	hp := prev
	if hp == 0 {
		out, _, err := consoleRun(ctx, console, "redir list")
		if err != nil {
			return 0, err
		}
		// "ipv4 tcp:5590  => 39271 "
		for _, line := range strings.Split(out[0], "\n") {
			f := strings.Fields(line)
			if len(f) >= 4 && f[0] == "ipv4" && strings.HasPrefix(f[1], "tcp:") && f[2] == "=>" && f[3] == strconv.Itoa(guestPort) {
				hp, _ = strconv.Atoi(strings.TrimPrefix(f[1], "tcp:"))
			}
		}
	}
	for i := 0; i < 5; i++ {
		if hp == 0 {
			l, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				return 0, err
			}
			hp = l.Addr().(*net.TCPAddr).Port
			l.Close()
		}
		_, errs, err := consoleRun(ctx, console, fmt.Sprintf("redir del tcp:%d", hp), fmt.Sprintf("redir add tcp:%d:%d", hp, guestPort))
		if err != nil {
			return 0, err
		}
		if errs[1] == nil {
			return hp, nil
		}
		hp = 0 // host port taken: pick another
	}
	return 0, fmt.Errorf("could not add a console port redirection to guest port %d", guestPort)
}
