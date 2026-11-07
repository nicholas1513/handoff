package claudesessions

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/nicholas1513/handoff/internal/queue"
)

// Closable is how many tabs of the last save are still running, and how many of those are working right now.
func Closable(q *queue.Queue) (tabs, busy int) {
	for _, l := range savedAndLive(q) {
		tabs++
		if l.busy {
			busy++
		}
	}
	return tabs, busy
}

// savedAndLive is the running sessions the last save holds. It starts from what's running now, by session id, so a
// tab that moved on with /clear, or started since, isn't touched.
func savedAndLive(q *queue.Queue) []liveSession {
	s, err := Load(q.Root)
	if err != nil {
		return nil
	}
	saved := map[string]bool{}
	for _, w := range s.Windows {
		for _, t := range w {
			saved[t.SessionID] = true
		}
	}
	var out []liveSession
	for _, l := range live(q) {
		if saved[l.SessionID] {
			out = append(out, l)
		}
	}
	return out
}

// Close ends the tabs of the sessions in the last save that are still running. A tab closes when its shell exits, so
// that's who is told, along with Claude: the transcripts are on disk and a restore resumes them.
//
// The viewer may be running inside one of those tabs (`! handoff`). That tab goes last, since it ends this process
// too, and it's left open when keepOwn is set: a run in progress would die with the viewer.
func Close(q *queue.Queue, keepOwn bool, settle time.Duration) (string, error) {
	own, ok := sessionLeader(0)
	if !ok {
		return "", errors.New("can't tell this terminal's session")
	}
	var leaders []int
	seen, kept := map[int]bool{}, 0
	claude := map[int]int{}
	var tabs []Tab
	for _, l := range savedAndLive(q) {
		sid, ok := sessionLeader(l.ClaudePID)
		if !ok || sid <= 1 || seen[sid] {
			continue
		}
		// The leader must hold the same terminal: it's the tab's shell, not something the pid's session turned into.
		if p, ok := proc(sid); !ok || p.tty != l.tty {
			continue
		}
		seen[sid] = true
		if sid == own && keepOwn {
			kept++
			continue
		}
		leaders = append(leaders, sid)
		claude[sid] = l.ClaudePID
		tabs = append(tabs, tabOf(l))
	}
	slices.SortFunc(leaders, func(a, b int) int { // own last
		switch {
		case a == own:
			return 1
		case b == own:
			return -1
		}
		return a - b
	})
	// Written first, and as what's about to happen: the viewer's own tab may end this process.
	record(q.Root, "close", fmt.Sprintf("closing %s, keeping %d", plural(len(leaders), "tab"), kept), nil, [][]Tab{tabs})
	closed := 0
	for _, sid := range leaders {
		if sid == own {
			time.Sleep(settle) // let the others go first: the rest of this function won't run
		}
		err := hangUp(claude[sid])
		if serr := hangUp(sid); serr != nil && err != nil {
			record(q.Root, "close", "", fmt.Errorf("after %s: %w", plural(closed, "tab"), serr), nil)
			return fmt.Sprintf("closed %s", plural(closed, "tab")), serr
		}
		closed++
	}
	msg := "closed " + plural(closed, "tab")
	if kept > 0 {
		msg += "; kept this tab while a run is in progress"
	}
	return msg, nil
}
