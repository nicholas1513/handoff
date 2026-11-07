// Package tui is the review-and-run viewer: sessions | items | detail.
//
// Everything shown from queue/ was written by the agent, so it goes through sanitize first.
package tui

import (
	"context"
	"fmt"
	"maps"
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

// wheelBurst: a terminal sends one wheel click as several events in one write (Ghostty: 3, under 0.5 ms apart;
// separate writes arrive 4.5 ms or more apart), so a list moves one item per burst, not one per event.
const wheelBurst = 2 * time.Millisecond

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
	state   map[string]string    // state per item, read at refresh and by loadData: drawing reads no result files
	title   string               // the transcript's /resume title, read at refresh
}

type tickMsg time.Time

type runDone struct {
	id, desc string
	err      error
}

// run is one of this viewer's runs in flight.
type run struct {
	rev  *queue.Reviewed // what was reviewed and is running; reopening the item shows this, not a new snapshot
	stop context.CancelFunc
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
	// this viewer's runs in flight by item id; copied on write
	runs          map[string]*run
	quitArmed     bool // keyboard: q pressed while runs are in flight, waiting for the second
	cancelArmed   bool // keyboard: one c pressed on a running item, waiting for the second
	cancelConfirm bool // mouse: Cancel run clicked, waiting for Yes
	restoreArmed  string // the restore overlay is open; what it would restore
	saved         string // the last save of the running sessions, for the Sessions pane title
	scroll   int
	itemTop  int            // first items-pane row drawn
	boxTop   map[string]int // first line drawn per box id ("result", "prompt", "file:<name>"); copied on write
	boxSel   string         // selected box; "" or an unframed id means the first framed box
	diff     fileDiff       // the reviewed edit item's diff, parsed at open
	diffExp  map[int][2]int // context revealed per gap between hunks; copied on write
	boxX     map[string]int // pan offset per unwrapped box id; copied on write
	settings viewerSettings
	menu     bool // the settings overlay is open
	manual    bool // the manual is open, over settings if it was opened from there
	manualTop int  // its first row shown
	menuSel  int  // its selected row in settingRows
	status   string
	w, h     int
	titles   titleCache

	data    *detailData // what the detail shows from disk and the clock; see loadData
	rowsGen int         // bumped whenever m.rows or any sessionRow's items/state/fin/nOut/title change
	seq     int         // bumped by refresh, restate, a run starting, o and re-checking the open item, which a load in flight must not undo
	rc      *renderCache
	pc      *paneCache

	am       *amView                // the open am item's review copy, once built (see reviewCmd)
	amFlight *flight                // the review copy being built, if any
	vscode   func(dir string) error // opens a directory in VS Code; nil when it isn't installed

	now        func() time.Time
	advancedAt time.Time
	wheelAt    time.Time // the last wheel event, and its pane and direction, for wheelBurst
	wheelPane  int
	wheelD     int
}

// gens numbers row snapshots, unique across copies of the model so a shared cache can't mistake one for another.
var gens atomic.Int64

func nextGen() int { return int(gens.Add(1)) }

func Run(q *queue.Queue, home string) error {
	_, err := tea.NewProgram(newModel(q, home), tea.WithAltScreen(), tea.WithMouseCellMotion()).Run()
	return err
}

func newModel(q *queue.Queue, home string) model {
	m := model{q: q, home: home, w: 120, h: 40, titles: titleCache{}, now: time.Now, rc: newRenderCache(), pc: newPaneCache(),
		vscode: findVSCode(home)}
	var err error
	if m.settings, err = loadSettings(q.Root); err != nil {
		m.status = "settings: " + err.Error()
	}
	m.refresh()
	return m
}

func (m model) Init() tea.Cmd { return tea.Batch(tick(), sweep(m.q)) }

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

// refresh reloads the queue on the event loop, keeping the selection on the same session and item ids. The tick
// reloads off the loop instead (see reload).
func (m *model) refresh() {
	m.seq++ // a load in flight read the disk before this one
	m.apply(m.load()())
}

// reload reads the queue off the event loop. The read comes back as a loaded message, which Update applies.
func (m *model) reload() tea.Cmd {
	f := m.load()
	return func() tea.Msg { return f() }
}

// loaded is one read of the queue: the rows, every item's state (also of items gone from the list, for the pull
// rule), and what the detail shows for the item that was under review when the read began.
//
// result.json and item.json are written in place, so a read can catch one mid-write. For that tick the item then
// reads as it did an instant before (claimed, so running) with the JSON error under its Result, or as an invalid
// entry: the reads on the event loop could catch the runner's or another process's writes the same way. This
// viewer's own writes bump seq, so a load that overlapped them is never applied.
type loaded struct {
	seq     int // the model's seq when the read began
	rows    []sessionRow
	same    bool // rows equal the model's rows when the read began
	st      map[string]string
	err     error
	titles  titleCache
	rev     *queue.Reviewed
	changed bool
	data    *detailData // nil: rev is no longer listed
	saved   string      // see model.saved; read each load, since a /handoff:claude-sessions save can change it
}

// load returns the read of the queue for refresh. It touches no model field and shares nothing the event loop
// changes (the rows and the reviewed item are never written once set, and it updates its own copy of the titles),
// so it can run off the loop.
func (m *model) load() func() loaded {
	q, home, now, old := m.q, m.home, m.now(), m.rows
	l := loaded{seq: m.seq, titles: maps.Clone(m.titles), rev: m.reviewed}
	return func() loaded {
		items, invalid, err := q.List()
		l.err, l.saved = err, savedLabel(q.Root, now)
		byID := map[string]*sessionRow{}
		var rows []*sessionRow
		for i := len(items) - 1; i >= 0; i-- { // newest first
			it := items[i]
			r := byID[it.SessionID]
			if r == nil {
				r = &sessionRow{id: it.SessionID}
				if s, err := q.ReadSession(it.SessionID); err == nil {
					r.sess, r.title = &s, l.titles.get(s.TranscriptPath)
				}
				byID[it.SessionID] = r
				rows = append(rows, r)
			}
			r.items = append(r.items, it)
		}
		l.rows, l.st = []sessionRow{}, map[string]string{} // a new array: copies of the model share the old one
		for _, r := range rows {
			r.nOut, r.fin, r.state = sortItems(q, r.items, now)
			maps.Copy(l.st, r.state)
			l.rows = append(l.rows, *r)
		}
		if len(invalid) > 0 {
			sort.Sort(sort.Reverse(sort.StringSlice(invalid)))
			l.rows = append(l.rows, sessionRow{id: "invalid", invalid: invalid})
		}
		l.same = reflect.DeepEqual(l.rows, old)
		for _, r := range old { // gone from the list: their state as State reads it
			for _, it := range r.items {
				if _, ok := l.st[it.ID]; !ok {
					l.st[it.ID] = q.State(it, now)
				}
			}
		}
		if r := l.rev; r != nil {
			for _, s := range l.rows {
				if st, ok := s.state[r.Item.ID]; ok {
					l.changed = st == queue.Pending && q.Verify(*r) != nil
					l.data = readData(q, home, s.sess, r, st)
				}
			}
		}
		return l
	}
}

// apply puts a load on screen, keeping the selection on the same session and item ids. It reads no files unless
// the selection is no longer the item the load read the detail for.
func (m *model) apply(l loaded) {
	st := func(it queue.Item) string { // as drawn: this viewer's runs are running, also before their claims land
		if m.runs[it.ID] != nil {
			return queue.Running
		}
		return l.st[it.ID]
	}
	var keepS, keepI string
	had := false // the kept session had a pending item before this reload
	if s := m.session(); s != nil {
		keepS = s.id
		_, had = oldestPending(s, st)
	}
	if it := m.item(); it != nil {
		keepI = it.ID
	}
	if l.err != nil {
		m.status = l.err.Error()
	}
	m.titles, m.saved = l.titles, l.saved
	if !l.same {
		m.rows, m.rowsGen = l.rows, nextGen()
	}
	m.si, m.ii = 0, 0
	if keepS == "" {
		m.enterSession(0, st)
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
	if it := m.item(); it != nil && !had {
		if s := st(*it); s != queue.Pending && s != queue.Running {
			if best, ok := oldestPending(m.session(), st); ok {
				m.ii, m.advancedAt, m.reviewed = best, m.now(), nil
			}
		}
	}
	if it := m.item(); it != nil && l.data != nil && m.reviewed == l.rev && it.ID == l.rev.Item.ID {
		m.changed = l.changed
		if !reflect.DeepEqual(l.data, m.data) { // the old snapshot when nothing changed, so caches keyed on it hold
			m.data = l.data
		}
		m.clampScroll()
		return
	}
	m.open() // the selection is not the item read for the detail: read it now
}

// enterSession selects the row's oldest pending item by st (guarding space like an advance), else the newest.
func (m *model) enterSession(si int, st func(queue.Item) string) {
	m.si, m.ii = si, 0
	if s := m.session(); s != nil {
		if best, ok := oldestPending(s, st); ok {
			m.ii, m.advancedAt = best, m.now()
		}
	}
}

// live is an item's state on disk now, or running while this viewer runs it (its claim may not have landed yet).
func (m *model) live(it queue.Item) string {
	if m.runs[it.ID] != nil {
		return queue.Running
	}
	return m.q.State(it, time.Now())
}

// open snapshots the selected item, unless it is already the one under review, and re-reads the detail's data.
func (m *model) open() {
	defer func() { m.loadData(); m.clampScroll() }() // clamp to the detail as just read, not as last drawn
	it := m.item()
	if it != nil && m.reviewed != nil && m.reviewed.Item.ID == it.ID {
		// newer than a load in flight, which would put its older changed and data back
		m.changed, m.seq = m.q.State(*it, time.Now()) == queue.Pending && m.q.Verify(*m.reviewed) != nil, m.seq+1
		return
	}
	m.cancelArmed, m.cancelConfirm = false, false // they were raised on the item open before
	if it == nil {
		m.reviewed, m.openErr, m.confirm, m.armed = nil, nil, false, false
		return
	}
	if r := m.runs[it.ID]; r != nil {
		m.reviewed, m.openErr = r.rev, nil // a new snapshot would rewrite the files the run was given
	} else {
		rev, err := m.q.Snapshot(it.ID)
		m.reviewed, m.openErr = &rev, err
	}
	m.changed, m.confirm, m.armed, m.scroll = false, false, false, 0
	m.boxTop, m.boxSel, m.diff, m.diffExp, m.boxX = nil, "", fileDiff{}, nil, nil
	if r := m.reviewed; m.openErr != nil {
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

// activateMenuRow acts on a row of the settings overlay: the settings, then the manual.
func (m *model) activateMenuRow(i int) {
	if i == len(settingRows) {
		m.openManual()
		return
	}
	m.toggleSetting(i)
}

// toggleSetting toggles settingRows[i] and saves.
func (m *model) toggleSetting(i int) {
	settingRows[i].toggle(&m.settings, m.vscode != nil)
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
		m.enterSession(clamp(m.si+d, len(m.rows)), m.live)
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
func finish(q *queue.Queue, it queue.Item) time.Time {
	if r, err := q.ReadResult(it); err == nil {
		return r.RanAt
	}
	if at, ok := q.RevokedAt(it); ok {
		return at
	}
	return it.Expires
}

// sortItems: outstanding by Created ascending, then completed by finish time descending; ties by ID.
// Returns the outstanding count, the completed items' finish times and every item's state.
func sortItems(q *queue.Queue, items []queue.Item, now time.Time) (int, map[string]time.Time, map[string]string) {
	out := map[string]bool{}
	at := map[string]time.Time{}
	st := map[string]string{}
	for _, it := range items {
		st[it.ID] = q.State(it, now)
		if out[it.ID] = outstanding(st[it.ID]); out[it.ID] {
			at[it.ID] = it.Created
		} else {
			at[it.ID] = finish(q, it)
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
		add("⚠ "+oneLine(id), i) // the raw directory name: a newline in it would add a screen row
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

// itemList is itemPane, cached while the rows, the selected session, this viewer's runs, the day the stamps are
// relative to and the styling are unchanged. Callers must not modify it.
func (m *model) itemList(s *sessionRow, now time.Time) *itemList {
	runs, n := "", now.Local()
	if len(m.runs) > 0 { // building the key allocates even for no runs, on every frame
		runs = strings.Join(slices.Sorted(maps.Keys(m.runs)), " ")
	}
	return m.pc.list.get(listKey{m.rowsGen, m.si, runs, n.Year()*1000 + n.YearDay(), lookNow()}, func() *itemList {
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
	return m.reviewed != nil && m.live(m.reviewed.Item) == queue.Pending
}

func (m *model) startRun() tea.Cmd {
	// like arm and reject: a prompt a refresh took off the screen answers nothing
	if !m.shownPending() || !m.pending() || !m.amReady() {
		return nil
	}
	if m.changed {
		m.status = "item changed after you opened it: press o to review it again"
		m.confirm, m.armed = false, false
		return nil
	}
	for _, r := range m.runs { // a second am would pass its base pin now, then apply on top of the first's commits
		if id := m.reviewed.AmRepoID; id != "" && r.rev.AmRepoID == id {
			m.status = "a git am into this repository is still running: run this once it finishes"
			m.confirm, m.armed = false, false
			return nil
		}
	}
	m.confirm, m.armed, m.status, m.seq = false, false, "running…", m.seq+1
	q, r := m.q, *m.reviewed
	if v := m.shownAm(); v != nil {
		r.AmCommits, r.AmChecked = v.made, true
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.runs = with(m.runs, r.Item.ID, &run{m.reviewed, cancel})
	return func() tea.Msg {
		_, err := runner.Run(ctx, q, r, time.Now())
		return runDone{id: r.Item.ID, desc: r.Item.Description, err: err}
	}
}

// cancelable: the selected item is one of this viewer's runs in progress, or a run left behind by a viewer that went
// away.
func (m model) cancelable() bool {
	it := m.item()
	return it != nil && m.live(*it) == queue.Running
}

// drawnState is the selected item's state as drawn (see shownState), or "".
func (m model) drawnState() string {
	if it := m.item(); it != nil {
		return m.shownState(m.session(), *it)
	}
	return ""
}

// shownPending and shownCancelable are pending() and cancelable() as drawn: from snapshots, so drawing reads no files.
func (m model) shownPending() bool {
	return m.reviewed != nil && m.drawnState() == queue.Pending
}

func (m model) shownCancelable() bool { return m.drawnState() == queue.Running }

// cancel stops the selected run: this viewer's own via its context, a stale one via runner.CancelStale.
func (m *model) cancel() {
	m.cancelArmed, m.cancelConfirm = false, false
	if !m.cancelable() {
		return
	}
	if r := m.runs[m.item().ID]; r != nil {
		r.stop()
		m.status = "canceling…"
		return
	}
	var mine []int // the OS may have given the stale run's recorded pid to one of these
	for _, r := range m.runs {
		if pid, ok := m.q.ClaimPID(r.rev.Item); ok {
			mine = append(mine, pid)
		}
	}
	if err := runner.CancelStale(m.q, *m.item(), time.Now(), mine...); err != nil {
		m.status = "cancel: " + err.Error()
		return
	}
	m.status = "canceled"
	m.refresh()
}

// reject acts, like every prompt, only when the screen shows the item as pending and the disk still says so.
func (m *model) reject() {
	if !m.shownPending() || !m.pending() {
		return
	}
	it := m.reviewed.Item
	err := m.q.Claim(it) // so a run can't start after the reject
	if err == nil {
		err = m.q.WriteResult(it, queue.Result{State: queue.Rejected, ExitCode: -1, RanAt: time.Now(), SHA256Reviewed: m.reviewed.Hash})
	}
	m.confirm, m.armed, m.status = false, false, "rejected"
	m.loadData() // also on an error: the claim may have landed
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
	if best, ok := oldestPending(s, m.live); ok {
		m.ii, m.advancedAt, m.reviewed = best, m.now(), nil
		m.open()
	}
}

// oldestPending is the index of the row's item pending by st with the earliest Created (first in list order on a tie).
func oldestPending(s *sessionRow, st func(queue.Item) string) (int, bool) {
	best := -1
	for i, it := range s.items {
		if st(it) == queue.Pending && (best < 0 || it.Created.Before(s.items[best].Created)) {
			best = i
		}
	}
	return best, best >= 0
}

// restate records one item's state in the rows when it differs. loadData calls it with the state it reads, so the
// lists show what the detail shows at once: after a reject, or on opening an item that changed since the last refresh.
func (m *model) restate(id, st string) {
	for i, r := range m.rows {
		if old, ok := r.state[id]; ok && old != st {
			m.rows = slices.Clone(m.rows) // copy on write, like the maps: other copies of the model share the array
			m.rows[i].state = with(r.state, id, st)
			m.rowsGen, m.seq = nextGen(), m.seq+1
		}
	}
}

// shownState is an item's state as drawn: running while this viewer runs it, else the refresh snapshot.
func (m model) shownState(s *sessionRow, it queue.Item) string {
	if m.runs[it.ID] != nil {
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
	return next, tea.Batch(cmd, next.reviewCmd())
}

func (m model) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.clampScroll()
	case tickMsg:
		return m, m.reload()
	case loaded:
		if msg.seq != m.seq { // this viewer changed something since the read began, which applying it could undo
			return m, m.reload()
		}
		m.apply(msg)
		return m, tick() // the next tick waits for this load, so slow loads can't pile up
	case runDone:
		if r := m.runs[msg.id]; r != nil {
			r.stop() // releases the context
			m.runs = maps.Clone(m.runs)
			delete(m.runs, msg.id)
		}
		if it := m.item(); it != nil && it.ID == msg.id {
			m.cancelArmed, m.cancelConfirm = false, false // the prompt to cancel this run goes with it
		}
		m.quitArmed = m.quitArmed && len(m.runs) > 0   // left armed with no runs, it would take the next key unseen
		d := ansi.Truncate(oneLine(msg.desc), 24, "…") // names the run without pushing the key hints off the line
		m.status = "finished: " + d
		if msg.err != nil {
			m.status = "run: " + d + ": " + msg.err.Error()
		}
		m.refresh()
		m.advance(msg.id)
	case statusMsg:
		m.status = string(msg)
	case *amView:
		if f := m.amFlight; f != nil && msg.key == f.key {
			f.stop()
			m.am, m.amFlight = msg, nil
			if m.status == notReady {
				m.status = ""
			}
			m.clampScroll()
		}
	case tea.KeyMsg:
		return m.key(msg.String())
	case tea.MouseMsg:
		return m.mouse(msg)
	}
	return m, nil
}

// arm is the only way a key starts a run; holds the advanceGuard check. Like every prompt, it is set only when the
// screen shows the item as pending (the drawn state) and the disk still says so (the live one).
func (m *model) arm() {
	m.armed = m.shownPending() && m.pending() && m.now().Sub(m.advancedAt) >= advanceGuard && m.amReady()
}

func (m model) key(k string) (tea.Model, tea.Cmd) {
	if m.restoreArmed != "" { // the overlay takes every key while open: nothing reaches the item behind it
		switch k {
		case " ":
			m.restoreArmed = ""
			return m, restoreCmd(m.q)
		case "x":
			m.restoreArmed = ""
		}
		return m, nil
	}
	if m.manual {
		return m.manualKeyPress(k)
	}
	if m.menu { // the overlay takes every key while open
		switch k {
		case " ", "enter":
			m.activateMenuRow(m.menuSel)
		case "?":
			m.openManual()
		case "up", "k":
			m.menuSel = max(0, m.menuSel-1)
		case "down", "j":
			m.menuSel = min(len(settingRows), m.menuSel+1) // the row after the settings is the manual
		case "esc", "s", "q":
			m.menu = false
		}
		return m, nil
	}
	if m.quitArmed {
		m.quitArmed = false
		if k == "q" || k == "ctrl+c" {
			return m, tea.Quit
		} // any other key only disarms and is not acted on
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
		if len(m.runs) > 0 { // quitting stops waiting for them, so their results would go unrecorded: ask again
			m.quitArmed = true
			return m, nil
		}
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
		m.cancelArmed = m.shownCancelable() && m.cancelable()
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
		if f := m.amFlight; f != nil { // its git am may be reading the snapshot that open is about to rewrite
			f.stop()
		}
		// and the review copy is built again, which brings a good one back from reviews.built and retries a failed one
		m.reviewed, m.am, m.amFlight, m.seq = nil, nil, nil, m.seq+1
		m.open()
	case "s":
		m.menu = true
	case "?":
		m.openManual()
	case "ctrl+s":
		return m, saveCmd(m.q, m.now())
	case "ctrl+l":
		m.armRestore()
	case "v":
		if m.layout().open >= 0 {
			return m, m.openCmd()
		}
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
func (m model) footer() (string, int) {
	l := m.layout()
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
	if l.open >= 0 {
		keys = append(keys, "v open in VS Code")
	}
	if m.changed {
		keys = append(keys, "o re-open")
	}
	plain, msg := strings.Join(append(keys, settingsKey, "q quit", manualKey), " · "), m.status
	if n := len(m.runs); m.quitArmed {
		msg = fmt.Sprintf("%d run%s in progress: q again to quit", n, map[bool]string{true: "s"}[n > 1])
	}
	f := m.rc.foot.get(styleKey{plain, msg, lookNow()}, func() footLine {
		status, x := dim.Render(plain), ansi.StringWidth(plain[:strings.Index(plain, settingsKey)])
		if msg != "" {
			s := sanitize(msg) // errors can quote agent-written text, such as an edit's target path
			status = s + dim.Render("  ·  ") + status
			x += ansi.StringWidth(s + "  ·  ")
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
	if m.restoreArmed != "" { // the overlay takes every mouse event while open, the wheel and releases too
		if !press {
			return m, nil
		}
		return m.restoreClick(msg.X, msg.Y)
	}
	if m.manual {
		return m.manualMouse(msg, press)
	}
	if m.menu { // the overlay takes every click while open
		if press {
			ol, ox, oy := m.overlay()
			inside := msg.X >= ox && msg.X < ox+lipgloss.Width(ol[0]) && msg.Y >= oy && msg.Y < oy+len(ol)
			if i := msg.Y - oy - settingsRow; inside && i >= 0 && i <= len(settingRows) {
				m.menuSel = i
				m.activateMenuRow(i)
			} else if !inside {
				m.menu = false
			}
		}
		return m, nil
	}
	if (m.armed || m.cancelArmed || m.quitArmed) && press {
		m.armed, m.cancelArmed, m.quitArmed = false, false, false // a click only cancels the arm; it is not acted on
		return m, nil
	}
	switch {
	case msg.Button == tea.MouseButtonWheelUp || msg.Button == tea.MouseButtonWheelDown:
		m.focus = pane
		d := 1
		if msg.Button == tea.MouseButtonWheelUp {
			d = -1
		}
		now := m.now()
		burst := now.Sub(m.wheelAt) < wheelBurst && pane == m.wheelPane && d == m.wheelD
		m.wheelAt, m.wheelPane, m.wheelD = now, pane, d
		if id, _, ok := m.boxAt(msg.Y); pane == 2 && ok {
			m.scrollBox(id, d) // a line per event: a click scrolls as far as the terminal's own scrollback
			m.boxSel = id
		} else if pane == 2 || !burst {
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
	case pane == 2 && msg.Y == buttonY && m.shownCancelable() && m.cancelable():
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
			m.confirm = m.shownPending() && m.pending() && m.amReady()
		case !m.confirm && x >= rejectX && x < rejectX+10:
			m.reject()
		}
		m.focus = 2
	case pane == 2 && m.openAt(msg.Y):
		m.focus = 2
		return m, m.openCmd()
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
			m.enterSession(row, m.live)
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
	switch {
	case m.restoreArmed != "":
		ol, x, y := m.restoreOverlay()
		v = withOverlay(v, ol, x, y)
	case m.manual:
		ol, x, y := m.manualOverlay()
		v = withOverlay(v, ol, x, y)
	case m.menu:
		ol, x, y := m.overlay()
		v = withOverlay(v, ol, x, y)
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
	runs    string // the ids of this viewer's runs in flight, sorted: drawn as running
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
	saved         string
}

// sessionsPane is the sessions pane's lines, built again when the rows, selection, size, focus or styling change or
// a label's age would read differently.
func (m model) sessionsPane(w, h int) []string {
	now, c := time.Now(), &m.pc.sess
	k := sessKey{m.rowsGen, m.si, w, h, m.focus == 0, lookNow(), m.saved}
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
	title := "Sessions"
	if m.saved != "" {
		title += " · " + m.saved
	}
	c.lines = paneLines(ansi.Truncate(title, w-2, "…"), w, h, m.focus == 0, rows, m.si)
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
