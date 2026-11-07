//go:build darwin || linux

package claudesessions

import "golang.org/x/sys/unix"

// ttyDev is the device number of a terminal such as /dev/ttys016 or /dev/pts/3, in the form proc reports a
// process's tty.
var ttyDev = func(path string) (int64, bool) {
	var st unix.Stat_t
	if unix.Stat(path, &st) != nil {
		return 0, false
	}
	return int64(st.Rdev), true
}

// sessionLeader is the process that owns pid's terminal session: in a Ghostty tab, the tab's shell. 0 means this one.
var sessionLeader = func(pid int) (int, bool) {
	sid, err := unix.Getsid(pid)
	return sid, err == nil
}

// hangUp tells a process its terminal is gone, as closing the tab would.
var hangUp = func(pid int) error { return unix.Kill(pid, unix.SIGHUP) }
