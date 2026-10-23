package runner

import "golang.org/x/sys/unix"

// procName is the kernel's short name for pid (at most 16 bytes), or "" if there is no such process.
// Asked of the kernel directly: /bin/ps is setuid and unavailable in some sandboxes.
func procName(pid int) string {
	k, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || k.Proc.P_pid != int32(pid) {
		return ""
	}
	return unix.ByteSliceToString(k.Proc.P_comm[:])
}
