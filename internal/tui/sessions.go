package tui

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/nicholas1513/handoff/internal/claudesessions"
	"github.com/nicholas1513/handoff/internal/queue"
)

// Saving (ctrl+s) and restoring (ctrl+l, twice) the running Claude sessions. Tests replace these.
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

// armRestore shows what a second ctrl+l would open. With nothing to open it only says so.
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

// restoreCmd checks again what is running, so a session started since the first ctrl+l isn't opened twice.
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
