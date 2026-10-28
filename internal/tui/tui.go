// Package tui is the review-and-run viewer: sessions | items | detail.
//
// Everything shown from queue/ was written by the agent, so it goes through sanitize first.
package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

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

	cancelRunX = 1  // "[ Cancel run ]"
	noX        = 18 // "Cancel this run?  [ No ]"
	yesCancelX = 26 // "[ Yes, cancel ]"
)

const (
	runBtn     = "[ Run ]  [ Reject ]"
	confirmBtn = "Run this?  [ Cancel ]  [ Yes, run ]"
	armedBtn   = "space again to run · any other key cancels"

	cancelRunBtn     = "[ Cancel run ]"
	cancelConfirmBtn = "Cancel this run?  [ No ]  [ Yes, cancel ]"
	cancelArmedBtn   = "c again to cancel this run · any other key keeps it running"
)

// advanceGuard: after an auto-advance, space/r are ignored so a burst of taps can't run the new item unseen.
const advanceGuard = time.Second

func columns(w int) (sessions, items, detail int) {
	sessions, items = max(22, w/5), max(28, w*3/10)
	return sessions, items, max(20, w-sessions-items)
}

type sessionRow struct {
	id      string
	sess    *queue.Session       // nil: no hook record, so unverified
	items   []queue.Item         // display order: outstanding oldest first, then completed newest-finished first
	invalid []string             // only on the "invalid" pseudo-session
	nOut    int                  // items[:nOut] are outstanding; fixed at refresh so headers don't move between paint and click
	fin     map[string]time.Time // finish time per completed item, snapshot at refresh
	state   map[string]string    // state per item, snapshot at refresh: drawing reads no result files per frame
	title   string               // the transcript's /resume title, read at refresh
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
	// the run this viewer started; cancelRun stops it (nil when nothing runs here)
	runningID     string
	cancelRun     context.CancelFunc
	cancelArmed   bool // keyboard: one c pressed on a running item, waiting for the second
	cancelConfirm bool // mouse: Cancel run clicked, waiting for Yes
	scroll   int
	itemTop  int            // first items-pane row drawn
	boxTop   map[string]int // first line drawn per box id ("result", "prompt", "file:<name>"); copied on write
	boxSel   string         // selected box; "" or an unframed id means the first framed box
	diff     fileDiff       // the reviewed edit item's diff, parsed at open
	diffExp  map[int][2]int // context revealed per gap between hunks; copied on write
	boxX     map[string]int // pan offset per unwrapped box id; copied on write
	settings viewerSettings
	menu     bool // the settings overlay is open
	menuSel  int  // its selected row in settingRows
	status   string
	w, h     int
	titles   titleCache

	data    *detailData // what the detail shows from disk and the clock; see loadData
	rowsGen int         // bumped whenever m.rows or any sessionRow's items/state/fin/nOut/title change
	rc      *renderCache
	pc      *paneCache

	now        func() time.Time
	advancedAt time.Time
}

// gens numbers row snapshots, unique across copies of the model so a shared cache can't mistake one for another.
var gens atomic.Int64

func nextGen() int { return int(gens.Add(1)) }

func Run(q *queue.Queue, home string) error {
	_, err := tea.NewProgram(newModel(q, home), tea.WithAltScreen(), tea.WithMouseCellMotion()).Run()
	return err
}

func newModel(q *queue.Queue, home string) model {
	m := model{q: q, home: home, w: 120, h: 40, titles: titleCache{}, now: time.Now, rc: newRenderCache(), pc: newPaneCache()}
	var err error
	if m.settings, err = loadSettings(q.Root); err != nil {
		m.status = "settings: " + err.Error()
	}
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
				r.sess, r.title = &s, m.titles.get(s.TranscriptPath)
			}
			byID[it.SessionID] = r
			rows = append(rows, r)
		}
		r.items = append(r.items, it)
	}
	fresh := []sessionRow{} // a new array: copies of the model share the old one
	for _, r := range rows {
		r.nOut, r.fin, r.state = m.sortItems(r.items)
		fresh = append(fresh, *r)
	}
	if len(invalid) > 0 {
		sort.Sort(sort.Reverse(sort.StringSlice(invalid)))
		fresh = append(fresh, sessionRow{id: "invalid", invalid: invalid})
	}
	if !reflect.DeepEqual(fresh, m.rows) {
		m.rows, m.rowsGen = fresh, nextGen()
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

// open snapshots the selected item, unless it is already the one under review, and re-reads the detail's data.
func (m *model) open() {
	defer m.loadData()
	it := m.item()
	if it == nil {
		m.reviewed, m.openErr, m.confirm, m.armed = nil, nil, false, false
		return
	}
	if m.reviewed != nil && m.reviewed.Item.ID == it.ID {
		m.changed = m.q.State(*it, time.Now()) == queue.Pending && m.q.Verify(*m.reviewed) != nil
		m.clampScroll()
		return
	}
	r, err := m.q.Snapshot(it.ID)
	m.reviewed, m.openErr, m.changed, m.confirm, m.armed, m.scroll = &r, err, false, false, false, 0
	m.boxTop, m.boxSel, m.diff, m.diffExp, m.boxX = nil, "", fileDiff{}, nil, nil
	if err != nil {
		m.reviewed = nil
	} else if r.Item.Edit != "" {
		base, _ := os.ReadFile(filepath.Join(r.SnapDir, queue.EditBase)) // unreadable: the diff is shown unparsed
		m.diff = parseDiff(r.Diff, string(base))
	}
}

// expandDiff applies one [+] row, or every one when a is nil.
func (m *model) expandDiff(a *expand) {
	if a == nil {
		m.diffExp = m.diff.expandAll(m.diffExp)
	} else {
		m.diffExp = applyExpand(m.diffExp, *a)
	}
}

// panBox moves an unwrapped box sideways by d cells, kept within its widest line.
func (m *model) panBox(id string, d int) {
	b, ok := m.box(id)
	if !ok || !b.pan {
		return
	}
	m.boxX = with(m.boxX, id, max(0, b.x+d))
	b, _ = m.box(id)
	m.boxX = with(m.boxX, id, b.x) // as clamped, so panning back responds at once
}

// panTarget is the box < > pan: the selected box if it pans, else the first that does, else "".
func (m model) panTarget() string { return panIn(m.layout()) }

func panIn(l layout) string {
	for _, b := range l.boxes {
		if b.id == l.sel && b.pan {
			return l.sel
		}
	}
	for _, b := range l.boxes {
		if b.pan {
			return b.id
		}
	}
	return ""
}

// toggleSetting flips settingRows[i] and saves.
func (m *model) toggleSetting(i int) {
	p := settingRows[i].on(&m.settings)
	*p = !*p
	m.boxX = nil
	if err := saveSettings(m.q.Root, m.settings); err != nil {
		m.status = "settings: " + err.Error()
	}
	m.clampScroll()
}

func (m *model) move(d int) {
	si, ii := m.si, m.ii
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
	if m.si != si || m.ii != ii { // scrolling, or a move pinned at either end, has nothing new to open
		m.open()
	}
	m.clampScroll()
}

func outstanding(st string) bool { return st == queue.Pending || st == queue.Running }

// finish is when a completed item ended: the result's RanAt, else when it was revoked, else (expired) its Expires.
func (m *model) finish(it queue.Item) time.Time {
	if r, err := m.q.ReadResult(it); err == nil {
		return r.RanAt
	}
	if at, ok := m.q.RevokedAt(it); ok {
		return at
	}
	return it.Expires
}

// sortItems: outstanding by Created ascending, then completed by finish time descending; ties by ID.
// Returns the outstanding count, the completed items' finish times and every item's state.
func (m *model) sortItems(items []queue.Item) (int, map[string]time.Time, map[string]string) {
	now := m.now()
	out := map[string]bool{}
	at := map[string]time.Time{}
	st := map[string]string{}
	for _, it := range items {
		st[it.ID] = m.q.State(it, now)
		if out[it.ID] = outstanding(st[it.ID]); out[it.ID] {
			at[it.ID] = it.Created
		} else {
			at[it.ID] = m.finish(it)
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		switch {
		case out[a.ID] != out[b.ID]:
			return out[a.ID]
		case !at[a.ID].Equal(at[b.ID]):
			return at[a.ID].Before(at[b.ID]) == out[a.ID]
		}
		return a.ID < b.ID
	})
	n := 0
	for _, it := range items {
		if out[it.ID] {
			n++
		}
	}
	return n, at, st
}

// stamp is local time: 15:04 for today, else Jan 2 15:04.
func stamp(t, now time.Time) string {
	t, now = t.Local(), now.Local()
	if t.Year() == now.Year() && t.YearDay() == now.YearDay() {
		return t.Format("15:04")
	}
	return t.Format("Jan 2 15:04")
}

// itemRow dims finished items (any state but pending/running).
func itemRow(it queue.Item, st, ts string) string {
	g := lipgloss.NewStyle().Foreground(glyphColor[st])
	if outstanding(st) {
		return ts + "  " + g.Render(glyph(st)) + " " + oneLine(it.Description)
	}
	// rendered piecewise: one dim.Render around a colored glyph would end the faint at the glyph's reset
	return dim.Render(ts+"  ") + g.Faint(true).Render(glyph(st)) + dim.Render(" "+oneLine(it.Description))
}

var glyphColor = map[string]lipgloss.Color{
	queue.Pending: "3", queue.Running: "3", queue.Succeeded: "2", queue.Failed: "1",
	queue.Rejected: "1", queue.Modified: "1", queue.Expired: "8", queue.Canceled: "1", queue.Revoked: "8",
}

// itemPane is the items pane's rows plus, per row, the index of the item it shows (-1: a header).
// For the invalid pseudo-session the index is into s.invalid. View and the mouse both use this.
func (m *model) itemPane(s *sessionRow, now time.Time) (rows []string, idx []int) {
	add := func(r string, i int) { rows, idx = append(rows, r), append(idx, i) }
	n := s.nOut
	for i, it := range s.items {
		if i == 0 && n > 0 {
			add(section.Render(fmt.Sprintf("Outstanding (%d)", n)), -1)
		}
		if i == n {
			add(section.Render(fmt.Sprintf("Completed (%d)", len(s.items)-n)), -1)
		}
		ts := it.Created
		if i >= n {
			ts = s.fin[it.ID]
		}
		add(itemRow(it, m.shownState(s, it), stamp(ts, now)), i)
	}
	for i, id := range s.invalid {
		add("⚠ "+id, i)
	}
	return rows, idx
}

// itemWindow is the first row drawn of n rows in height lines, given the previous top, so the
// selected row sel is drawn and not under a "↑ N more" / "↓ N more" line (which take a line each).
func itemWindow(n, sel, height, top int) int {
	if n <= height {
		return 0
	}
	top = max(0, min(top, n-height))
	if sel < 0 {
		return top
	}
	if top > 0 && sel < top+1 {
		top = max(0, sel-1)
	}
	if top+height < n && sel > top+height-2 {
		top = min(n-height, sel-height+2)
	}
	return top
}

// itemList is a session's items-pane rows and the item index each shows, as itemPane builds them.
type itemList struct {
	rows []string
	idx  []int
}

// itemList is itemPane, cached while the rows, the selected session, this viewer's run, the day the stamps are
// relative to and the styling are unchanged. Callers must not modify it.
func (m *model) itemList(s *sessionRow, now time.Time) *itemList {
	run, n := "", now.Local()
	if m.running {
		run = m.runningID
	}
	return m.pc.list.get(listKey{m.rowsGen, m.si, run, n.Year()*1000 + n.YearDay(), lookNow()}, func() *itemList {
		rows, idx := m.itemPane(s, now)
		return &itemList{rows, idx}
	})
}

// itemWin is the items pane's rows, the first one drawn and the selected one's index (-1: none). It keeps itemTop in
// step with the selection, so View and the mouse agree.
func (m *model) itemWin(s *sessionRow, now time.Time) (l *itemList, top, row int) {
	l, row = m.itemList(s, now), -1
	for j, i := range l.idx {
		if i == m.ii {
			row = j
		}
	}
	m.itemTop = itemWindow(len(l.rows), row, max(5, m.h-3)-1, m.itemTop)
	return l, m.itemTop, row
}

// itemView is the items pane as drawn: the pane rows from the scroll offset with indicator lines,
// and the item index each drawn line selects (-1: header or indicator). View and the mouse both use it.
// They may share itemList's arrays, so they are read-only.
func (m *model) itemView(s *sessionRow, now time.Time) (rows []string, idx []int, sel int) {
	l, top, row := m.itemWin(s, now)
	all, ai := l.rows, l.idx
	height := max(5, m.h-3) - 1
	end := min(len(all), top+height)
	rows, idx, sel = all[top:end], ai[top:end], row-top
	if len(all) > height {
		rows, idx = append([]string{}, rows...), append([]int{}, idx...)
		if top > 0 {
			rows[0], idx[0] = dim.Render(fmt.Sprintf("↑ %d more", top+1)), -1
		}
		if end < len(all) {
			rows[len(rows)-1], idx[len(idx)-1] = dim.Render(fmt.Sprintf("↓ %d more", len(all)-end+1)), -1
		}
	}
	return rows, idx, sel
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
	ctx, cancel := context.WithCancel(context.Background())
	m.runningID, m.cancelRun = r.Item.ID, cancel
	m.restate(r.Item.ID)
	m.loadData()
	return func() tea.Msg {
		_, err := runner.Run(ctx, q, r, time.Now())
		return runDone{id: r.Item.ID, err: err}
	}
}

// cancelable: the selected item is this viewer's run in progress, or a run left behind by a viewer that went away.
func (m model) cancelable() bool {
	it := m.item()
	if it == nil {
		return false
	}
	if m.running {
		return it.ID == m.runningID
	}
	return m.q.State(*it, time.Now()) == queue.Running
}

// drawnState is the selected item's state as drawn: this viewer's run in progress, else the detail's snapshot.
func (m model) drawnState() string {
	if it := m.item(); it != nil && m.running && it.ID == m.runningID {
		return queue.Running
	}
	return m.data.state
}

// shownPending and shownCancelable are pending() and cancelable() as drawn: from snapshots, so drawing reads no files.
func (m model) shownPending() bool {
	return m.reviewed != nil && !m.running && m.drawnState() == queue.Pending
}

func (m model) shownCancelable() bool {
	it := m.item()
	if it == nil {
		return false
	}
	if m.running {
		return it.ID == m.runningID
	}
	return m.data.state == queue.Running
}

// cancel stops the selected run: this viewer's own via its context, a stale one via runner.CancelStale.
func (m *model) cancel() {
	m.cancelArmed, m.cancelConfirm = false, false
	if !m.cancelable() {
		return
	}
	if m.running {
		m.cancelRun()
		m.status = "canceling…"
		return
	}
	if err := runner.CancelStale(m.q, *m.item(), time.Now()); err != nil {
		m.status = "cancel: " + err.Error()
		return
	}
	m.status = "canceled"
	m.refresh()
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
	m.restate(it.ID) // also on an error: the claim may have landed
	m.loadData()
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

// restate re-reads one item's state after this viewer changed it (reject, cancel, a run starting or ending),
// so the lists and the detail show it at once instead of at the next refresh.
func (m *model) restate(id string) {
	m.rows = slices.Clone(m.rows) // copy on write, like the maps: other copies of the model share the array
	for i := range m.rows {
		r := &m.rows[i]
		for _, it := range r.items {
			if it.ID == id {
				r.state = with(r.state, id, m.q.State(it, m.now()))
				m.rowsGen = nextGen()
			}
		}
	}
}

// shownState is an item's state as drawn: this viewer's run in progress, else the refresh snapshot.
func (m model) shownState(s *sessionRow, it queue.Item) string {
	if m.running && it.ID == m.runningID {
		return queue.Running
	}
	return s.state[it.ID]
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	nm, cmd := m.update(msg)
	next := nm.(model)
	if s := next.session(); s != nil {
		next.itemWin(s, next.now())
	}
	return next, cmd
}

func (m model) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.clampScroll()
	case tickMsg:
		m.refresh()
		return m, tick()
	case runDone:
		if m.cancelRun != nil {
			m.cancelRun() // releases the context
		}
		m.running, m.runningID, m.cancelRun, m.cancelArmed, m.cancelConfirm, m.status = false, "", nil, false, false, "finished"
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
	if m.menu { // the overlay takes every key while open
		switch k {
		case " ", "enter":
			m.toggleSetting(m.menuSel)
		case "up", "k":
			m.menuSel = max(0, m.menuSel-1)
		case "down", "j":
			m.menuSel = min(len(settingRows)-1, m.menuSel+1)
		case "esc", "s", "q":
			m.menu = false
		}
		return m, nil
	}
	if m.cancelArmed {
		m.cancelArmed = false
		if k == "c" {
			m.cancel()
		} // any other key only disarms and is not acted on
		return m, nil
	}
	if m.cancelConfirm { // mouse confirm: any key keeps the run
		m.cancelConfirm = false
		return m, nil
	}
	if m.running && k != "q" && k != "ctrl+c" && k != "c" && k != "s" {
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
	case "c":
		m.cancelArmed = m.cancelable()
	case "[", "pgup", "]", "pgdown":
		if b, ok := m.box(m.selBox()); m.focus == 2 && ok {
			d := b.cap
			if k == "[" || k == "pgup" {
				d = -d
			}
			m.scrollBox(b.id, d)
			m.reveal(b.id)
		}
	case "{", "}":
		if m.focus == 2 {
			m.nextBox(map[string]int{"{": -1, "}": 1}[k])
			m.reveal(m.selBox())
		}
	case "o":
		m.reviewed = nil
		m.open()
	case "s":
		m.menu = true
	case "+":
		if _, ok := m.box(diffBox); ok {
			m.expandDiff(nil)
			m.clampScroll()
		}
	case "<", ">":
		_, _, dw := columns(m.w)
		m.panBox(m.panTarget(), map[string]int{"<": -1, ">": 1}[k]*max(1, (dw-6)/2))
	}
	return m, nil
}

// detailHeader is the button row: Run/Reject, confirm/armed, cancel-run variants, as drawn.
func (m model) detailHeader() string {
	text, alert := "", false
	switch {
	case m.shownPending() && m.confirm:
		text, alert = confirmBtn, true
	case m.shownPending() && m.armed:
		text, alert = armedBtn, true
	case m.shownPending():
		text = runBtn
	case m.shownCancelable() && m.cancelConfirm:
		text, alert = cancelConfirmBtn, true
	case m.shownCancelable() && m.cancelArmed:
		text, alert = cancelArmedBtn, true
	case m.shownCancelable():
		text = cancelRunBtn
	}
	if !alert {
		return text
	}
	return m.rc.hdr.get(styleKey{text: text, look: lookNow()}, func() string { return warn.Render(text) })
}

const settingsKey = "s settings"

// footer is the status line, and the column its settings hint starts at, for the mouse.
func (m model) footer() (string, int) { return m.footerOf(m.layout()) }

func (m model) footerOf(l layout) (string, int) {
	keys := []string{"↑↓ move", "tab/click pane"}
	if m.focus > 0 && m.shownPending() {
		keys = append(keys, "space space run", "x reject")
	}
	if m.focus > 0 && m.shownCancelable() {
		keys = append(keys, "c c cancel run")
	}
	if m.focus == 2 && len(l.boxes) > 0 {
		keys = append(keys, "[ ] scroll · { } next box")
	}
	if _, ok := l.box(diffBox); ok {
		keys = append(keys, "+ more context")
	}
	if panIn(l) != "" {
		keys = append(keys, "< > pan")
	}
	if m.changed {
		keys = append(keys, "o re-open")
	}
	plain := strings.Join(append(keys, settingsKey, "q quit"), " · ")
	f := m.rc.foot.get(styleKey{plain, m.status, lookNow()}, func() footLine {
		status, x := dim.Render(plain), ansi.StringWidth(plain[:strings.Index(plain, settingsKey)])
		if m.status != "" {
			status = m.status + dim.Render("  ·  ") + status
			x += ansi.StringWidth(m.status + "  ·  ")
		}
		return footLine{status, x}
	})
	return f.text, f.x
}

// footLine is the status line as drawn and the column its settings hint starts at.
type footLine struct {
	text string
	x    int
}

// statusY is the footer's screen row: below the panes, which are max(5, h-3) rows plus their borders.
func (m model) statusY() int { return max(5, m.h-3) + 2 }

func (m model) mouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	sw, iw, _ := columns(m.w)
	pane := 2
	if msg.X < sw {
		pane = 0
	} else if msg.X < sw+iw {
		pane = 1
	}
	press := msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft
	if m.menu { // the overlay takes every click while open
		if press {
			ol, ox, oy := m.overlay()
			inside := msg.X >= ox && msg.X < ox+lipgloss.Width(ol[0]) && msg.Y >= oy && msg.Y < oy+len(ol)
			if i := msg.Y - oy - settingsRow; inside && i >= 0 && i < len(settingRows) {
				m.menuSel = i
				m.toggleSetting(i)
			} else if !inside {
				m.menu = false
			}
		}
		return m, nil
	}
	if (m.armed || m.cancelArmed) && press {
		m.armed, m.cancelArmed = false, false // a click only cancels the arm; it is not acted on
		return m, nil
	}
	switch {
	case msg.Button == tea.MouseButtonWheelUp || msg.Button == tea.MouseButtonWheelDown:
		m.focus = pane
		d := 1
		if msg.Button == tea.MouseButtonWheelUp {
			d = -1
		}
		if id, _, ok := m.boxAt(msg.Y); pane == 2 && ok {
			m.scrollBox(id, 3*d)
			m.boxSel = id
		} else {
			m.move(d)
		}
	case msg.Button == tea.MouseButtonWheelLeft || msg.Button == tea.MouseButtonWheelRight:
		if id, _, ok := m.boxAt(msg.Y); pane == 2 && ok {
			m.panBox(id, map[bool]int{true: -4, false: 4}[msg.Button == tea.MouseButtonWheelLeft])
		}
	case !press:
	case msg.Y == m.statusY():
		if _, x := m.footer(); msg.X >= x && msg.X < x+len(settingsKey) {
			m.menu = true
		}
	case pane == 2 && msg.Y == buttonY && m.cancelable():
		x := msg.X - sw - iw
		switch {
		case m.cancelConfirm && x >= noX && x < noX+6:
			m.cancelConfirm = false
		case m.cancelConfirm && x >= yesCancelX && x < yesCancelX+15:
			m.cancel()
		case !m.cancelConfirm && x >= cancelRunX && x < cancelRunX+14:
			m.cancelConfirm = true
		}
		m.focus = 2
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
		row := msg.Y - listTop
		vis := row < max(5, m.h-3)-1 // only drawn rows are clickable
		item := -1
		if s := m.session(); pane == 1 && s != nil && vis {
			if _, idx, _ := m.itemView(s, m.now()); row < len(idx) {
				if item = idx[row]; item < 0 {
					return m, nil // a header: fully inert
				}
			}
		}
		m.focus = pane
		if pane == 0 && vis && row < len(m.rows) {
			m.enterSession(row)
		} else if item >= 0 {
			m.ii = item
		}
		m.open()
	default:
		m.focus = pane
		if id, line, ok := m.boxAt(msg.Y); pane == 2 && ok && id == diffBox {
			m.boxSel = id
			if b, _ := m.box(diffBox); line < len(b.meta) && b.meta[line].act != nil {
				m.expandDiff(b.meta[line].act)
				m.clampScroll()
			}
		}
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
	section = lipgloss.NewStyle().Foreground(lipgloss.Color("#7aa2f7")).Bold(true) // items-pane headers and detail headings
)

func (m model) View() string {
	sw, iw, dw := columns(m.w)
	h := max(5, m.h-3) // borders + status line
	sp, ip := m.sessionsPane(sw, h), m.itemsPane(iw, h)
	f := m.pc.detail.get(frameKey{dw, m.focus == 2, lookNow()}, func() paneFrame { return paneFrameOf("Detail", dw, m.focus == 2) })
	body := append([]string{f.title, pad(ansi.Truncate(m.detailHeader(), dw-2, ""), dw-2)}, m.detailRows(h-2)...)
	status, _ := m.footer()
	// lipgloss.JoinVertical padded every line to the widest, which is the status line when it is the longer
	sl := strings.Split(status, "\n")
	width := sw + iw + dw
	for _, l := range sl {
		width = max(width, ansi.StringWidth(l))
	}
	fill := strings.Repeat(" ", width-sw-iw-dw)
	var b strings.Builder
	b.Grow((h + 3) * width * 4)
	for i := range h + 2 {
		b.WriteString(sp[i])
		b.WriteString(ip[i])
		switch i {
		case 0:
			b.WriteString(f.top)
		case h + 1:
			b.WriteString(f.bottom)
		default:
			b.WriteString(f.side)
			b.WriteString(body[i-1])
			b.WriteString(f.side)
		}
		b.WriteString(fill)
		b.WriteByte('\n')
	}
	for i, l := range sl {
		b.WriteString(pad(l, width))
		if i < len(sl)-1 {
			b.WriteByte('\n')
		}
	}
	v := b.String()
	if m.menu {
		v = m.withOverlay(v)
	}
	return v
}

// paneCache holds the sessions and items panes as drawn, and the detail pane's frame, shared by every copy of the
// model like renderCache and keyed the same way.
type paneCache struct {
	sess   sessPane
	list   once[listKey, *itemList]
	items  once[itemsKey, []string]
	detail once[frameKey, paneFrame]
}

func newPaneCache() *paneCache { return &paneCache{} }

type listKey struct {
	gen, si int
	run     string // this viewer's running item, drawn as running
	day     int    // the local date stamps are relative to
	look    look
}

type itemsKey struct {
	list           *itemList
	top, row, w, h int
	on             bool
	look           look
}

type frameKey struct {
	w    int
	on   bool
	look look
}

// sessPane is the sessions pane as drawn, valid while its key holds and the clock is in [from, until), the span in
// which every label's age reads the same.
type sessPane struct {
	key         sessKey
	from, until time.Time
	lines       []string
}

type sessKey struct {
	gen, si, w, h int
	on            bool
	look          look
}

// sessionsPane is the sessions pane's lines, built again when the rows, selection, size, focus or styling change or
// a label's age would read differently.
func (m model) sessionsPane(w, h int) []string {
	now, c := time.Now(), &m.pc.sess
	k := sessKey{m.rowsGen, m.si, w, h, m.focus == 0, lookNow()}
	if c.lines != nil && c.key == k && !now.Before(c.from) && now.Before(c.until) {
		return c.lines
	}
	c.key, c.from, c.until = k, time.Time{}, now.Add(time.Hour) // the labels' ages narrow this
	var rows []string
	for _, r := range m.rows {
		pending := 0
		for _, it := range r.items {
			if r.state[it.ID] == queue.Pending {
				pending++
			}
		}
		rows = append(rows, sessionLabel(r, r.title, pending, now))
		if len(r.items) > 0 {
			from, until := ageSpan(newest(r.items), now)
			if from.After(c.from) {
				c.from = from
			}
			if until.Before(c.until) {
				c.until = until
			}
		}
	}
	c.lines = paneLines("Sessions", w, h, m.focus == 0, rows, m.si)
	return c.lines
}

// itemsPane is the items pane's lines, built again when its rows, window, selection, size, focus or styling change.
func (m model) itemsPane(w, h int) []string {
	s := m.session()
	if s == nil {
		return m.pc.items.get(itemsKey{w: w, h: h, on: m.focus == 1, look: lookNow()}, func() []string {
			return paneLines("Items", w, h, m.focus == 1, nil, -1)
		})
	}
	l, top, row := m.itemWin(s, m.now())
	return m.pc.items.get(itemsKey{l, top, row, w, h, m.focus == 1, lookNow()}, func() []string {
		rows, _, sel := m.itemView(s, m.now())
		return paneLines("Items", w, h, m.focus == 1, rows, sel)
	})
}

// paneFrame is a pane's border, styled for focus, and its title row padded to the inner width.
type paneFrame struct{ top, side, bottom, title string }

func paneFrameOf(title string, w int, on bool) paneFrame {
	bs := border
	if on {
		bs = focused
	}
	st, b := lipgloss.NewStyle().Foreground(bs.GetBorderTopForeground()), lipgloss.RoundedBorder()
	return paneFrame{
		top:    st.Render(b.TopLeft + strings.Repeat(b.Top, w-2) + b.TopRight),
		side:   st.Render(b.Left),
		bottom: st.Render(b.BottomLeft + strings.Repeat(b.Bottom, w-2) + b.BottomRight),
		title:  pad(bold.Render(title), w-2),
	}
}

// paneLines is a pane drawn line by line as the lipgloss border style drew it: the title then the rows, each cut to
// w-2 cells and padded (the selected one reversed), h lines in all between the top and bottom borders.
func paneLines(title string, w, h int, on bool, rows []string, selected int) []string {
	f := paneFrameOf(title, w, on)
	out := []string{f.top, f.side + f.title + f.side}
	for j, r := range rows {
		if len(out) == h+1 {
			break
		}
		r = lipgloss.NewStyle().MaxWidth(w - 2).Render(r)
		if j == selected {
			r = sel.Render(r)
		}
		out = append(out, f.side+pad(r, w-2)+f.side)
	}
	for len(out) < h+1 {
		out = append(out, f.side+strings.Repeat(" ", w-2)+f.side)
	}
	return append(out, f.bottom)
}

// pad fills s with spaces to w cells.
func pad(s string, w int) string { return s + strings.Repeat(" ", max(0, w-ansi.StringWidth(s))) }

// ageSpan is when age(now-t) last changed and when it next will: it changes only on whole minutes after t.
func ageSpan(t, now time.Time) (from, until time.Time) {
	d := now.Sub(t)
	if d < time.Minute {
		return time.Time{}, t.Add(time.Minute)
	}
	from = t.Add(d.Truncate(time.Minute))
	return from, from.Add(time.Minute)
}

// newest is the latest Created among items.
func newest(items []queue.Item) time.Time {
	n := items[0].Created
	for _, it := range items {
		if it.Created.After(n) {
			n = it.Created
		}
	}
	return n
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
		parts = append(parts, age(now.Sub(newest(r.items))))
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
		queue.Rejected: "⊘", queue.Expired: "⌛", queue.Modified: "!", queue.Canceled: "■", queue.Revoked: "↩",
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
