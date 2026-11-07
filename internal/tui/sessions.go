package tui

import (
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/nicholas1513/handoff/internal/claudesessions"
	"github.com/nicholas1513/handoff/internal/queue"
)

// Saving (ctrl+s) and restoring (ctrl+l, confirmed in a prompt) the running Claude sessions. Tests replace these.
var (
	saveSessions = claudesessions.Save
	closableTabs = claudesessions.Closable
	closeTabs    = func(q *queue.Queue, keepOwn bool) (string, error) {
		return claudesessions.Close(q, keepOwn, closeSettle)
	}
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

// closeSettle is how long the viewer's own tab waits for the others before it goes too.
const closeSettle = 300 * time.Millisecond

// savedMsg is a finished save, and the saved tabs the viewer can offer to close.
type savedMsg struct {
	status     string
	tabs, busy int
}

// saveCmd saves off the event loop: it asks Ghostty for its layout.
func saveCmd(q *queue.Queue, now time.Time) tea.Cmd {
	return func() tea.Msg {
		msg, err := saveSessions(q, now)
		if err != nil {
			return statusMsg("save sessions: " + err.Error())
		}
		tabs, busy := closableTabs(q)
		return savedMsg{msg, tabs, busy}
	}
}

// askClose opens the close prompt, which defaults to No.
func (m *model) askClose(s savedMsg) {
	m.status = s.status
	if s.tabs == 0 {
		return
	}
	q := fmt.Sprintf("Close the %d saved Claude tabs?", s.tabs)
	if s.tabs == 1 {
		q = "Close the saved Claude tab?"
	}
	rows := []string{section.Render("Sessions saved"), q}
	if s.busy > 0 {
		rows = append(rows, warn.Render(fmt.Sprintf("%d still working: it stops now", s.busy)))
	}
	m.ask(prompt{rows: rows, onYes: func(m model) tea.Cmd { return closeCmd(m.q, len(m.runs) > 0) }})
}

func closeCmd(q *queue.Queue, keepOwn bool) tea.Cmd {
	return func() tea.Msg {
		msg, err := closeTabs(q, keepOwn)
		if err != nil {
			return statusMsg("close tabs: " + err.Error())
		}
		return statusMsg(msg)
	}
}

// armRestore asks about what a restore would open, defaulting to Yes. With nothing to open it only says so.
func (m *model) armRestore() {
	p, err := prepareRestore(m.q)
	switch {
	case err != nil:
		m.status = "restore sessions: " + err.Error()
	case len(p.Open) == 0:
		m.status = "nothing to restore: " + p.Summary()
	default:
		m.ask(prompt{rows: []string{section.Render("Restore sessions"), p.Summary()}, yes: true,
			onYes: func(m model) tea.Cmd { return restoreCmd(m.q) }})
	}
}

// restoreCmd checks again what is running, so a session started since the prompt opened isn't opened twice.
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
