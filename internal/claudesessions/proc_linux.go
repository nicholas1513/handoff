package claudesessions

import (
	"os"
	"strconv"
	"strings"
)

// proc reads /proc/<pid>/stat. start is in clock ticks since boot, not Unix µs: it is only compared with itself,
// on the same machine, to tell a pid from a later process that reused it.
var proc = func(pid int) (procInfo, bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return procInfo{}, false
	}
	return parseStat(string(b))
}

// parseStat reads the fields of proc(5) a save needs. The name sits in parentheses and may hold anything, spaces
// and parentheses too, so the fields are counted from its last ')'.
func parseStat(s string) (procInfo, bool) {
	open, end := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
	if open < 0 || end < open {
		return procInfo{}, false
	}
	f := strings.Fields(s[end+1:]) // f[0] is field 3 of proc(5)
	if len(f) < 20 {
		return procInfo{}, false
	}
	ppid, err1 := strconv.Atoi(f[1])
	tty, err2 := strconv.ParseInt(f[4], 10, 64)
	start, err3 := strconv.ParseInt(f[19], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil {
		return procInfo{}, false
	}
	if tty == 0 {
		tty = noTTY
	}
	return procInfo{ppid: ppid, start: start, tty: tty, name: s[open+1 : end]}, true
}
