//go:build !darwin

package claudesessions

// Saving sessions reads Ghostty's layout through AppleScript, so it is macOS only.
var proc = func(pid int) (procInfo, bool) { return procInfo{}, false }

var ttyDev = func(path string) (int64, bool) { return 0, false }
