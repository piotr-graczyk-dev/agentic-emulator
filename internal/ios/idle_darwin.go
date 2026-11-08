//go:build darwin

package ios

import "syscall"

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
