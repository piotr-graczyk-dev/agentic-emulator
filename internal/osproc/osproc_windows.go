//go:build windows

package osproc

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"sync"
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
	if err := cmd.Start(); err != nil {
		return err
	}
	track(cmd.Process.Pid)
	return nil
}

// Windows reuses a process id as soon as no handle to the exited process is left, and exec.Cmd
// closes its handle once Wait returns. The group functions must never act on a process that got
// the id of one agemu started, so StartDetached keeps a handle of its own to each process it
// starts: while it is open the id cannot be reused. The group functions accept only these
// processes, and a process counts as a child only when it was created after its parent (a
// process with a reused id and an older parent is not). The handle is closed once the whole
// group has exited.
type tracked struct {
	h       windows.Handle
	created int64 // creation time, 100 ns units
}

var (
	trackedMu sync.Mutex
	trackedBy = map[int]tracked{}
)

func track(pid int) {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return
	}
	created, ok := creationTime(h)
	if !ok {
		windows.CloseHandle(h)
		return
	}
	trackedMu.Lock()
	if old, ok := trackedBy[pid]; ok {
		windows.CloseHandle(old.h)
	}
	trackedBy[pid] = tracked{h: h, created: created}
	trackedMu.Unlock()
}

func creationTime(h windows.Handle) (int64, bool) {
	var c, e, k, u windows.Filetime
	if windows.GetProcessTimes(h, &c, &e, &k, &u) != nil {
		return 0, false
	}
	return c.Nanoseconds() / 100, true
}

// running reports whether the process behind h has not exited.
func running(h windows.Handle) bool {
	ev, err := windows.WaitForSingleObject(h, 0)
	return err == nil && ev == uint32(windows.WAIT_TIMEOUT)
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

// KillGroup ends the process tree under pid, children first. pid must be a process StartDetached
// started in this process; for any other pid it does nothing.
func KillGroup(pid int) {
	trackedMu.Lock()
	t, ok := trackedBy[pid]
	trackedMu.Unlock()
	if !ok {
		return
	}
	kids := descendants(pid, t.created)
	for _, k := range kids {
		_ = windows.TerminateProcess(k.h, 1)
		windows.CloseHandle(k.h)
	}
	if running(t.h) {
		_ = windows.TerminateProcess(t.h, 1)
	}
}

// GroupAlive reports whether pid or any process it started still runs. pid must be a process
// StartDetached started in this process; for any other pid it is false. Windows keeps a child's
// parent id after the parent exits, so the children are found also then.
func GroupAlive(pid int) bool {
	trackedMu.Lock()
	t, ok := trackedBy[pid]
	trackedMu.Unlock()
	if !ok {
		return false
	}
	if running(t.h) {
		return true
	}
	kids := descendants(pid, t.created)
	alive := false
	for _, k := range kids {
		alive = alive || running(k.h)
		windows.CloseHandle(k.h)
	}
	if !alive { // the group is gone: let Windows reuse the id
		trackedMu.Lock()
		if cur, ok := trackedBy[pid]; ok && cur.h == t.h {
			delete(trackedBy, pid)
			windows.CloseHandle(t.h)
		}
		trackedMu.Unlock()
	}
	return alive
}

type proc struct {
	pid int
	h   windows.Handle
}

// descendants opens the processes under the process pid created at created, deepest first. A
// process counts as a child only when its parent id matches and it was created after that
// parent, so a process that got a reused id, or whose parent id was reused, is left out. The
// caller closes the handles; holding them keeps the ids from being reused meanwhile.
func descendants(pid int, created int64) []proc {
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
	var out []proc
	seen := map[uint32]bool{uint32(pid): true}
	var walk func(parent uint32, parentCreated int64)
	walk = func(parent uint32, parentCreated int64) {
		for _, c := range children[parent] {
			if seen[c] {
				continue
			}
			seen[c] = true
			h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, c)
			if err != nil {
				continue
			}
			ct, ok := creationTime(h)
			if !ok || ct < parentCreated {
				windows.CloseHandle(h)
				continue
			}
			walk(c, ct)
			out = append(out, proc{pid: int(c), h: h})
		}
	}
	walk(uint32(pid), created)
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

// PeerClosed reports whether the other end has closed conn, without reading from it. Unlike on
// Unix, a write to a Windows socket whose peer has gone succeeds, so a client that must not send
// a request into a dead connection asks first. Use it only when no data is expected: pending data
// also counts as closed.
func PeerClosed(conn net.Conn) bool {
	sc, ok := conn.(syscall.Conn)
	if !ok {
		return false
	}
	raw, err := sc.SyscallConn()
	if err != nil {
		return false
	}
	closed := false
	_ = raw.Control(func(fd uintptr) {
		pfd := wsaPollFD{fd: fd, events: pollRdNorm}
		n, _, _ := procWSAPoll.Call(uintptr(unsafe.Pointer(&pfd)), 1, 0)
		closed = int32(n) == 1 && pfd.revents&(pollRdNorm|pollHup|pollErr|pollNval) != 0
	})
	return closed
}

type wsaPollFD struct {
	fd      uintptr
	events  int16
	revents int16
}

const (
	pollErr    = 0x0001
	pollHup    = 0x0002
	pollNval   = 0x0004
	pollRdNorm = 0x0100
)

var procWSAPoll = windows.NewLazySystemDLL("ws2_32.dll").NewProc("WSAPoll")
