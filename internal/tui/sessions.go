package tui

import (
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/nicholas1513/handoff/internal/claudesessions"
	"github.com/nicholas1513/handoff/internal/queue"
)

// Saving (ctrl+s) and restoring (ctrl+l, confirmed in an overlay) the running Claude sessions. Tests replace these.
var (
	saveSessions    = claudesessions.Save
	prepareRestore  = claudesessions.Prepare
	restoreSessions = func(p claudesessions.Plan) (string, error) { return p.Restore() }
)

// savedLabel is the Sessions pane's note of the last save, or "" when there is none.
func savedLabel(root string, now time.Time) string {
	s, err := claudesessions.Load(root)
	if err != nil {
		return ""
	}
	return s.Label(now)
}

// saveCmd saves off the event loop: it asks Ghostty for its layout.
func saveCmd(q *queue.Queue, now time.Time) tea.Cmd {
	return func() tea.Msg {
		msg, err := saveSessions(q, now)
		if err != nil {
			return statusMsg("save sessions: " + err.Error())
		}
		return statusMsg(msg)
	}
}

// The restore overlay's button row, counted from its top border, and its buttons, from its left border.
const (
	restoreRow     = 4
	restoreBtn     = "[ Restore ]"
	restoreRejectB = "[ Reject ]"
	restoreBtnX    = 2
	restoreRejectX = restoreBtnX + 13
)

// restoreOverlay is the restore confirmation as drawn, and where it sits (top-left cell) when centred in the window.
func (m model) restoreOverlay() (lines []string, x, y int) {
	rows := []string{section.Render("Restore sessions"), m.restoreArmed, "", restoreBtn + "  " + restoreRejectB,
		dim.Render("space restore · x reject")}
	lines = strings.Split(focused.Padding(0, 1).Render(strings.Join(rows, "\n")), "\n")
	w := lipgloss.Width(lines[0])
	return lines, max(0, (m.w-w)/2), max(0, (m.h-len(lines))/2)
}

// restoreClick acts on the overlay's buttons. Any other click is dropped: it must not land on what's behind.
func (m model) restoreClick(x, y int) (tea.Model, tea.Cmd) {
	_, ox, oy := m.restoreOverlay()
	if y != oy+restoreRow {
		return m, nil
	}
	switch c := x - ox; {
	case c >= restoreBtnX && c < restoreBtnX+len(restoreBtn):
		m.restoreArmed = ""
		return m, restoreCmd(m.q)
	case c >= restoreRejectX && c < restoreRejectX+len(restoreRejectB):
		m.restoreArmed = ""
	}
	return m, nil
}

// armRestore opens the overlay on what a restore would open. With nothing to open it only says so.
func (m *model) armRestore() {
	p, err := prepareRestore(m.q)
	switch {
	case err != nil:
		m.status = "restore sessions: " + err.Error()
	case len(p.Open) == 0:
		m.status = "nothing to restore: " + p.Summary()
	default:
		m.restoreArmed = p.Summary()
	}
}

// restoreCmd checks again what is running, so a session started since the overlay opened isn't opened twice.
func restoreCmd(q *queue.Queue) tea.Cmd {
	return func() tea.Msg {
		p, err := prepareRestore(q)
		var msg string
		if err == nil {
			msg, err = restoreSessions(p)
		}
		if err != nil {
			return statusMsg("restore sessions: " + err.Error())
		}
		return statusMsg(msg)
	}
}
