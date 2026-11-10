//go:build !windows

package main

import (
	"os"
	"syscall"

	"github.com/piotr-graczyk-dev/agentic-emulator/internal/osproc"
)

// killAllDaemonsCommand is the command a person can run to end every agemud.
const killAllDaemonsCommand = "pkill -x agemud"

// runDaemonForeground replaces this process with agemud, so signals reach the daemon directly.
func runDaemonForeground(path string) error {
	return syscall.Exec(path, []string{path}, os.Environ())
}

// askToExit asks the daemon with this pid to exit: SIGTERM, which it handles like an exit
// request also when it no longer answers on its socket. os.ErrProcessDone: it is gone.
func askToExit(pid int) error { return osproc.Terminate(pid) }
