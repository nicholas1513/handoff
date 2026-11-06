// Package claudesessions saves the set of Claude sessions running in Ghostty and reopens them later.
package claudesessions

type procInfo struct {
	ppid  int
	start int64 // Unix µs
	tty   int64 // device number; noTTY when the process has no controlling terminal
	name  string
}

const noTTY = -1

var shells = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true}

// Owner returns the Claude process a hook runs under, starting from the hook's parent pid, and its start time.
// Claude runs its children without a controlling terminal, through a shell or a wrapper, so it is the first
// ancestor that has one and isn't a shell. Its name is no help: the native install's process is named after its
// version (e.g. "2.1.319").
func Owner(pid int) (int, int64, bool) {
	for range 8 {
		p, ok := proc(pid)
		if !ok {
			return 0, 0, false
		}
		if p.tty != noTTY && !shells[p.name] {
			return pid, p.start, true
		}
		if p.ppid <= 1 {
			return 0, 0, false
		}
		pid = p.ppid
	}
	return 0, 0, false
}
