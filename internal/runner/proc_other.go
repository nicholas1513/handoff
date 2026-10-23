//go:build !darwin

package runner

import (
	"os"
	"strconv"
	"strings"
)

// procName is the kernel's short name for pid (at most 15 bytes on Linux), or "" if there is no such process.
func procName(pid int) string {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/comm")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
