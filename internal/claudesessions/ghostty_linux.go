package claudesessions

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Ghostty's GTK build has no way to ask for its windows and tabs, but it does list them to the accessibility bus
// (AT-SPI, see atspi_linux.go). A tab's entry there has only a title, so a save tells tabs apart by writing a
// one-off title to each Claude terminal and seeing which tab shows it. That goes to Ghostty, the display side of
// the terminal: it reaches neither Claude's input nor the keyboard focus.

const surfaceEnv = "GHOSTTY_SURFACE_ID="

// How long a save keeps looking for its titles, how often, and how often it writes them again. A busy Claude
// animates its title, which replaces a marker: it takes writing it again and catching it between two frames.
const (
	markWindow  = 500 * time.Millisecond
	markPoll    = 15 * time.Millisecond
	markRewrite = 4 // polls
)

// tabReader is what matching needs from the accessibility bus.
type tabReader interface {
	tabs() ([]tabNode, error)
	title(tabNode) (string, error)
	close()
}

// A tabNode is one tab as the bus shows it, with the title it had when listed.
type tabNode struct {
	window, tab int
	bus, path   string
	title       string
}

// Tests replace these.
var (
	dialTabs   = func(ghostty []int) (tabReader, error) { return dialAtspi(ghostty) }
	setTitle   = writeTitle
	sleepPoll  = time.Sleep
	markSuffix = func() string {
		b := make([]byte, 8)
		rand.Read(b)
		return hex.EncodeToString(b)
	}
)

// ghosttyTerminals finds the tab of each Claude process running in Ghostty, in window and tab order.
var ghosttyTerminals = func(pids []int) ([]terminal, error) {
	var ttys []string
	var ghostty []int
	for _, pid := range pids {
		tty, ok := surfaceTTY(pid)
		if !ok {
			continue
		}
		g, ok := ghosttyOf(pid)
		if !ok {
			continue
		}
		ttys = append(ttys, tty)
		if !slices.Contains(ghostty, g) {
			ghostty = append(ghostty, g)
		}
	}
	if len(ttys) == 0 {
		return nil, nil
	}
	r, err := dialTabs(ghostty)
	if err != nil {
		return nil, fmt.Errorf("accessibility bus: %w", err)
	}
	defer r.close()
	return matchTabs(r, ttys)
}

// surfaceTTY is the terminal of a process Ghostty started, directly or through its descendants.
func surfaceTTY(pid int) (string, bool) {
	dir := "/proc/" + strconv.Itoa(pid)
	env, err := os.ReadFile(dir + "/environ")
	if err != nil || !hasSurface(env) {
		return "", false
	}
	tty, err := os.Readlink(dir + "/fd/0")
	return tty, err == nil && strings.HasPrefix(tty, "/dev/pts/")
}

func hasSurface(env []byte) bool {
	_, ok := surfaceOf(env)
	return ok
}

func surfaceOf(env []byte) (string, bool) {
	for _, kv := range bytes.Split(env, []byte{0}) {
		if v, ok := bytes.CutPrefix(kv, []byte(surfaceEnv)); ok {
			return string(v), true
		}
	}
	return "", false
}

// ghosttyOf is the Ghostty process above pid.
func ghosttyOf(pid int) (int, bool) {
	for range 16 {
		p, ok := proc(pid)
		if !ok {
			return 0, false
		}
		if p.name == "ghostty" {
			return pid, true
		}
		pid = p.ppid
	}
	return 0, false
}

// matchTabs tags each tty with a title and reads back which tab shows it, then puts the titles back.
// A tty it can't place (another Claude's frame kept winning, a tab with a fixed title, a split that isn't focused)
// is left out, and Save puts its session in the extra window.
func matchTabs(r tabReader, ttys []string) ([]terminal, error) {
	tabs, err := r.tabs()
	if err != nil {
		return nil, err
	}
	if len(tabs) == 0 {
		return nil, errors.New("no Ghostty tabs listed")
	}
	suffix, marks := markSuffix(), map[string]string{}
	for i, tty := range ttys {
		marks["handoff-save-"+strconv.Itoa(i)+"-"+suffix] = tty
	}
	found, open := map[int]string{}, map[string]string{} // tab index -> tty; marker -> tty still unplaced
	for m, tty := range marks {
		open[m] = tty
	}
	look := func() {
		for i, n := range tabs {
			if _, done := found[i]; done {
				continue
			}
			t, err := r.title(n)
			if err != nil {
				continue
			}
			if tty, ok := open[t]; ok {
				found[i] = tty
				delete(open, t)
			}
		}
	}
	for poll := 0; len(open) > 0 && time.Duration(poll)*markPoll < markWindow; poll++ {
		if poll%markRewrite == 0 {
			for m, tty := range open {
				setTitle(tty, m)
			}
		}
		sleepPoll(markPoll)
		look()
	}
	// A marker that showed late, or on a tab that never matched, must not stay as the tab's title.
	look()
	var out []terminal
	for i, tty := range found {
		setTitle(tty, tabs[i].title)
		out = append(out, terminal{strconv.Itoa(tabs[i].window), strconv.Itoa(tabs[i].tab), tty})
	}
	slices.SortFunc(out, func(a, b terminal) int {
		wa, _ := strconv.Atoi(a.window)
		wb, _ := strconv.Atoi(b.window)
		ta, _ := strconv.Atoi(a.tab)
		tb, _ := strconv.Atoi(b.tab)
		if wa != wb {
			return wa - wb
		}
		return ta - tb
	})
	return out, nil
}

// writeTitle sets a terminal's title. Anything that could end the sequence early is dropped: titles come from
// what the terminal's programs set, and go back out here.
func writeTitle(tty, title string) {
	f, err := os.OpenFile(tty, os.O_WRONLY|unix.O_NOCTTY, 0)
	if err != nil {
		return
	}
	defer f.Close()
	clean := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return -1
		}
		return r
	}, title)
	f.WriteString("\x1b]2;" + clean + "\a")
}

// Tests replace these.
var (
	runGhostty = func(args ...string) error {
		out, err := exec.Command("ghostty", args...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("ghostty %s: %v: %s", args[0], err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	findSurface = surfaceOfSession
	shellPath   = loginShell
)

const surfaceWait = 5 * time.Second

// openWindows opens a window per entry and adds its other tabs. A tab is added by naming a surface in the window,
// and only the process in that surface knows its id, so each window waits for its first session to start.
var openWindows = func(windows [][]Tab) error {
	shell := shellPath()
	for i, w := range windows {
		if err := runGhostty(tabArgs("+new-window", "", w[0], shell)...); err != nil {
			return fmt.Errorf("opened %d of %s: %w", i, plural(len(windows), "window"), err)
		}
		if len(w) == 1 {
			continue
		}
		surface, ok := "", false
		for t0 := time.Now(); !ok && time.Since(t0) < surfaceWait; {
			sleepPoll(100 * time.Millisecond)
			surface, ok = findSurface(w[0].SessionID)
		}
		if !ok {
			return fmt.Errorf("opened %d of %s: the new window didn't report its surface, so %s not opened",
				i+1, plural(len(windows), "window"), plural(len(w)-1, "tab"))
		}
		for _, t := range w[1:] {
			if err := runGhostty(tabArgs("+new-tab", surface, t, shell)...); err != nil {
				return err
			}
		}
	}
	return nil
}

// tabArgs starts a login shell that resumes the session and stays open after it, like a command typed at a prompt.
// The folder is a separate argument, never shell text; the id was checked by Restore and the shell by loginShell.
func tabArgs(action, surface string, t Tab, shell string) []string {
	args := []string{action}
	if surface != "" {
		args = append(args, "--surface-id="+surface)
	}
	return append(args, "--working-directory="+t.Cwd, "-e", shell, "-l", "-c",
		"claude --resume "+t.SessionID+"; exec "+shell+" -l")
}

// surfaceOfSession finds the Ghostty surface of the process resuming a session.
func surfaceOfSession(id string) (string, bool) {
	cmds, _ := filepath.Glob("/proc/[0-9]*/cmdline")
	for _, c := range cmds {
		b, err := os.ReadFile(c)
		if err != nil || !bytes.Contains(b, []byte("--resume "+id)) && !bytes.Contains(b, append([]byte("--resume\x00"), id...)) {
			continue
		}
		env, err := os.ReadFile(filepath.Join(filepath.Dir(c), "environ"))
		if err != nil {
			continue
		}
		if s, ok := surfaceOf(env); ok {
			return s, true
		}
	}
	return "", false
}

var plainPath = regexp.MustCompile(`^/[A-Za-z0-9_./-]+$`)

// loginShell is the user's shell from the password file: $SHELL can be whatever started this process.
func loginShell() string {
	u, err := user.Current()
	if err != nil {
		return "/bin/sh"
	}
	b, err := os.ReadFile("/etc/passwd")
	if err != nil {
		return "/bin/sh"
	}
	return shellIn(string(b), u.Username)
}

func shellIn(passwd, name string) string {
	for _, l := range strings.Split(passwd, "\n") {
		if f := strings.Split(l, ":"); len(f) == 7 && f[0] == name && plainPath.MatchString(f[6]) {
			return f[6]
		}
	}
	return "/bin/sh"
}
