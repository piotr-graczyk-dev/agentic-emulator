//go:build darwin

package ios

import (
	"encoding/binary"
	"syscall"
	"unsafe"
)

// procDirty reads a process's kernel dirty-tracking flags, as libproc's proc_get_dirty does:
// proc_info(PROC_INFO_CALL_DIRTYCONTROL, pid, PROC_DIRTYCONTROL_GET).
func procDirty(pid int) (uint32, error) {
	const callDirtyControl, dirtyControlGet = 8, 3
	r, _, errno := syscall.Syscall6(syscall.SYS_PROC_INFO, callDirtyControl, uintptr(pid), dirtyControlGet, 0, 0, 0)
	if errno != 0 {
		return 0, errno
	}
	return uint32(r), nil
}

// procUsageOf reads a process's CPU time and start time, as libproc's proc_pid_rusage does:
// proc_info(PROC_INFO_CALL_PIDRUSAGE, pid, RUSAGE_INFO_V2) into a rusage_info_v2 (16-byte uuid,
// then user time, system time, ..., ri_proc_start_abstime at byte 80).
func procUsageOf(pid int) (procUsage, error) {
	const callPidRusage, rusageInfoV2 = 9, 2
	var buf [296]byte // larger than rusage_info_v2 (184 bytes)
	_, _, errno := syscall.Syscall6(syscall.SYS_PROC_INFO, callPidRusage, uintptr(pid), rusageInfoV2, 0, uintptr(unsafe.Pointer(&buf[0])), 0)
	if errno != 0 {
		return procUsage{}, errno
	}
	le := binary.LittleEndian
	return procUsage{cpu: le.Uint64(buf[16:]) + le.Uint64(buf[24:]), start: le.Uint64(buf[80:])}, nil
}
