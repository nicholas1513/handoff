package claudesessions

import "golang.org/x/sys/unix"

// proc asks the kernel about pid directly: /bin/ps is setuid and unavailable in some sandboxes.
var proc = func(pid int) (procInfo, bool) {
	k, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || k.Proc.P_pid != int32(pid) {
		return procInfo{}, false
	}
	st := k.Proc.P_starttime
	return procInfo{ppid: int(k.Eproc.Ppid), start: st.Sec*1e6 + int64(st.Usec), tty: int64(k.Eproc.Tdev),
		name: unix.ByteSliceToString(k.Proc.P_comm[:])}, true
}
