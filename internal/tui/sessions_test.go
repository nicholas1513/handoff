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

func TestCtrlLOpensOverlaySpaceRestores(t *testing.T) {
	_, m := setup(t)
	m.w, m.h = 120, 40
	restored := fakeSessions(t, 2)
	m, cmd := key(m, tea.KeyCtrlL)
	if cmd != nil || *restored != 0 {
		t.Fatal("ctrl+l must only open the overlay")
	}
	v := ansi.Strip(m.View())
	for _, want := range []string{"1 window · 2 sessions (1 running)", "[ Restore ]  [ Reject ]", "space restore · x reject"} {
		if !strings.Contains(v, want) {
			t.Fatalf("overlay is missing %q:\n%s", want, v)
		}
	}
	m, cmd = press(m, " ")
	if cmd == nil {
		t.Fatal("space didn't restore")
	}
	next, _ := m.Update(cmd())
	if m = next.(model); *restored != 1 || m.status != "restored" || m.restoreArmed != "" {
		t.Fatalf("restored=%d status=%q armed=%q", *restored, m.status, m.restoreArmed)
	}
}

func TestXRejectsTheRestoreNotTheItem(t *testing.T) {
	q, m := setup(t)
	restored := fakeSessions(t, 1)
	m, _ = key(m, tea.KeyCtrlL)
	m, cmd := press(m, "x")
	if cmd != nil || m.restoreArmed != "" || *restored != 0 {
		t.Fatalf("cmd=%v armed=%q restored=%d", cmd != nil, m.restoreArmed, *restored)
	}
	if st := q.State(*m.item(), time.Now()); st != queue.Pending {
		t.Fatalf("item state = %v: x reached the item behind the overlay", st)
	}
	if m.armed || m.confirm {
		t.Fatal("x armed something behind the overlay")
	}
}

// Nothing reaches the item, list or panes behind the overlay, and it stays open until answered.
func TestRestoreOverlaySwallowsKeysAndMouse(t *testing.T) {
	q, m := setup(t)
	m.w, m.h = 120, 40
	restored := fakeSessions(t, 1)
	m, _ = key(m, tea.KeyCtrlL)
	sel, focus, armed := m.ii, m.focus, m.restoreArmed
	for _, k := range []string{"j", "k", "r", "q", "s", "o", "y", "c", "\t"} {
		var cmd tea.Cmd
		if m, cmd = press(m, k); cmd != nil {
			t.Fatalf("%q returned a command", k)
		}
	}
	m, cmd := key(m, tea.KeyCtrlL)
	if cmd != nil {
		t.Fatal("ctrl+l again restored: space confirms")
	}
	m, _ = click(m, 1, listTop) // behind the overlay, in the sessions pane
	sw, _, _ := columns(m.w)
	m, _ = click(m, sw+2, listTop) // the items pane
	for _, b := range []tea.MouseButton{tea.MouseButtonWheelUp, tea.MouseButtonWheelDown} {
		next, _ := m.Update(tea.MouseMsg{X: sw + 2, Y: listTop, Action: tea.MouseActionPress, Button: b})
		m = next.(model)
	}
	if m.ii != sel || m.focus != focus || m.restoreArmed != armed || m.menu || m.armed || m.confirm || *restored != 0 {
		t.Fatalf("leaked: ii=%d/%d focus=%d/%d armed=%q menu=%v run=%v confirm=%v restored=%d",
			m.ii, sel, m.focus, focus, m.restoreArmed, m.menu, m.armed, m.confirm, *restored)
	}
	if st := q.State(*m.item(), time.Now()); st != queue.Pending {
		t.Fatalf("item state = %v", st)
	}
}

func TestRestoreOverlayButtons(t *testing.T) {
	_, m := setup(t)
	m.w, m.h = 120, 40
	restored := fakeSessions(t, 1)
	m, _ = key(m, tea.KeyCtrlL)
	_, ox, oy := m.restoreOverlay()
	m, cmd := click(m, ox+restoreRejectX+1, oy+restoreRow)
	if cmd != nil || m.restoreArmed != "" {
		t.Fatalf("Reject: cmd=%v armed=%q", cmd != nil, m.restoreArmed)
	}
	m, _ = key(m, tea.KeyCtrlL)
	m, cmd = click(m, ox+restoreBtnX+1, oy+restoreRow)
	if cmd == nil || m.restoreArmed != "" {
		t.Fatal("Restore button didn't restore")
	}
	cmd()
	if *restored != 1 {
		t.Fatalf("restored = %d", *restored)
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
