package android

import (
	"errors"

	"golang.org/x/sys/unix"
)

// sysctlDenied reports whether this process may not read sysctl hw.cachelinesize, as in the
// Seatbelt profiles of agent sandboxes (Codex, Claude Code). The emulator reads it at start and
// dies of SIGILL when it is denied (startcause.go).
func sysctlDenied() bool {
	_, err := unix.SysctlRaw("hw.cachelinesize")
	return errors.Is(err, unix.EPERM)
}
