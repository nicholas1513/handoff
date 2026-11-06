package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/nicholas1513/handoff/internal/claudesessions"
	"github.com/nicholas1513/handoff/internal/queue"
)

func key(m model, t tea.KeyType) (model, tea.Cmd) {
	next, cmd := m.Update(tea.KeyMsg{Type: t})
	return next.(model), cmd
}

// fakeSessions plans one window to restore and counts restores.
func fakeSessions(t *testing.T, open int) *int {
	restored := 0
	oldP, oldR, oldS := prepareRestore, restoreSessions, saveSessions
	prepareRestore = func(*queue.Queue) (claudesessions.Plan, error) {
		p := claudesessions.Plan{Running: 1}
		if open > 0 {
			p.Open = [][]claudesessions.Tab{make([]claudesessions.Tab, open)}
		}
		return p, nil
	}
	restoreSessions = func(claudesessions.Plan) (string, error) { restored++; return "restored", nil }
	saveSessions = func(*queue.Queue, time.Time) (string, error) { return "saved 1 window · 2 sessions", nil }
	t.Cleanup(func() { prepareRestore, restoreSessions, saveSessions = oldP, oldR, oldS })
	return &restored
}

func TestCtrlLTwiceRestores(t *testing.T) {
	_, m := setup(t)
	restored := fakeSessions(t, 2)
	m, cmd := key(m, tea.KeyCtrlL)
	if cmd != nil || *restored != 0 {
		t.Fatal("one ctrl+l must only arm")
	}
	if f, _ := m.footer(); !strings.Contains(ansi.Strip(f), "restore 1 window · 2 sessions (1 running): ctrl+l again") {
		t.Fatalf("footer doesn't say what ctrl+l again restores:\n%s", ansi.Strip(f))
	}
	m, cmd = key(m, tea.KeyCtrlL)
	if cmd == nil {
		t.Fatal("second ctrl+l didn't restore")
	}
	next, _ := m.Update(cmd())
	if m = next.(model); *restored != 1 || m.status != "restored" || m.restoreArmed != "" {
		t.Fatalf("restored=%d status=%q armed=%q", *restored, m.status, m.restoreArmed)
	}
}

func TestOtherKeyDisarmsRestore(t *testing.T) {
	_, m := setup(t)
	restored := fakeSessions(t, 1)
	m, _ = key(m, tea.KeyCtrlL)
	sel := m.ii
	m, cmd := press(m, "j") // only disarms: not acted on
	if cmd != nil || m.restoreArmed != "" || m.ii != sel {
		t.Fatalf("cmd=%v armed=%q moved=%v", cmd != nil, m.restoreArmed, m.ii != sel)
	}
	m, _ = key(m, tea.KeyCtrlL)
	m, _ = click(m, 1, listTop) // a click disarms too
	if _, cmd = key(m, tea.KeyCtrlL); cmd != nil || *restored != 0 {
		t.Fatal("ctrl+l after a click restored without asking again")
	}
}

func TestCtrlLWithNothingToOpenDoesNotArm(t *testing.T) {
	_, m := setup(t)
	fakeSessions(t, 0)
	if m, _ = key(m, tea.KeyCtrlL); m.restoreArmed != "" || !strings.HasPrefix(m.status, "nothing to restore") {
		t.Fatalf("armed=%q status=%q", m.restoreArmed, m.status)
	}
}

func TestCtrlSSaves(t *testing.T) {
	_, m := setup(t)
	fakeSessions(t, 0)
	m, cmd := key(m, tea.KeyCtrlS)
	if cmd == nil {
		t.Fatal("ctrl+s didn't save")
	}
	next, _ := m.Update(cmd())
	if m = next.(model); m.status != "saved 1 window · 2 sessions" {
		t.Fatalf("status = %q", m.status)
	}
}

// The last save shows in the Sessions pane title, also one made by /handoff:claude-sessions in a Claude tab.
func TestSessionsTitleShowsLastSave(t *testing.T) {
	q, m := setup(t)
	at := time.Now().Add(-time.Minute)
	b := `{"saved_at":"` + at.Format(time.RFC3339Nano) + `","windows":[[{"session_id":"x"}],[{"session_id":"y"},{"session_id":"z"}]]}`
	if err := os.WriteFile(filepath.Join(q.Root, "claude-sessions.json"), []byte(b), 0o600); err != nil {
		t.Fatal(err)
	}
	m.refresh()
	want := "Sessions · saved " + at.Format("15:04") + " · 2 windows · 3 sessions"
	if got := ansi.Strip(m.sessionsPane(60, 20)[1]); !strings.Contains(got, want) {
		t.Fatalf("title row = %q, want %q", got, want)
	}
	// A narrow pane cuts the counts first; when the save ran stays readable.
	if got := ansi.Strip(m.sessionsPane(26, 20)[1]); !strings.Contains(got, "saved "+at.Format("15:04")) || ansi.StringWidth(got) != 26 {
		t.Fatalf("narrow title row = %q", got)
	}
}
