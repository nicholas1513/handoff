package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/nicholas1513/handoff/internal/claudesessions"
	"github.com/nicholas1513/handoff/internal/queue"
)

// Saving (ctrl+s) and restoring (ctrl+l, confirmed in an overlay) the running Claude sessions. Tests replace these.
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

// askClose opens the close prompt. A save finishes whenever it does, so whatever was armed by then is dropped: its
// next key mustn't answer this prompt and then a run's.
func (m *model) askClose(s savedMsg) {
	m.status = s.status
	if s.tabs == 0 {
		return
	}
	m.closeAsk, m.closeYes = fmt.Sprintf("Close the %d saved Claude tabs?", s.tabs), false
	if s.tabs == 1 {
		m.closeAsk = "Close the saved Claude tab?"
	}
	m.closeBusy = s.busy
	m.armed, m.confirm, m.cancelArmed, m.cancelConfirm, m.quitArmed = false, false, false, false, false
}

// closeKey handles every key while the close prompt is open. ← → pick, and it defaults to No on the left.
func (m model) closeKey(k string) (tea.Model, tea.Cmd) {
	switch k {
	case "left":
		m.closeYes = false
	case "right":
		m.closeYes = true
	case " ", "enter":
		m.closeAsk = ""
		if m.closeYes {
			return m, closeCmd(m.q, len(m.runs) > 0)
		}
	}
	return m, nil
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

const (
	closeNoBtn  = "[ No ]"
	closeYesBtn = "[ Yes ]"
	closeGap    = 2
)

// closeRows are the prompt's rows above the decision row.
func (m model) closeRows() []string {
	rows := []string{section.Render("Sessions saved"), m.closeAsk}
	if m.closeBusy > 0 {
		rows = append(rows, warn.Render(fmt.Sprintf("%d still working: it stops now", m.closeBusy)))
	}
	return append(rows, "")
}

// closeOverlay is the close prompt as drawn, and where it sits (top-left cell) when centred in the window.
func (m model) closeOverlay() (lines []string, x, y int) {
	no, yes := closeNoBtn, closeYesBtn
	if m.closeYes {
		yes = sel.Render(yes)
	} else {
		no = sel.Render(no)
	}
	rows := m.closeRows()
	decision := lipgloss.PlaceHorizontal(lipgloss.Width(strings.Join(rows, "\n")), lipgloss.Center, no+strings.Repeat(" ", closeGap)+yes)
	lines = strings.Split(focused.Padding(0, 1).Render(strings.Join(append(rows, decision), "\n")), "\n")
	return lines, max(0, (m.w-lipgloss.Width(lines[0]))/2), max(0, (m.h-len(lines))/2)
}

// closeClick acts on the prompt's two options. Any other click is dropped: it must not land on what's behind.
func (m model) closeClick(x, y int) (tea.Model, tea.Cmd) {
	_, ox, oy := m.closeOverlay()
	rows := m.closeRows()
	if y != oy+1+len(rows) {
		return m, nil
	}
	w := lipgloss.Width(strings.Join(rows, "\n"))
	noX := ox + 2 + (w-len(closeNoBtn)-closeGap-len(closeYesBtn))/2 // border and padding, then centred
	yesX := noX + len(closeNoBtn) + closeGap
	switch {
	case x >= noX && x < noX+len(closeNoBtn):
		m.closeAsk = ""
	case x >= yesX && x < yesX+len(closeYesBtn):
		m.closeAsk = ""
		return m, closeCmd(m.q, len(m.runs) > 0)
	}
	return m, nil
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
