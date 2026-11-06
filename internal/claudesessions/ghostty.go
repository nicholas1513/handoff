package claudesessions

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// Ghostty 1.4+ is scripted through AppleScript (see Ghostty.app/Contents/Resources/Ghostty.sdef). Data reaches
// these scripts only as run arguments, never as script text.

// queryScript prints one line per terminal: window id, tab index and tty, tab-separated, in window and tab order.
// It doesn't start Ghostty when it isn't running.
const queryScript = `on run
	if application "Ghostty" is not running then return ""
	set sep to character id 9
	set out to ""
	tell application "Ghostty"
		repeat with w in windows
			repeat with t in tabs of w
				repeat with c in terminals of t
					set out to out & (id of w) & sep & (index of t) & sep & (tty of c) & linefeed
				end repeat
			end repeat
		end repeat
	end tell
	return out
end run`

// openScript takes (window key, folder, session id) triples. A new key opens a window, a repeated one a tab in it;
// each starts the user's shell in the folder and types the resume command.
const openScript = `on run argv
	tell application "Ghostty"
		set lastKey to ""
		set w to missing value
		repeat with i from 1 to (count of argv) by 3
			set cfg to new surface configuration
			set initial working directory of cfg to item (i + 1) of argv
			set initial input of cfg to "claude --resume " & item (i + 2) of argv & linefeed
			if item i of argv is not lastKey then
				set w to new window with configuration cfg
				set lastKey to item i of argv
			else
				new tab in w with configuration cfg
			end if
		end repeat
	end tell
end run`

// osascript runs a script with arguments. Each line goes in its own -e, as osascript builds multi-line scripts.
var osascript = func(script string, args ...string) (string, error) {
	var argv []string
	for _, l := range strings.Split(script, "\n") {
		argv = append(argv, "-e", l)
	}
	out, err := exec.Command("osascript", append(argv, args...)...).Output()
	if ee := (*exec.ExitError)(nil); errors.As(err, &ee) {
		err = fmt.Errorf("osascript: %s", strings.TrimSpace(string(ee.Stderr)))
	}
	return string(out), err
}

type terminal struct {
	window string
	tab    string
	tty    string
}

// ghosttyTerminals lists Ghostty's terminals in window and tab order; none when Ghostty isn't running.
func ghosttyTerminals() ([]terminal, error) {
	out, err := osascript(queryScript)
	if err != nil {
		return nil, err
	}
	var ts []terminal
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if f := strings.Split(l, "\t"); len(f) == 3 {
			ts = append(ts, terminal{f[0], f[1], f[2]})
		}
	}
	return ts, nil
}
