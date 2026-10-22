// Package tui is the review-and-run viewer: sessions | items | detail.
//
// Everything shown from queue/ was written by the agent, so it goes through sanitize first.
package tui

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/nicholas1513/handoff/internal/queue"
	"github.com/nicholas1513/handoff/internal/runner"
)

// Screen geometry, shared by View and the mouse hit-testing. Each pane has a 1-cell border and a
// title row, so list row i is at y = listTop+i and the button row is the detail pane's first line.
const (
	listTop = 2
	buttonY = 2
	runX    = 1  // "[ Run ]"
	rejectX = 10 // "[ Reject ]"
	cancelX = 12 // "Run this?  [ Cancel ]"
	yesX    = 24 // "[ Yes, run ]"
)

const (
	runBtn     = "[ Run ]  [ Reject ]"
	confirmBtn = "Run this?  [ Cancel ]  [ Yes, run ]"
	armedBtn   = "space again to run · any other key cancels"
)

// advanceGuard: after an auto-advance, space/r are ignored so a burst of taps can't run the new item unseen.
const advanceGuard = time.Second

func columns(w int) (sessions, items, detail int) {
	sessions, items = max(22, w/5), max(28, w*3/10)
	return sessions, items, max(20, w-sessions-items)
}

type sessionRow struct {
	id      string
	sess    *queue.Session // nil: no hook record, so unverified
	items   []queue.Item   // newest first
	invalid []string       // only on the "invalid" pseudo-session
}

type tickMsg time.Time

type runDone struct {
	id  string
	err error
}

type model struct {
	q        *queue.Queue
	home     string
	rows     []sessionRow
	si, ii   int
	focus    int // 0 sessions, 1 items, 2 detail
	reviewed *queue.Reviewed
	openErr  error
	changed  bool // live item differs from the reviewed snapshot
	confirm  bool // mouse: Run clicked, waiting for Yes
	armed    bool // keyboard: one space pressed, waiting for the second
	running  bool
	scroll   int
	status   string
	w, h     int
	titles   titleCache

	now        func() time.Time
	advancedAt time.Time
}

func Run(q *queue.Queue, home string) error {
	_, err := tea.NewProgram(newModel(q, home), tea.WithAltScreen(), tea.WithMouseCellMotion()).Run()
	return err
}

func newModel(q *queue.Queue, home string) model {
	m := model{q: q, home: home, w: 120, h: 40, titles: titleCache{}, now: time.Now}
	m.refresh()
	return m
}

func (m model) Init() tea.Cmd { return tick() }

func tick() tea.Cmd {
	return tea.Tick(2*time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m *model) session() *sessionRow {
	if m.si < len(m.rows) {
		return &m.rows[m.si]
	}
	return nil
}

func (m *model) item() *queue.Item {
	if s := m.session(); s != nil && m.ii < len(s.items) {
		return &s.items[m.ii]
	}
	return nil
}

// refresh reloads the queue, keeping the selection on the same session and item ids.
func (m *model) refresh() {
	var keepS, keepI string
	had := false // the kept session had a pending item before this reload
	if s := m.session(); s != nil {
		keepS = s.id
		_, had = m.oldestPending(s)
	}
	if it := m.item(); it != nil {
		keepI = it.ID
	}
	items, invalid, err := m.q.List()
	if err != nil {
		m.status = err.Error()
	}
	byID := map[string]*sessionRow{}
	var rows []*sessionRow
	for i := len(items) - 1; i >= 0; i-- { // newest first
		it := items[i]
		r := byID[it.SessionID]
		if r == nil {
			r = &sessionRow{id: it.SessionID}
			if s, err := m.q.ReadSession(it.SessionID); err == nil {
				r.sess = &s
			}
			byID[it.SessionID] = r
			rows = append(rows, r)
		}
		r.items = append(r.items, it)
	}
	m.rows = m.rows[:0]
	for _, r := range rows {
		m.rows = append(m.rows, *r)
	}
	if len(invalid) > 0 {
		sort.Sort(sort.Reverse(sort.StringSlice(invalid)))
		m.rows = append(m.rows, sessionRow{id: "invalid", invalid: invalid})
	}
	m.si, m.ii = 0, 0
	if keepS == "" {
		m.enterSession(0)
	}
	for i, r := range m.rows {
		if r.id == keepS {
			m.si = i
			for j, it := range r.items {
				if it.ID == keepI {
					m.ii = j
				}
			}
		}
	}
	if keepI != "" && (m.item() == nil || m.item().ID != keepI) {
		m.advancedAt = m.now() // the selection moved without the user: guard space like an advance
	}
	// a new item arriving (none was pending before) after the selection finished pulls the selection onto it (same session only)
	if it := m.item(); it != nil && !had && !m.running {
		if st := m.q.State(*it, time.Now()); st != queue.Pending && st != queue.Running {
			if best, ok := m.oldestPending(m.session()); ok {
				m.ii, m.advancedAt, m.reviewed = best, m.now(), nil
			}
		}
	}
	m.open()
}

// enterSession selects the row's oldest pending item (guarding space like an advance), else the newest.
func (m *model) enterSession(si int) {
	m.si, m.ii = si, 0
	if s := m.session(); s != nil {
		if best, ok := m.oldestPending(s); ok {
			m.ii, m.advancedAt = best, m.now()
		}
	}
}

// open snapshots the selected item, unless it is already the one under review.
func (m *model) open() {
	it := m.item()
	if it == nil {
		m.reviewed, m.openErr, m.confirm, m.armed = nil, nil, false, false
		return
	}
	if m.reviewed != nil && m.reviewed.Item.ID == it.ID {
		m.changed = m.q.State(*it, time.Now()) == queue.Pending && m.q.Verify(*m.reviewed) != nil
		return
	}
	r, err := m.q.Snapshot(it.ID)
	m.reviewed, m.openErr, m.changed, m.confirm, m.armed, m.scroll = &r, err, false, false, false, 0
	if err != nil {
		m.reviewed = nil
	}
}

func (m *model) move(d int) {
	switch m.focus {
	case 0:
		m.enterSession(clamp(m.si+d, len(m.rows)))
	case 1:
		if s := m.session(); s != nil {
			m.ii = clamp(m.ii+d, max(len(s.items), len(s.invalid)))
		}
	case 2:
		m.scroll = max(0, m.scroll+d)
	}
	m.open()
}

// itemRow dims finished items (any state but pending/running).
func itemRow(it queue.Item, st string) string {
	row := glyph(st) + " " + oneLine(it.Description)
	if st != queue.Pending && st != queue.Running {
		return dim.Render(row)
	}
	return row
}

func clamp(i, n int) int { return max(0, min(i, n-1)) }

func (m model) pending() bool {
	return m.reviewed != nil && !m.running && m.q.State(m.reviewed.Item, time.Now()) == queue.Pending
}

func (m *model) startRun() tea.Cmd {
	if !m.pending() {
		return nil
	}
	if m.changed {
		m.status = "item changed after you opened it: press o to review it again"
		m.confirm, m.armed = false, false
		return nil
	}
	m.confirm, m.armed, m.running, m.status = false, false, true, "running…"
	q, r := m.q, *m.reviewed
	return func() tea.Msg {
		_, err := runner.Run(context.Background(), q, r, time.Now())
		return runDone{id: r.Item.ID, err: err}
	}
}

func (m *model) reject() {
	if !m.pending() {
		return
	}
	it := m.reviewed.Item
	err := m.q.Claim(it) // so a run can't start after the reject
	if err == nil {
		err = m.q.WriteResult(it, queue.Result{State: queue.Rejected, ExitCode: -1, RanAt: time.Now(), SHA256Reviewed: m.reviewed.Hash})
	}
	m.confirm, m.armed, m.status = false, false, "rejected"
	if err != nil {
		m.status = "reject: " + err.Error()
		return
	}
	m.advance(it.ID)
}

// advance selects the session's oldest pending item, but only if the selection is still on fromID.
func (m *model) advance(fromID string) {
	s, cur := m.session(), m.item()
	if s == nil || cur == nil || cur.ID != fromID {
		return
	}
	if best, ok := m.oldestPending(s); ok {
		m.ii, m.advancedAt, m.reviewed = best, m.now(), nil
		m.open()
	}
}

// oldestPending is the index of the row's pending item with the earliest Created (first in list order on a tie).
func (m *model) oldestPending(s *sessionRow) (int, bool) {
	best := -1
	for i, it := range s.items {
		if m.q.State(it, time.Now()) == queue.Pending && (best < 0 || it.Created.Before(s.items[best].Created)) {
			best = i
		}
	}
	return best, best >= 0
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
	case tickMsg:
		m.refresh()
		return m, tick()
	case runDone:
		m.running, m.status = false, "finished"
		if msg.err != nil {
			m.status = "run: " + msg.err.Error()
		}
		m.refresh()
		m.advance(msg.id)
	case tea.KeyMsg:
		return m.key(msg.String())
	case tea.MouseMsg:
		return m.mouse(msg)
	}
	return m, nil
}

// arm is the only way a key starts a run; holds the advanceGuard check.
func (m *model) arm() { m.armed = m.pending() && m.now().Sub(m.advancedAt) >= advanceGuard }

func (m model) key(k string) (tea.Model, tea.Cmd) {
	if m.running && k != "q" && k != "ctrl+c" {
		return m, nil
	}
	if m.armed {
		m.armed = false // also when startRun refuses, so no invisible arm is left behind
		if k == " " {
			return m, m.startRun()
		} // the cancelling key is not acted on
		return m, nil
	}
	if m.confirm { // mouse confirm: any key cancels, space included
		m.confirm = false
		return m, nil
	}
	switch k {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "tab", "right", "l":
		m.focus = min(2, m.focus+1)
	case "shift+tab", "left", "h":
		m.focus = max(0, m.focus-1)
	case "down", "j":
		m.move(1)
	case "up", "k":
		m.move(-1)
	case " ", "r":
		m.arm()
	case "x":
		m.reject()
	case "o":
		m.reviewed = nil
		m.open()
	}
	return m, nil
}

func (m model) mouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	sw, iw, _ := columns(m.w)
	pane := 2
	if msg.X < sw {
		pane = 0
	} else if msg.X < sw+iw {
		pane = 1
	}
	if m.armed && msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft {
		m.armed = false // a click only cancels the arm; it is not acted on
		return m, nil
	}
	switch {
	case msg.Button == tea.MouseButtonWheelUp || msg.Button == tea.MouseButtonWheelDown:
		m.focus = pane
		if msg.Button == tea.MouseButtonWheelUp {
			m.move(-1)
		} else {
			m.move(1)
		}
	case msg.Action != tea.MouseActionPress || msg.Button != tea.MouseButtonLeft:
	case pane == 2 && msg.Y == buttonY:
		x := msg.X - sw - iw
		switch {
		case m.confirm && x >= cancelX && x < cancelX+10:
			m.confirm = false
		case m.confirm && x >= yesX && x < yesX+12:
			return m, m.startRun()
		case !m.confirm && x >= runX && x < runX+7:
			m.confirm = m.pending()
		case !m.confirm && x >= rejectX && x < rejectX+10:
			m.reject()
		}
		m.focus = 2
	case pane < 2 && msg.Y >= listTop:
		m.focus = pane
		row := msg.Y - listTop
		if pane == 0 && row < len(m.rows) {
			m.enterSession(row)
		} else if s := m.session(); pane == 1 && s != nil && row < max(len(s.items), len(s.invalid)) {
			m.ii = row
		}
		m.open()
	default:
		m.focus = pane
	}
	return m, nil
}

var (
	border  = lipgloss.NewStyle().Border(lipgloss.RoundedBorder())
	focused = border.BorderForeground(lipgloss.Color("6"))
	sel     = lipgloss.NewStyle().Reverse(true)
	warn    = lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Bold(true)
	ok      = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	dim     = lipgloss.NewStyle().Faint(true)
	bold    = lipgloss.NewStyle().Bold(true)
)

func (m model) View() string {
	sw, iw, dw := columns(m.w)
	h := max(5, m.h-3) // borders + status line

	var srows []string
	now := time.Now()
	for _, r := range m.rows {
		title, pending := "", 0
		if r.sess != nil {
			title = m.titles.get(r.sess.TranscriptPath)
		}
		for _, it := range r.items {
			if m.q.State(it, now) == queue.Pending {
				pending++
			}
		}
		srows = append(srows, sessionLabel(r, title, pending, now))
	}
	var irows []string
	if s := m.session(); s != nil {
		for _, it := range s.items {
			irows = append(irows, itemRow(it, m.q.State(it, time.Now())))
		}
		for _, id := range s.invalid {
			irows = append(irows, "⚠ "+id)
		}
	}

	header := ""
	if m.pending() {
		header = runBtn
		if m.confirm {
			header = warn.Render(confirmBtn)
		} else if m.armed {
			header = warn.Render(armedBtn)
		}
	}
	body := strings.Split(lipgloss.NewStyle().Width(dw-2).Render(m.detail()), "\n")
	body = body[min(m.scroll, max(0, len(body)-1)):]

	status := dim.Render("↑↓ move · tab/click pane · space space run · x reject · o re-open · q quit")
	if m.status != "" {
		status = m.status + dim.Render("  ·  q quit")
	}
	return lipgloss.JoinVertical(lipgloss.Left,
		lipgloss.JoinHorizontal(lipgloss.Top,
			m.pane(0, sw, h, "Sessions", srows, m.si),
			m.pane(1, iw, h, "Items", irows, m.ii),
			m.pane(2, dw, h, "Detail", append([]string{header}, body...), -1),
		),
		status,
	)
}

func (m model) pane(i, w, h int, title string, rows []string, selected int) string {
	st := border
	if m.focus == i {
		st = focused
	}
	lines := []string{bold.Render(title)}
	for j, r := range rows {
		if len(lines) == h {
			break
		}
		r = lipgloss.NewStyle().MaxWidth(w - 2).Render(r)
		if j == selected {
			r = sel.Render(r)
		}
		lines = append(lines, r)
	}
	return st.Width(w - 2).Height(h).MaxHeight(h + 2).Render(strings.Join(lines, "\n"))
}

func sessionLabel(r sessionRow, title string, pending int, now time.Time) string {
	if r.invalid != nil {
		return warn.Render(fmt.Sprintf("⚠ invalid items (%d)", len(r.invalid)))
	}
	short := r.id[:min(8, len(r.id))]
	if r.sess == nil {
		return warn.Render("⚠ "+short) + fmt.Sprintf(" (%d)", len(r.items))
	}
	if title == "" {
		title = short
	}
	parts := []string{title, filepath.Base(r.sess.Cwd)}
	if len(r.items) > 0 {
		parts = append(parts, age(now.Sub(r.items[0].Created)))
	}
	label := strings.Join(parts, " · ")
	if pending > 0 {
		label = fmt.Sprintf("● %d %s", pending, label)
	}
	return oneLine(label)
}

func age(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

func glyph(state string) string {
	return map[string]string{
		queue.Pending: "●", queue.Running: "…", queue.Succeeded: "✓", queue.Failed: "✗",
		queue.Rejected: "⊘", queue.Expired: "⌛", queue.Modified: "!",
	}[state]
}

func oneLine(s string) string { return strings.ReplaceAll(sanitize(s), "\n", " ⏎ ") }

// sanitize escapes control and invisible formatting characters (ANSI escapes, carriage returns,
// bidi overrides, zero-width spaces) so agent-written text can't fake or hide what's displayed.
func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0):
			fmt.Fprintf(&b, `\x%02x`, r)
		case unicode.Is(unicode.Cf, r):
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
