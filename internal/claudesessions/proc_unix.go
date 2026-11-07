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
