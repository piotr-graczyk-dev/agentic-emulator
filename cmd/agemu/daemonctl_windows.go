//go:build windows

package main

import (
	"errors"
	"os"
	"os/exec"
	"time"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/client"
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/daemon"
	"github.com/piotr-graczyk-dev/agentic-emulator/internal/osproc"
)

// killAllDaemonsCommand is the command a person can run to end every agemud.
const killAllDaemonsCommand = "taskkill /IM agemud.exe /F"

// runDaemonForeground runs agemud in this console until it exits (Windows cannot replace a
// process), with this process's exit code. Ctrl+C reaches both, and agemud exits on it.
func runDaemonForeground(path string) error {
	cmd := exec.Command(path)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		os.Exit(exit.ExitCode())
	}
	return err
}

// askToExit asks the daemon with this pid to exit. Windows has no SIGTERM for a process without
// a console, so the request goes over the daemon's socket (the exit method), and the daemon
// shuts down as SIGTERM makes it do elsewhere. A daemon that does not answer, or is not the one
// on the socket, is ended at once. os.ErrProcessDone: it is gone.
func askToExit(pid int) error {
	if !osproc.Exists(pid) {
		return os.ErrProcessDone
	}
	if cl, ok := client.DialRunning(); ok {
		defer cl.Close()
		_ = cl.SetDeadline(time.Now().Add(pingTimeout))
		var p daemon.Ping
		if _, err := cl.Call("ping", nil, &p); err == nil && p.PID == pid {
			if _, err := cl.Call("exit", nil, nil); err == nil {
				return nil
			}
		}
	}
	return osproc.Kill(pid)
}
