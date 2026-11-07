package claudesessions

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// closeWorld has each pid p led by shell p+1000 on the same tty, and this process's session at own.
func closeWorld(t *testing.T, w *world, own int) *[]int {
	var hung []int
	oldL, oldH := sessionLeader, hangUp
	sessionLeader = func(pid int) (int, bool) {
		if pid == 0 {
			return own, true
		}
		return pid + 1000, true
	}
	hangUp = func(pid int) error { hung = append(hung, pid); return nil }
	t.Cleanup(func() { sessionLeader, hangUp = oldL, oldH })
	for pid, p := range w.pids {
		if pid < 1000 {
			w.pids[pid+1000] = procInfo{tty: p.tty, name: "fish"}
		}
	}
	return &hung
}

func TestCloseEndsOnlySavedTabsStillRunning(t *testing.T) {
	w := newWorld(t)
	w.listed(t, idA, 1, map[string]any{"status": "busy"})
	w.listed(t, idB, 2, nil)
	w.listed(t, idC, 3, nil)
	if _, err := Save(w.q, t0); err != nil {
		t.Fatal(err)
	}
	w.listed(t, idD, 4, nil)                                       // started after the save
	os.Remove(filepath.Join(claudeDir(), "sessions", idC+".json")) // /clear: same process, new id
	w.listed(t, "eeeeeeee-0000-4000-8000-000000000005", 3, nil)
	if tabs, busy := Closable(w.q); tabs != 2 || busy != 1 {
		t.Fatalf("tabs=%d busy=%d, want A and B, A busy", tabs, busy)
	}
	hung := closeWorld(t, w, 9000)
	msg, err := Close(w.q, false, 0)
	if err != nil || msg != "closed 2 tabs" {
		t.Fatalf("msg=%q err=%v", msg, err)
	}
	slices.Sort(*hung)
	if want := []int{1, 2, 1001, 1002}; !reflect.DeepEqual(*hung, want) {
		t.Fatalf("hung = %v, want only A and B's", *hung)
	}
}

// Own tab goes last, or not at all while a run is in progress.
func TestCloseHandlesTheViewersOwnTab(t *testing.T) {
	w := newWorld(t)
	w.listed(t, idA, 1, nil)
	w.listed(t, idB, 2, nil)
	Save(w.q, t0)
	hung := closeWorld(t, w, 1001)
	if _, err := Close(w.q, false, 0); err != nil {
		t.Fatal(err)
	}
	if want := []int{2, 1002, 1, 1001}; !reflect.DeepEqual(*hung, want) {
		t.Fatalf("hung = %v, want %v", *hung, want)
	}
	*hung = nil
	msg, err := Close(w.q, true, 0)
	if err != nil || slices.Contains(*hung, 1001) || msg != "closed 1 tab; kept this tab while a run is in progress" {
		t.Fatalf("msg=%q err=%v hung=%v", msg, err, *hung)
	}
}

// A leader on another terminal isn't this tab's shell: nothing is signalled.
func TestCloseSkipsALeaderOnAnotherTerminal(t *testing.T) {
	w := newWorld(t)
	w.listed(t, idA, 1, nil)
	Save(w.q, t0)
	hung := closeWorld(t, w, 9000)
	w.pids[1001] = procInfo{tty: 555, name: "fish"}
	if _, err := Close(w.q, false, 0); err != nil || len(*hung) != 0 {
		t.Fatalf("err=%v hung=%v", err, *hung)
	}
}

func TestClosableWithNoSave(t *testing.T) {
	w := newWorld(t)
	if tabs, _ := Closable(w.q); tabs != 0 {
		t.Fatalf("tabs = %d", tabs)
	}
}

func logLines(t *testing.T, w *world) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(w.q.Root, logFile))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func TestLogRecordsSaveCloseAndRestore(t *testing.T) {
	w := newWorld(t)
	w.listed(t, idA, 1, nil)
	if _, err := Save(w.q, t0); err != nil {
		t.Fatal(err)
	}
	closeWorld(t, w, 9000)
	if _, err := Close(w.q, false, 0); err != nil {
		t.Fatal(err)
	}
	l := logLines(t, w)
	if len(l) != 2 || !strings.Contains(l[0], ` save "saved 1 window`) || !strings.Contains(l[0], idA[:8]) ||
		!strings.Contains(l[1], ` close "closing 1 tab, keeping 0"`) || !strings.Contains(l[1], idA[:8]) {
		t.Fatalf("log:\n%s", strings.Join(l, "\n"))
	}
}

func TestLogKeepsAFolderOnOneLine(t *testing.T) {
	root := t.TempDir()
	record(root, "restore", "", errors.New("boom"), [][]Tab{{{SessionID: idA, Cwd: "/tmp/a\nb"}}})
	b, err := os.ReadFile(filepath.Join(root, logFile))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSuffix(string(b), "\n"); strings.Contains(got, "\n") || !strings.Contains(got, `"failed: boom"`) {
		t.Fatalf("log: %q", b)
	}
}

func TestLogRecordsARestore(t *testing.T) {
	w := newWorld(t)
	w.session(t, idA, 1, t0)
	Save(w.q, t0)
	delete(w.pids, 1)
	p, err := Prepare(w.q)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Restore(); err != nil {
		t.Fatal(err)
	}
	l := logLines(t, w)
	if last := l[len(l)-1]; !strings.Contains(last, ` restore "restored 1 window`) || !strings.Contains(last, idA[:8]) {
		t.Fatalf("log:\n%s", strings.Join(l, "\n"))
	}
}
