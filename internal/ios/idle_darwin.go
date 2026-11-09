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

// procUsageOf reads a process's CPU time, as libproc's proc_pid_rusage does
// (proc_info(PROC_INFO_CALL_PIDRUSAGE, pid, RUSAGE_INFO_V2): user and system time at bytes 16 and
// 24), and its start time, as proc_pidinfo(PROC_PIDTBSDINFO) does (struct proc_bsdinfo:
// pbi_start_tvsec and pbi_start_tvusec at bytes 120 and 128).
func procUsageOf(pid int) (procUsage, error) {
	const callPidInfo, callPidRusage, flavorBSDInfo, rusageInfoV2 = 2, 9, 3, 2
	var ru [296]byte // larger than rusage_info_v2 (184 bytes)
	if _, _, errno := syscall.Syscall6(syscall.SYS_PROC_INFO, callPidRusage, uintptr(pid), rusageInfoV2, 0, uintptr(unsafe.Pointer(&ru[0])), 0); errno != 0 {
		return procUsage{}, errno
	}
	var bi [136]byte // sizeof(struct proc_bsdinfo)
	r, _, errno := syscall.Syscall6(syscall.SYS_PROC_INFO, callPidInfo, uintptr(pid), flavorBSDInfo, 0, uintptr(unsafe.Pointer(&bi[0])), uintptr(len(bi)))
	if errno != 0 {
		return procUsage{}, errno
	}
	if r != uintptr(len(bi)) {
		return procUsage{}, syscall.EINVAL
	}
	le := binary.LittleEndian
	return procUsage{
		cpu:   le.Uint64(ru[16:]) + le.Uint64(ru[24:]),
		start: int64(le.Uint64(bi[120:]))*1_000_000 + int64(le.Uint64(bi[128:])),
	}, nil
}
