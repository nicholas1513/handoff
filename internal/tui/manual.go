package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

const manualKey = "? manual"

// manualText is the viewer's own overview. Headings are the lines that don't start with a space. Keep it brief: the
// README has the detail.
const manualText = `What this is
  An agent stages commands it can't run from its sandbox. You review each one here and run it as yourself, and the agent gets the result back.

Reviewing
  Sessions, items and detail are the three panes. tab or a click changes pane, ↑↓ moves. Detail shows what the item does, which session and prompt it came from, the exact command, and any warnings.

Running
  space twice, or Run then Yes, runs the selected item. x or Reject refuses it. Several can run at once: c twice stops the selected one. o reviews an item again after it changed.

File edits and commits
  An edit shows as a diff of the file: + shows more context. Commits show as git made them, in a review copy of the repo, here or in VS Code (see settings): v opens it there.

Saving and restoring sessions
  ctrl+s saves the Claude sessions running on this machine, with their Ghostty windows and tabs, then offers to close those tabs: ← → choose, space or enter selects, and No is the default. ctrl+l asks to reopen the last save: one window per saved window, each tab resuming its session in its folder. Sessions still running aren't opened twice. Space confirms, x cancels. The Sessions pane title shows the last save. From a Claude tab, /handoff:claude-sessions save or restore does the same.

Settings
  s opens them: diff and attachment word wrap, and where commits are reviewed. This manual is there too.

Leaving
  q quits. While a run is in progress it asks for a second q.`

// manualRows is the manual wrapped to width w.
func manualRows(w int) []string {
	var out []string
	for _, l := range strings.Split(manualText, "\n") {
		text, heading := strings.CutPrefix(l, "  ")
		if !heading && l != "" {
			out = append(out, section.Render(l))
			continue
		}
		if l == "" {
			out = append(out, "")
			continue
		}
		out = append(out, strings.Split(ansi.Wordwrap(text, w, ""), "\n")...)
	}
	return out
}

// manualSize is the text's width and how many of its rows show at once.
func (m model) manualSize() (w, h int) { return max(20, min(72, m.w-6)), max(3, m.h-8) }

func (m *model) scrollManual(d int) {
	w, h := m.manualSize()
	m.manualTop = max(0, min(m.manualTop+d, len(manualRows(w))-h))
}

// manualOverlay is the manual as drawn, and where it sits (top-left cell) when centred in the window.
func (m model) manualOverlay() (lines []string, x, y int) {
	w, h := m.manualSize()
	rows := manualRows(w)
	end := min(len(rows), m.manualTop+h)
	shown := append([]string(nil), rows[m.manualTop:end]...)
	for i := range shown {
		shown[i] = pad(shown[i], w)
	}
	hint := "esc close"
	if len(rows) > h {
		hint = fmt.Sprintf("↑↓ scroll · esc close · %d-%d of %d", m.manualTop+1, end, len(rows))
	}
	shown = append(shown, "", dim.Render(hint))
	lines = strings.Split(focused.Padding(0, 1).Render(strings.Join(shown, "\n")), "\n")
	return lines, max(0, (m.w-lipgloss.Width(lines[0]))/2), max(0, (m.h-len(lines))/2)
}

// manualKeyPress handles every key while the manual is open. Closing returns to what was below: the settings overlay
// if it was opened from there.
func (m model) manualKeyPress(k string) (tea.Model, tea.Cmd) {
	_, h := m.manualSize()
	switch k {
	case "esc", "q", "?":
		m.manual = false
	case "up", "k":
		m.scrollManual(-1)
	case "down", "j":
		m.scrollManual(1)
	case "pgup", "[":
		m.scrollManual(-h)
	case "pgdown", "]":
		m.scrollManual(h)
	}
	return m, nil
}

// manualMouse handles every mouse event while the manual is open: the wheel scrolls, a click outside closes it, and
// nothing reaches what's behind.
func (m model) manualMouse(msg tea.MouseMsg, press bool) (tea.Model, tea.Cmd) {
	switch {
	case msg.Button == tea.MouseButtonWheelUp:
		m.scrollManual(-1)
	case msg.Button == tea.MouseButtonWheelDown:
		m.scrollManual(1)
	case press:
		ol, ox, oy := m.manualOverlay()
		if msg.X < ox || msg.X >= ox+lipgloss.Width(ol[0]) || msg.Y < oy || msg.Y >= oy+len(ol) {
			m.manual = false
		}
	}
	return m, nil
}

func (m *model) openManual() { m.manual, m.manualTop = true, 0 }
