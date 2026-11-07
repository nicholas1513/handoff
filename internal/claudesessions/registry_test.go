package claudesessions

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"
)

// listed writes Claude Code's file for a session and its transcript, with a start that matches pid's process.
func (w *world) listed(t *testing.T, id string, pid int, extra map[string]any) {
	dir := claudeDir()
	r := map[string]any{"pid": pid, "sessionId": id, "cwd": "/src/" + id[:1], "startedAt": t0.UnixMilli(), "kind": "interactive"}
	if w.pids[pid] == (procInfo{}) {
		w.pids[pid] = procInfo{start: t0.UnixMicro(), tty: int64(pid * 100), name: "claude"}
	}
	r["procStart"] = strconv.FormatInt(w.pids[pid].start, 10)
	for k, v := range extra {
		r[k] = v
	}
	b, _ := json.Marshal(r)
	os.MkdirAll(filepath.Join(dir, "sessions"), 0o700)
	os.MkdirAll(filepath.Join(dir, "projects", "-src"), 0o700)
	if err := os.WriteFile(filepath.Join(dir, "sessions", filepath.Base(id)+".json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "projects", "-src", id+".jsonl"), []byte("{}\n"), 0o600)
}

func saved(t *testing.T, w *world) [][]string {
	s, err := Load(w.q.Root)
	if err != nil {
		t.Fatal(err)
	}
	return ids(s.Windows)
}

// A session the plugin never saw is saved, and restore can find its transcript.
func TestSaveIncludesSessionsOnlyClaudeLists(t *testing.T) {
	w := newWorld(t)
	w.listed(t, idA, 1, nil)
	if _, err := Save(w.q, t0); err != nil {
		t.Fatal(err)
	}
	if got := saved(t, w); !reflect.DeepEqual(got, [][]string{{"a"}}) {
		t.Fatalf("windows = %v", got)
	}
	p, err := Prepare(w.q)
	if err != nil || p.Running != 1 {
		t.Fatalf("Prepare = %+v %v, want it counted as running", p, err)
	}
	delete(w.pids, 1)
	p, _ = Prepare(w.q)
	if len(p.Open) != 1 || p.Gone != 0 {
		t.Fatalf("Prepare = %+v, want it to open", p)
	}
}

// /clear: Claude's file has the current id, the hook's record for that process may be behind.
func TestClaudesListWinsOverAnOlderHookRecord(t *testing.T) {
	w := newWorld(t)
	w.session(t, idA, 1, t0)
	w.listed(t, idB, 1, nil)
	Save(w.q, t0)
	if got := saved(t, w); !reflect.DeepEqual(got, [][]string{{"b"}}) {
		t.Fatalf("windows = %v, want only Claude's current id", got)
	}
}

// The fallback: a process Claude doesn't list, or lists in a form this doesn't read, is still found through the hook.
func TestHookRecordsCoverWhatClaudeDoesNotList(t *testing.T) {
	w := newWorld(t)
	w.session(t, idA, 1, t0)
	w.session(t, idB, 2, t0)
	w.listed(t, idC, 3, nil)
	os.WriteFile(filepath.Join(claudeDir(), "sessions", "junk.json"), []byte("{not json"), 0o600)
	w.listed(t, idD, 2, map[string]any{"sessionId": "not-a-uuid"})
	Save(w.q, t0)
	s, _ := Load(w.q.Root)
	if count(s.Windows) != 3 {
		t.Fatalf("windows = %v, want a, b and c", ids(s.Windows))
	}
}

func TestLeftOverAndOtherSessionsAreSkipped(t *testing.T) {
	w := newWorld(t)
	w.listed(t, idA, 1, nil) // crashed: no such process
	delete(w.pids, 1)
	w.listed(t, idB, 2, nil) // pid reused since
	w.pids[2] = procInfo{start: 1, tty: 200}
	w.listed(t, idC, 3, map[string]any{"kind": "background"})
	w.listed(t, idD, 4, nil) // no terminal
	w.pids[4] = procInfo{start: w.pids[4].start, tty: noTTY}
	if _, err := Save(w.q, t0); err == nil {
		t.Fatalf("saved %v, want nothing live", saved(t, w))
	}
}

func TestStartedNear(t *testing.T) {
	ms := t0.UnixMilli()
	if !startedNear(ms, t0.Add(-3*time.Second).UnixMicro()) || startedNear(ms, t0.Add(-time.Hour).UnixMicro()) {
		t.Fatal("startedNear")
	}
}
