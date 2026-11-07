//go:build !darwin && !linux

package claudesessions

import "errors"

// Saving sessions needs Ghostty's layout, which is only read on macOS and Linux.
var proc = func(pid int) (procInfo, bool) { return procInfo{}, false }

var ttyDev = func(path string) (int64, bool) { return 0, false }

var ghosttyTerminals = func([]int) ([]terminal, error) { return nil, errors.New("not supported on this OS") }

var openWindows = func(windows [][]Tab) error { return errors.New("not supported on this OS") }
