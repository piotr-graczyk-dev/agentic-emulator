//go:build windows

package osproc

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// StartDetached starts cmd without a console and in its own process group, so it keeps running
// when the process that started it exits and Ctrl+C in the starter's console does not reach it.
// When the starter runs inside a job object that allows it (terminals and IDEs often put their
// children in one, and close it with them), the child also leaves that job.
func StartDetached(cmd *exec.Cmd) error {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS | breakawayFlag()
	cmd.SysProcAttr.HideWindow = true
	return cmd.Start()
}

// breakawayFlag is CREATE_BREAKAWAY_FROM_JOB when this process is in a job that lets children
// break away. Asking for it in a job that does not allow it fails CreateProcess, so it is only
// set when allowed.
func breakawayFlag() uint32 {
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	// A null job handle queries the job this process is in; it fails when there is none.
	if err := windows.QueryInformationJobObject(0, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil); err != nil {
		return 0
	}
	if info.BasicLimitInformation.LimitFlags&windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK != 0 {
		return windows.CREATE_BREAKAWAY_FROM_JOB
	}
	return 0
}

const stillActive = 259 // STILL_ACTIVE, GetExitCodeProcess's code for a running process

// Alive reports whether a process with this pid runs.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		// Access denied: it exists but belongs to someone else.
		return errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	defer windows.CloseHandle(h)
	var code uint32
	if windows.GetExitCodeProcess(h, &code) != nil {
		return false
	}
	return code == stillActive
}

// Exists is Alive: Windows has no zombies to tell apart.
func Exists(pid int) bool { return Alive(pid) }

// Terminate ends the process. Windows has no request to exit that a detached process without a
// window receives, so it is the same as Kill; callers ask agemu processes to exit over their own
// connection first. It returns os.ErrProcessDone when there is no such process.
func Terminate(pid int) error { return Kill(pid) }

// Kill ends the process at once (TerminateProcess). It returns os.ErrProcessDone when there is
// no such process.
func Kill(pid int) error {
	if pid <= 0 {
		return os.ErrProcessDone
	}
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return os.ErrProcessDone
		}
		return err
	}
	defer windows.CloseHandle(h)
	var code uint32
	if windows.GetExitCodeProcess(h, &code) == nil && code != stillActive {
		return os.ErrProcessDone
	}
	return windows.TerminateProcess(h, 1)
}

// TerminateGroup ends the process tree under pid, children first (see Terminate).
func TerminateGroup(pid int) { KillGroup(pid) }

// KillGroup ends the process tree under pid, children first.
func KillGroup(pid int) {
	if pid <= 0 {
		return
	}
	for _, c := range descendants(pid) {
		_ = Kill(c)
	}
	_ = Kill(pid)
}

// GroupAlive reports whether pid or any process it started still runs. Windows keeps a child's
// parent pid after the parent exits, so the children are found also then.
func GroupAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	if Alive(pid) {
		return true
	}
	for _, c := range descendants(pid) {
		if Alive(c) {
			return true
		}
	}
	return false
}

// descendants lists the processes under pid, deepest first.
func descendants(pid int) []int {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(snap)
	children := map[uint32][]uint32{}
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		if e.ProcessID != e.ParentProcessID {
			children[e.ParentProcessID] = append(children[e.ParentProcessID], e.ProcessID)
		}
	}
	var out []int
	seen := map[uint32]bool{uint32(pid): true}
	var walk func(p uint32)
	walk = func(p uint32) {
		for _, c := range children[p] {
			if seen[c] {
				continue
			}
			seen[c] = true
			walk(c)
			out = append(out, int(c))
		}
	}
	walk(uint32(pid))
	return out
}

// lockOffset is where TryLock locks one byte. Windows locks are mandatory: a lock on the file's
// content would stop other processes from reading it (the daemon's pid is in its lock file), so
// the lock covers a byte far past the end, which no one reads or writes.
const lockOffsetHigh = 0x7fffffff

// TryLock takes a lock on f without waiting: exclusive, or shared when exclusive is false. It
// returns ErrLocked when another process holds a conflicting lock. The lock ends when f is closed
// or Unlock is called.
func TryLock(f *os.File, exclusive bool) error {
	flags := uint32(windows.LOCKFILE_FAIL_IMMEDIATELY)
	if exclusive {
		flags |= windows.LOCKFILE_EXCLUSIVE_LOCK
	}
	ol := windows.Overlapped{OffsetHigh: lockOffsetHigh}
	err := windows.LockFileEx(windows.Handle(f.Fd()), flags, 0, 1, 0, &ol)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING) {
		return ErrLocked
	}
	return err
}

// Unlock releases a lock TryLock took.
func Unlock(f *os.File) error {
	ol := windows.Overlapped{OffsetHigh: lockOffsetHigh}
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &ol)
}

// MapFile maps the first size bytes of f read-write and shared, so writes by another process
// that maps the same file show up in the returned slice. The file must be at least size bytes
// long. Release it with Unmap; f may be closed once it is mapped.
func MapFile(f *os.File, size int) ([]byte, error) {
	if size <= 0 {
		return nil, errors.New("osproc: mapping of no bytes")
	}
	m, err := windows.CreateFileMapping(windows.Handle(f.Fd()), nil, windows.PAGE_READWRITE,
		uint32(uint64(size)>>32), uint32(size), nil)
	if err != nil {
		return nil, os.NewSyscallError("CreateFileMapping", err)
	}
	defer windows.CloseHandle(m) // the view keeps the mapping alive
	addr, err := windows.MapViewOfFile(m, windows.FILE_MAP_READ|windows.FILE_MAP_WRITE, 0, 0, uintptr(size))
	if err != nil {
		return nil, os.NewSyscallError("MapViewOfFile", err)
	}
	// The view is memory the OS mapped outside Go's heap, so the address is not one the GC
	// moves; reading it back through a pointer avoids vet's uintptr conversion check.
	return unsafe.Slice((*byte)(*(*unsafe.Pointer)(unsafe.Pointer(&addr))), size), nil
}

// Unmap releases a mapping MapFile made.
func Unmap(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	return windows.UnmapViewOfFile(uintptr(unsafe.Pointer(&b[0])))
}

// IsConnRefused reports whether err is a refused TCP or socket connection.
func IsConnRefused(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, windows.WSAECONNREFUSED)
}
