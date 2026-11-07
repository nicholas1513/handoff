package claudesessions

import "strings"

// How a save and a restore reach Ghostty differs by OS: ghosttyTerminals and openWindows live in ghostty_<os>.go.
// Everything else about a save is shared.

type terminal struct {
	window string
	tab    string
	tty    string
}

// parseTerminals reads one terminal per line: window, tab and tty, tab-separated.
func parseTerminals(out string) []terminal {
	var ts []terminal
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if f := strings.Split(l, "\t"); len(f) == 3 {
			ts = append(ts, terminal{f[0], f[1], f[2]})
		}
	}
	return ts
}
