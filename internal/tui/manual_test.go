package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/nicholas1513/handoff/internal/queue"
)

func TestManualOpensFromAKeyAndCoversSaving(t *testing.T) {
	_, m := setup(t)
	m.w, m.h = 120, 60
	if f, _ := m.footer(); !strings.Contains(ansi.Strip(f), manualKey) {
		t.Fatalf("footer doesn't offer the manual: %q", ansi.Strip(f))
	}
	m, _ = press(m, "?")
	v := ansi.Strip(m.View())
	for _, want := range []string{"Saving and restoring sessions", "ctrl+s saves", "ctrl+l asks"} {
		if !strings.Contains(v, want) {
			t.Fatalf("manual is missing %q:\n%s", want, v)
		}
	}
	if m, _ = key(m, tea.KeyEsc); m.manual {
		t.Fatal("esc didn't close it")
	}
}

// From settings, by key and by click; closing goes back to settings.
func TestManualOpensFromSettings(t *testing.T) {
	_, m := setup(t)
	m.w, m.h = 120, 60
	m, _ = press(m, "s")
	for range settingRows {
		m, _ = key(m, tea.KeyDown)
	}
	if !strings.Contains(ansi.Strip(m.View()), "› Manual") {
		t.Fatalf("Manual row isn't selectable:\n%s", ansi.Strip(m.View()))
	}
	settings := m.settings
	m, _ = press(m, " ")
	if !m.manual || m.settings != settings {
		t.Fatalf("space on Manual: manual=%v settings changed=%v", m.manual, m.settings != settings)
	}
	m, _ = key(m, tea.KeyEsc)
	if m.manual || !m.menu {
		t.Fatalf("closing: manual=%v settings=%v, want back in settings", m.manual, m.menu)
	}
	_, ox, oy := m.overlay()
	m, _ = click(m, ox+3, oy+settingsRow+len(settingRows))
	if !m.manual {
		t.Fatal("clicking the Manual row didn't open it")
	}
}

func TestManualScrollsInASmallWindow(t *testing.T) {
	_, m := setup(t)
	m.w, m.h = 60, 20
	m, _ = press(m, "?")
	m, _ = key(m, tea.KeyUp)
	if m.manualTop != 0 {
		t.Fatalf("scrolled above the top: %d", m.manualTop)
	}
	m, _ = key(m, tea.KeyDown)
	if m.manualTop != 1 {
		t.Fatalf("top = %d", m.manualTop)
	}
	for range 200 {
		m, _ = key(m, tea.KeyDown)
	}
	w, h := m.manualSize()
	if want := len(manualRows(w)) - h; m.manualTop != want {
		t.Fatalf("top = %d, want %d", m.manualTop, want)
	}
	ol, _, _ := m.manualOverlay()
	for _, l := range ol {
		if ansi.StringWidth(l) > m.w {
			t.Fatalf("overlay wider than the window: %q", ansi.Strip(l))
		}
	}
}

func TestManualSwallowsKeysAndMouse(t *testing.T) {
	q, m := setup(t)
	m.w, m.h = 120, 60
	m, _ = press(m, "?")
	sel, focus := m.ii, m.focus
	for _, k := range []string{"x", "r", " ", "s", "c", "o"} {
		m, _ = press(m, k)
	}
	m, _ = click(m, m.w/2, m.h/2) // inside: dropped
	sw, _, _ := columns(m.w)
	next, _ := m.Update(tea.MouseMsg{X: sw + 2, Y: listTop, Action: tea.MouseActionPress, Button: tea.MouseButtonWheelDown})
	m = next.(model)
	if !m.manual || m.ii != sel || m.focus != focus || m.armed || m.menu {
		t.Fatalf("leaked: manual=%v ii=%d focus=%d armed=%v menu=%v", m.manual, m.ii, m.focus, m.armed, m.menu)
	}
	if st := q.State(*m.item(), time.Now()); st != queue.Pending {
		t.Fatalf("item state = %v", st)
	}
	sel = m.ii
	m, _ = click(m, 1, listTop) // outside closes, and doesn't select behind it
	if m.manual || m.ii != sel {
		t.Fatalf("outside click: manual=%v moved=%v", m.manual, m.ii != sel)
	}
}
