package claudesessions

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/nicholas1513/handoff/internal/queue"
)

// The save lives outside queue/, which is all the sandbox may write, so an agent can't plant sessions to restore.
const file = "claude-sessions.json"

// uuid is the only kind of session id restore will type into a shell.
var uuid = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// Saved is the set of sessions a save found, by window, in Ghostty's window and tab order.
type Saved struct {
	SavedAt time.Time `json:"saved_at"`
	Windows [][]Tab   `json:"windows"`
}

type Tab struct {
	SessionID      string `json:"session_id"`
	Cwd            string `json:"cwd"`
	TranscriptPath string `json:"transcript_path"`
}

func count(ws [][]Tab) int {
	n := 0
	for _, w := range ws {
		n += len(w)
	}
	return n
}

func plural(n int, s string) string {
	if n == 1 {
		return "1 " + s
	}
	return fmt.Sprintf("%d %ss", n, s)
}

func size(ws [][]Tab) string {
	return plural(len(ws), "window") + " · " + plural(count(ws), "session")
}

// Label is "saved 14:02 · 2 windows · 5 sessions", with the date when it wasn't today.
func (s Saved) Label(now time.Time) string {
	at := s.SavedAt.Local().Format("15:04")
	if y, m, d := s.SavedAt.Local().Date(); y != now.Year() || m != now.Month() || d != now.Day() {
		at = s.SavedAt.Local().Format("Jan 2 15:04")
	}
	return "saved " + at + " · " + size(s.Windows)
}

// Load reads the last save; an error wrapping fs.ErrNotExist when there is none.
func Load(root string) (Saved, error) {
	var s Saved
	b, err := os.ReadFile(filepath.Join(root, file))
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal(b, &s)
}

type liveSession struct {
	queue.Session
	tty  int64
	busy bool // Claude Code's list says it's working; the hook's records don't say
}

// live is the session of each Claude process still running, by pid: what Claude Code lists, which covers sessions
// the plugin never saw, then the hook records for any process it doesn't list.
func live(q *queue.Queue) map[int]liveSession {
	out := map[int]liveSession{}
	hooked := liveFromHooks(q)
	for pid, l := range liveFromClaude() {
		if h, ok := hooked[pid]; ok && h.SessionID == l.SessionID {
			l.TranscriptPath = h.TranscriptPath // the hook was told it; otherwise it's looked for
		}
		out[pid] = l
	}
	for pid, l := range hooked {
		if _, ok := out[pid]; !ok {
			out[pid] = l
		}
	}
	return out
}

// liveFromHooks is the newest session the hook recorded for each process still running. /clear and /resume start a
// new session in the same process; a pid whose start time differs belongs to another process now.
func liveFromHooks(q *queue.Queue) map[int]liveSession {
	ents, _ := os.ReadDir(filepath.Join(q.Root, "sessions"))
	out := map[int]liveSession{}
	for _, e := range ents {
		s, err := q.ReadSession(e.Name())
		if err != nil || s.ClaudePID == 0 || !uuid.MatchString(s.SessionID) {
			continue
		}
		p, ok := proc(s.ClaudePID)
		if !ok || p.start != s.ClaudeStart {
			continue
		}
		if cur, ok := out[s.ClaudePID]; ok && !s.Started.After(cur.Started) {
			continue
		}
		out[s.ClaudePID] = liveSession{Session: s, tty: p.tty}
	}
	return out
}

func tabOf(l liveSession) Tab { return Tab{l.SessionID, l.Cwd, l.TranscriptPath} }

// Save records the running sessions by Ghostty window and tab. Sessions Ghostty doesn't show (another terminal, or
// Ghostty not answering) go in one more window. It refuses to save none, so a save made right after a reboot keeps
// the one worth restoring.
// shortcut: a tab split into several Claude terminals comes back as one tab per terminal.
func Save(q *queue.Queue, now time.Time) (string, error) {
	ls := live(q)
	if len(ls) == 0 {
		err := errors.New("no running Claude sessions found; kept the last save")
		record(q.Root, "save", "", err, nil)
		return "", err
	}
	byTTY := map[int64]liveSession{}
	for _, l := range ls {
		if l.tty != noTTY {
			byTTY[l.tty] = l
		}
	}
	s, win, done := Saved{SavedAt: now}, map[string]int{}, map[string]bool{}
	pids := slices.Sorted(maps.Keys(ls))
	ts, gerr := ghosttyTerminals(pids)
	for _, t := range ts {
		dev, ok := ttyDev(t.tty)
		l, found := byTTY[dev]
		if !ok || !found || done[l.SessionID] {
			continue
		}
		done[l.SessionID] = true
		i, ok := win[t.window]
		if !ok {
			i, win[t.window] = len(s.Windows), len(s.Windows)
			s.Windows = append(s.Windows, nil)
		}
		s.Windows[i] = append(s.Windows[i], tabOf(l))
	}
	var rest []liveSession
	for _, l := range ls {
		if !done[l.SessionID] {
			rest = append(rest, l)
		}
	}
	if len(rest) > 0 {
		slices.SortFunc(rest, func(a, b liveSession) int { return a.Started.Compare(b.Started) })
		var w []Tab
		for _, l := range rest {
			w = append(w, tabOf(l))
		}
		s.Windows = append(s.Windows, w)
	}
	if err := write(q.Root, s); err != nil {
		record(q.Root, "save", "", err, s.Windows)
		return "", err
	}
	msg := "saved " + size(s.Windows)
	if gerr != nil {
		msg += "; Ghostty's layout was unavailable, so they're in one window (" + gerr.Error() + ")"
	}
	record(q.Root, "save", msg, nil, s.Windows)
	return msg, nil
}

// write replaces the save in one step, so a crash mid-write can't lose the last one.
func write(root string, s Saved) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(root, ".claude-sessions-*") // 0600
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), filepath.Join(root, file))
	}
	if err != nil {
		os.Remove(f.Name())
	}
	return err
}

// Plan is what a restore would do: the saved tabs still to open, by window, and what it skips.
type Plan struct {
	Open    [][]Tab
	Running int // already running, so not opened again
	Gone    int // transcript deleted, or not a session id: can't be resumed
	root    string
}

// Prepare reads the last save and checks each session against what is running now.
func Prepare(q *queue.Queue) (Plan, error) {
	s, err := Load(q.Root)
	if errors.Is(err, fs.ErrNotExist) {
		return Plan{}, errors.New("no saved sessions yet")
	}
	if err != nil {
		return Plan{}, err
	}
	running := map[string]bool{}
	for _, l := range live(q) {
		running[l.SessionID] = true
	}
	p := Plan{root: q.Root}
	for _, w := range s.Windows {
		var open []Tab
		for _, t := range w {
			_, serr := os.Stat(t.TranscriptPath)
			switch {
			case running[t.SessionID]:
				p.Running++
			case !uuid.MatchString(t.SessionID) || t.TranscriptPath == "" || serr != nil:
				p.Gone++
			default:
				open = append(open, t)
			}
		}
		if len(open) > 0 {
			p.Open = append(p.Open, open)
		}
	}
	return p, nil
}

// Summary is "2 windows · 5 sessions (1 running, 1 gone)".
func (p Plan) Summary() string {
	var skip []string
	if p.Running > 0 {
		skip = append(skip, fmt.Sprintf("%d running", p.Running))
	}
	if p.Gone > 0 {
		skip = append(skip, fmt.Sprintf("%d gone", p.Gone))
	}
	msg := size(p.Open)
	if len(skip) > 0 {
		msg += " (" + strings.Join(skip, ", ") + ")"
	}
	return msg
}

// Restore opens the plan's windows and tabs, each resuming its session in its folder.
func (p Plan) Restore() (string, error) {
	msg, err := p.restore()
	record(p.root, "restore", msg, err, p.Open)
	return msg, err
}

func (p Plan) restore() (string, error) {
	if len(p.Open) == 0 {
		return "nothing to restore: " + p.Summary(), nil
	}
	for _, w := range p.Open {
		for _, t := range w {
			if !uuid.MatchString(t.SessionID) { // checked again here: the id reaches a shell
				return "", fmt.Errorf("not a session id: %q", t.SessionID)
			}
		}
	}
	if err := openWindows(p.Open); err != nil {
		return "", err
	}
	return "restored " + p.Summary(), nil
}

// Command runs /handoff:claude-sessions with its argument and returns the line to show the user.
func Command(q *queue.Queue, arg string, now time.Time) string {
	var msg string
	var err error
	switch arg {
	case "save":
		msg, err = Save(q, now)
	case "restore":
		var p Plan
		if p, err = Prepare(q); err == nil {
			msg, err = p.Restore()
		}
	case "":
		var s Saved
		if s, err = Load(q.Root); errors.Is(err, fs.ErrNotExist) {
			return "no saved sessions yet; /handoff:claude-sessions save | restore"
		}
		msg = s.Label(now) + "; /handoff:claude-sessions save | restore"
	default:
		return "usage: /handoff:claude-sessions save | restore"
	}
	if err != nil {
		return strings.TrimSpace("claude-sessions "+arg) + ": " + err.Error()
	}
	return msg
}
