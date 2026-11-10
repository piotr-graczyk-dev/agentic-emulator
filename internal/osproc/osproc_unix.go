//go:build !windows

package osproc

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// StartDetached starts cmd in its own session, so it keeps running when the process that
// started it exits and a signal to the starter's terminal does not reach it.
func StartDetached(cmd *exec.Cmd) error {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
	return cmd.Start()
}

// Alive reports whether a process with this pid runs and this user may signal it.
func Alive(pid int) bool { return pid > 0 && syscall.Kill(pid, 0) == nil }

// Exists reports whether a process with this pid exists, also one owned by another user or a
// zombie that has not been reaped.
func Exists(pid int) bool {
	return pid > 0 && !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}

// Terminate asks the process to exit (SIGTERM). It returns os.ErrProcessDone when there is no
// such process.
func Terminate(pid int) error { return signal(pid, syscall.SIGTERM) }

// Kill ends the process at once (SIGKILL). It returns os.ErrProcessDone when there is no such
// process.
func Kill(pid int) error { return signal(pid, syscall.SIGKILL) }

func signal(pid int, sig syscall.Signal) error {
	if pid <= 0 { // kill(0) and kill(-n) signal process groups
		return os.ErrProcessDone
	}
	err := syscall.Kill(pid, sig)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}

// TerminateGroup sends SIGTERM to the group led by pid (see the package comment).
func TerminateGroup(pid int) {
	if pid > 0 {
		_ = syscall.Kill(-pid, syscall.SIGTERM)
	}
}

// KillGroup sends SIGKILL to the group led by pid.
func KillGroup(pid int) {
	if pid > 0 {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
}

// GroupAlive reports whether any process of the group led by pid still runs.
func GroupAlive(pid int) bool { return pid > 0 && syscall.Kill(-pid, 0) == nil }

// TryLock takes an advisory lock on f without waiting: exclusive, or shared when exclusive is
// false. It returns ErrLocked when another process holds a conflicting lock. The lock ends when
// f is closed or Unlock is called.
func TryLock(f *os.File, exclusive bool) error {
	how := syscall.LOCK_SH
	if exclusive {
		how = syscall.LOCK_EX
	}
	err := syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return ErrLocked
	}
	return err
}

// Unlock releases a lock TryLock took.
func Unlock(f *os.File) error { return syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }

// MapFile maps the first size bytes of f read-write and shared, so writes by another process
// that maps the same file show up in the returned slice. Release it with Unmap.
func MapFile(f *os.File, size int) ([]byte, error) {
	return syscall.Mmap(int(f.Fd()), 0, size, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
}

// Unmap releases a mapping MapFile made.
func Unmap(b []byte) error { return syscall.Munmap(b) }

// IsConnRefused reports whether err is a refused TCP or socket connection.
func IsConnRefused(err error) bool { return errors.Is(err, syscall.ECONNREFUSED) }
