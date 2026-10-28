package tui

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/nicholas1513/handoff/internal/queue"
	"github.com/nicholas1513/handoff/internal/rules"
	"github.com/nicholas1513/handoff/internal/runner"
)

var (
	cmdProg = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	cmdFlag = lipgloss.NewStyle().Foreground(lipgloss.Color("5"))
	cmdAtt  = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	attRe   = regexp.MustCompile(`\{file:([^}]*)\}`)
)

// flatten strips styling and rejoins wrapped lines (newline plus following indent).
func flatten(s string) string {
	s = ansi.Strip(s)
	for {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			return s
		}
		j := i + 1
		for j < len(s) && (s[j] == ' ' || s[j] == '\t') {
			j++
		}
		s = s[:i] + s[j:]
	}
}

func isURL(w string) bool { return strings.HasPrefix(w, "http://") || strings.HasPrefix(w, "https://") }

// shown sanitizes one argv word and Go-quotes it when empty or holding whitespace or a quote,
// so a newline can't draw a fake argument line and one word can't pass for two.
func shown(w string) string {
	w = sanitize(w)
	if w == "" || strings.ContainsFunc(w, func(r rune) bool { return unicode.IsSpace(r) || r == '"' }) {
		return strconv.Quote(w)
	}
	return w
}

// word renders one argv word; URLs break at "/" boundaries, continuation lines indented by indent.
func word(w string, indent int, sizes map[string]int) string {
	q := shown(w)
	if m := attRe.FindStringSubmatch(w); m != nil {
		note := " (attached, see below)"
		if n, ok := sizes[m[1]]; ok {
			note = fmt.Sprintf(" (attached, %.1f KB, see below)", float64(n)/1000)
		}
		return cmdAtt.Render("📎 ") + q + cmdAtt.Render(note)
	}
	if q != w || !isURL(w) {
		return q
	}
	rest := w[strings.Index(w, "://")+3:]
	h := strings.IndexByte(rest, '/')
	if h < 0 {
		return cmdProg.Render(w)
	}
	cut := len(w) - len(rest) + h
	parts := []string{cmdProg.Render(w[:cut])}
	segs := strings.Split(w[cut:], "/")[1:] // each segment keeps its leading "/"
	for i, s := range segs {
		s = "/" + s
		if i == len(segs)-1 {
			parts = append(parts, cmdProg.Render(s))
		} else {
			parts = append(parts, dim.Render(s))
		}
	}
	return strings.Join(parts, "\n"+strings.Repeat(" ", indent))
}

// commandLines lays argv out in argv order: program line, then one line per flag (with its value) or positional.
func commandLines(argv []string, sizes map[string]int) []string {
	i, head := 0, []string{}
	for i < len(argv) && !strings.HasPrefix(argv[i], "-") {
		head = append(head, cmdProg.Render(shown(argv[i])))
		i++
	}
	type arg struct {
		flag, val string // flag "" marks a positional
		has       bool
	}
	var args []arg
	width := 0
	for i < len(argv) {
		a := argv[i]
		i++
		if !strings.HasPrefix(a, "-") || a == "-" {
			args = append(args, arg{val: a})
			continue
		}
		f := arg{flag: a}
		if i < len(argv) && !strings.HasPrefix(argv[i], "--") {
			f.val, f.has, i = argv[i], true, i+1
		}
		args = append(args, f)
		width = max(width, len(shown(a)))
	}
	var lines []string
	if len(head) > 0 {
		lines = append(lines, strings.Join(head, " "))
	}
	for _, a := range args {
		if a.flag == "" {
			lines = append(lines, "  "+word(a.val, 4, sizes))
			continue
		}
		s := shown(a.flag)
		l := "  " + cmdFlag.Render(s)
		if a.has {
			l += strings.Repeat(" ", width-len(s)+1) + word(a.val, 2+width+1, sizes)
		}
		lines = append(lines, l)
	}
	return lines
}

// methodOf returns the upper-cased HTTP method from the last --method/-X/--request, or "" (CLIs honour the last).
func methodOf(argv []string) string {
	mt := ""
	for i, a := range argv {
		switch {
		case a == "--method" || a == "-X" || a == "--request":
			if i+1 < len(argv) {
				mt = strings.ToUpper(argv[i+1])
			}
		case strings.HasPrefix(a, "--method="):
			mt = strings.ToUpper(strings.TrimPrefix(a, "--method="))
		case strings.HasPrefix(a, "--request="):
			mt = strings.ToUpper(strings.TrimPrefix(a, "--request="))
		}
	}
	return mt
}

func heading(title string) string {
	return dim.Render("── ") + section.Render(title) + dim.Render(" ──")
}

func chip(text, color string) string {
	return lipgloss.NewStyle().Foreground(lipgloss.Color(color)).Bold(true).Render(text)
}

// seg is one run of the detail: plain text, or a scrollable box that is framed only when it exceeds its cap.
type seg struct {
	text string
	box  *box
}

type box struct {
	id       string
	lines    []string // one per drawn line, wrapped to the inner width when framed
	framed   bool
	cap, top int
	meta     []dline // diff box only: the logical row each drawn line came from
	pan      bool    // unwrapped: lines are cut at column x and pan sideways
	x        int
}

const (
	resultBox = "result"
	promptBox = "prompt"
	diffBox   = "diff"
)

// diffColor colours a unified-diff line by its first character: added green, removed red, hunk cyan.
func diffColor(l string) lipgloss.TerminalColor {
	switch {
	case strings.HasPrefix(l, "+"):
		return lipgloss.Color("2")
	case strings.HasPrefix(l, "-"):
		return lipgloss.Color("1")
	case strings.HasPrefix(l, "@@"):
		return lipgloss.Color("6")
	}
	return lipgloss.NoColor{}
}

func (m model) detail() string { return m.detailOf(m.layout()) }

func (m model) detailOf(l layout) string {
	var b strings.Builder
	for _, s := range l.segs {
		b.WriteString(s.text)
		if s.box != nil {
			b.WriteString(m.render(*s.box, s.box.id == l.sel))
		}
	}
	return b.String()
}

// capPct is a box height: pct% of the window, at least 3 lines.
func (m model) capPct(pct int) int { return max(3, m.h*pct/100) }

// boxBg is the framed box's panel colour, visibly lighter than the terminal's.
var boxBg = lipgloss.AdaptiveColor{Light: "#e8e8ef", Dark: "#2a2e3f"}

// mkBox wraps its text with wrap(width): to the inner width (dw-6) and framed when that is longer than cap, else
// plainly to dw-2. Its top is m.boxTop[id], clamped to the lines.
func (m model) mkBox(id string, wrap func(w int) []string, cap int) box {
	_, _, dw := columns(m.w)
	if l := wrap(max(1, dw-6)); len(l) > cap {
		return box{id: id, lines: l, framed: true, cap: cap, top: max(0, min(m.boxTop[id], len(l)-cap))}
	}
	return box{id: id, lines: wrap(dw - 2), cap: cap}
}

// render draws a box and its trailing newline: the framed window, or the plain lines.
func (m model) render(b box, sel bool) string {
	if !b.framed {
		return strings.Join(b.lines, "\n") + "\n"
	}
	var colors []lipgloss.TerminalColor
	for _, d := range b.meta[min(b.top, len(b.meta)):min(b.top+b.cap, len(b.meta))] {
		colors = append(colors, d.color)
	}
	return m.frame(b.lines[b.top:b.top+b.cap], b.top, len(b.lines), sel, colors) + "\n"
}

// placed is a framed box and its first content row in the wrapped detail.
type placed struct {
	box
	first int
}

// layout is one build of the detail: its runs, the framed boxes in display order with where each sits in the
// wrapped detail, and the selected one. Building it is most of a frame's cost (the wheel can send dozens of
// events a second), so code that reads the detail more than once takes one layout and reuses it.
type layout struct {
	segs  []seg
	boxes []placed
	sel   string // m.boxSel while it is framed, else the first framed box, else ""
}

func (m model) layout() (l layout) {
	l.segs = m.detailSegs()
	n := 0
	for _, s := range l.segs {
		if s.box == nil {
			n += strings.Count(m.wrapDetail(strings.TrimSuffix(s.text, "\n")), "\n") + 1
		} else if s.box.framed {
			l.boxes = append(l.boxes, placed{*s.box, n + 1})
			n += s.box.cap + 2
		} else {
			n += max(1, len(s.box.lines))
		}
	}
	for _, b := range l.boxes {
		if l.sel == "" || b.id == m.boxSel {
			l.sel = b.id
		}
		if b.id == m.boxSel {
			break
		}
	}
	return l
}

// box returns the box with that id, if drawn.
func (l layout) box(id string) (box, bool) {
	for _, s := range l.segs {
		if s.box != nil && s.box.id == id {
			return *s.box, true
		}
	}
	return box{}, false
}

func (m model) boxes() []placed { return m.layout().boxes }

func (m model) selBox() string { return m.layout().sel }

// nextBox moves the selection d framed boxes along in display order, stopping at either end.
func (m *model) nextBox(d int) {
	l := m.layout()
	for i, b := range l.boxes {
		if b.id == l.sel {
			m.boxSel = l.boxes[max(0, min(i+d, len(l.boxes)-1))].id
		}
	}
}

// reveal scrolls the pane so the box with that id, borders included, is in view (its top when it can't all fit).
func (m *model) reveal(id string) {
	for _, b := range m.boxes() {
		if b.id == id {
			if hi := b.first + b.cap + 1; hi > m.scroll+m.bodyRows() {
				m.scroll = hi - m.bodyRows()
			}
			m.scroll = min(m.scroll, b.first-1)
		}
	}
	m.clampScroll()
}

// with returns a copy of mp with k set: models are values, so a shared map would leak state between copies.
func with[K comparable, V any](mp map[K]V, k K, v V) map[K]V {
	c := make(map[K]V, len(mp)+1)
	maps.Copy(c, mp)
	c[k] = v
	return c
}

// box returns the box with that id, if drawn.
func (m model) box(id string) (box, bool) { return m.layout().box(id) }

// scrollbar is one glyph per row of a cap-row window over total lines starting at top: a dim track and a thumb sized
// and placed in proportion, touching the bottom at the last page. total <= cap gives an all-thumb bar.
func scrollbar(cap, total, top int) []string {
	thumb := cap
	if total > cap {
		thumb = max(1, min(cap, int(float64(cap*cap)/float64(total)+0.5)))
	}
	pos := 0
	if total > cap {
		pos = int(float64(top)*float64(cap-thumb)/float64(total-cap) + 0.5)
		pos = max(0, min(pos, cap-thumb))
	}
	bar := make([]string, cap)
	for i := range bar {
		bar[i] = "│"
		if i >= pos && i < pos+thumb {
			bar[i] = "┃"
		}
	}
	return bar
}

// frame draws the visible slice of a long result as a scrollable panel: rounded border, scrollbar in the last
// inner column, and the line range in the bottom border. Its width is dw-2, the detail wrap width.
// colors, when given, is each row's foreground (the diff box's, so wrapped continuations match their line).
func (m model) frame(rows []string, top, total int, sel bool, colors []lipgloss.TerminalColor) string {
	_, _, dw := columns(m.w)
	outer, inner := dw-2, dw-6
	edge := lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Background(boxBg)
	if sel {
		edge = edge.Foreground(lipgloss.Color("#7aa2f7"))
	}
	txt := lipgloss.NewStyle().Background(boxBg)
	thumb := lipgloss.NewStyle().Foreground(lipgloss.Color("#7aa2f7")).Background(boxBg)
	bar := scrollbar(len(rows), total, top)
	out := []string{edge.Render("╭" + strings.Repeat("─", outer-2) + "╮")}
	for i, l := range rows {
		st := txt
		if l != ansi.Strip(l) { // the stdout/stderr label
			st = st.Bold(true)
		}
		l = ansi.Strip(l)
		if i < len(colors) {
			st = st.Foreground(colors[i])
		}
		bs := edge
		if bar[i] == "┃" {
			bs = thumb
		}
		out = append(out, edge.Render("│")+st.Render(" "+l+strings.Repeat(" ", max(0, inner-lipgloss.Width(l))))+bs.Render(bar[i])+edge.Render("│"))
	}
	foot := ansi.Truncate(fmt.Sprintf("lines %d–%d of %d", top+1, top+len(rows), total), outer-6, "…")
	out = append(out, edge.Render("╰─ "+foot+" "+strings.Repeat("─", max(0, outer-6-lipgloss.Width(foot)))+"─╯"))
	return strings.Join(out, "\n")
}

// scrollBox moves the box with that id by d lines, clamped to its text.
func (m *model) scrollBox(id string, d int) {
	b, ok := m.box(id)
	if !ok {
		return
	}
	m.boxTop = with(m.boxTop, id, max(0, min(b.top+d, len(b.lines)-b.cap)))
}

// boxAt reports which framed box has screen row y as one of its content rows (body line k is at buttonY+1+k-scroll).
// Border rows don't count. line is the box line drawn on that row.
func (m model) boxAt(y int) (id string, line int, ok bool) {
	l := m.layout()
	top := m.bodyTop(len(m.linesOf(l)))
	for _, b := range l.boxes {
		first := buttonY + 1 + b.first - top
		if y > buttonY && y >= first && y < first+b.cap && y <= max(5, m.h-3) {
			return b.id, b.top + y - first, true
		}
	}
	return "", 0, false
}

// bodyTop is the first detail line drawn: m.scroll, kept inside the n wrapped lines. View and boxAt share it.
func (m model) bodyTop(n int) int { return min(m.scroll, max(0, n-1)) }

// wrapDetail wraps s to the detail pane's text width.
func (m model) wrapDetail(s string) string {
	_, _, dw := columns(m.w)
	return lipgloss.NewStyle().Width(dw - 2).Render(s)
}

// detailLines is the wrapped detail, one entry per drawn line. View, boxAt and clampScroll share it.
func (m model) detailLines() []string { return m.linesOf(m.layout()) }

func (m model) linesOf(l layout) []string {
	return strings.Split(m.wrapDetail(strings.TrimSuffix(m.detailOf(l), "\n")), "\n")
}

// bodyRows is how many detail lines the pane draws: pane() keeps h lines, less the title and the button row.
func (m model) bodyRows() int { return max(5, m.h-3) - 2 }

// clampScroll keeps m.scroll within the detail.
func (m *model) clampScroll() {
	m.scroll = max(0, min(m.scroll, len(m.detailLines())-m.bodyRows()))
}

// wrapText wraps txt to a box width, tabs as four spaces.
func wrapText(txt string) func(int) []string {
	return func(w int) []string { return wrapLines(strings.ReplaceAll(txt, "\t", "    "), w) }
}

// memo caches pure work by key. Each wheel tick rebuilds the detail several times, and re-wrapping a long box
// or re-reading an attachment on every rebuild made fast scrolling fall seconds behind. Callers must not
// modify what it returns.
type memo[K comparable, V any] struct {
	sync.Mutex
	m map[K]V
}

func (c *memo[K, V]) get(k K, f func() V) V {
	c.Lock()
	defer c.Unlock()
	if v, ok := c.m[k]; ok {
		return v
	}
	if len(c.m) >= 8192 || c.m == nil { // shortcut: drops everything when full, fine while one item is on screen
		c.m = map[K]V{}
	}
	v := f()
	c.m[k] = v
	return v
}

type wrapKey struct {
	s string
	w int
}

var (
	wraps    memo[wrapKey, []string]
	attTexts memo[queue.FileHash, string]
)

// wrapLines is s wrapped to w cells, one entry per line.
func wrapLines(s string, w int) []string {
	return wraps.get(wrapKey{s, w}, func() []string {
		return strings.Split(strings.TrimSuffix(ansi.Wrap(s, w, ""), "\n"), "\n")
	})
}

// attText is an attachment's snapshot copy as shown: JSON pretty-printed, sanitized. Re-opening an item rewrites
// its snapshot in place, so the file's hash is part of the key.
func attText(f queue.FileHash) string {
	return attTexts.get(f, func() string {
		data, err := os.ReadFile(f.Path)
		if err != nil {
			return sanitize(err.Error())
		}
		var pretty bytes.Buffer
		if json.Indent(&pretty, data, "", "  ") == nil {
			data = pretty.Bytes()
		}
		return sanitize(string(data))
	})
}

// attBox is an attachment's box: the snapshot copy, JSON pretty-printed, sanitized. With attachment wrap off it is
// always framed, so it can be selected and panned, and its lines are cut at the pan offset (see cutLines).
func (m model) attBox(name string) box {
	id, txt := "file:"+name, m.data.atts[name]
	if m.settings.AttachmentWrap {
		return m.mkBox(id, wrapText(txt), m.capPct(30))
	}
	_, _, dw := columns(m.w)
	lines := strings.Split(strings.TrimSuffix(strings.ReplaceAll(txt, "\t", "    "), "\n"), "\n")
	b := box{id: id, framed: true, pan: true}
	b.lines, b.x = cutLines(lines, m.boxX[id], max(1, dw-6))
	b.cap = max(1, min(len(b.lines), m.capPct(30)))
	b.top = max(0, min(m.boxTop[id], len(b.lines)-b.cap))
	return b
}

// detailData is everything the detail shows from disk and the clock. loadData reads it when an item opens (so at
// each tick too) and after this viewer acts on an item; drawing reads only this. Never modified once set.
type detailData struct {
	state, expires string // the selected item's state, and "expires in" while pending
	invalid        bool   // the selection is an item that failed to load
	loadErr        string
	verified       bool   // the session has a hook record
	cwd            string // the session's, when verified
	res            *queue.Result
	resErr         string // the result is unreadable (not just missing)
	prompt         *queue.Prompt
	problems       []string
	badge          rules.Badge
	sizes          map[string]int    // attachment sizes by name
	atts           map[string]string // attachment text as shown, by name; not for edit items
}

// loadData re-reads the detail's data, keeping the old snapshot when nothing changed so caches keyed on it hold.
func (m *model) loadData() {
	d, s, now := &detailData{}, m.session(), time.Now()
	switch r := m.reviewed; {
	case s != nil && s.invalid != nil && m.ii < len(s.invalid):
		_, err := m.q.Load(s.invalid[m.ii])
		d.invalid, d.loadErr = true, fmt.Sprint(err)
	case r != nil:
		it := r.Item
		if d.state = m.q.State(it, now); d.state == queue.Pending {
			d.expires = strings.TrimSuffix(it.Expires.Sub(now).Round(time.Minute).String(), "0s")
		}
		project := it.Cwd
		if d.verified = s != nil && s.sess != nil; d.verified {
			d.cwd, project = s.sess.Cwd, s.sess.Cwd
			if pr, err := m.q.PromptBefore(it.SessionID, it.Created); err == nil {
				d.prompt = &pr
			}
		}
		settings := rules.Load(rules.DefaultFiles(m.home, project))
		d.problems, d.badge = settings.Problems, settings.Check(it.Argv)
		if res, err := m.q.ReadResult(it); err == nil {
			d.res = &res
		} else if !errors.Is(err, os.ErrNotExist) {
			d.resErr = err.Error()
		}
		d.sizes, d.atts = map[string]int{}, map[string]string{}
		for _, f := range r.FileHashes[1:] { // item.json, then the attachments in it.Files order
			if fi, err := os.Stat(f.Path); err == nil {
				d.sizes[f.Name] = int(fi.Size())
			}
			if it.Edit == "" { // an edit's attachment is shown as the Diff instead
				d.atts[f.Name] = attText(f)
			}
		}
	default:
		if it := m.item(); it != nil {
			d.state = m.q.State(*it, now)
		}
	}
	if !reflect.DeepEqual(d, m.data) {
		m.data = d
	}
}

// detailSegs is the detail in order: text runs and the Result, prompt and attachment boxes. It reads m.data, never
// the disk or the clock.
func (m model) detailSegs() []seg {
	d := m.data
	if d.invalid {
		return []seg{{text: warn.Render("This item failed to load and can't be run.") + "\n\n" + sanitize(d.loadErr)}}
	}
	if m.openErr != nil {
		return []seg{{text: warn.Render("Could not open item: ") + sanitize(m.openErr.Error())}}
	}
	if m.reviewed == nil {
		return []seg{{text: dim.Render("No items.")}}
	}
	it := m.reviewed.Item
	var b strings.Builder
	var segs []seg
	p := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	addBox := func(x box) {
		segs = append(segs, seg{text: b.String()}, seg{box: &x})
		b.Reset()
	}
	var atts []box
	for _, name := range it.Files {
		if it.Edit != "" { // its contents are shown as the Diff instead
			continue
		}
		atts = append(atts, m.attBox(name))
	}

	state := m.drawnState()
	chips := []string{chip(strings.ToUpper(state), resColor(state))}
	if mt := methodOf(it.Argv); mt != "" {
		c := ""
		switch mt {
		case "GET":
			c = "2"
		case "PUT", "POST", "PATCH", "DELETE":
			c = "1"
		}
		chips = append(chips, chip(sanitize(mt), c))
	}
	verified := d.verified
	if verified {
		chips = append(chips, ok.Render("✓ verified"))
	} else {
		chips = append(chips, warn.Render("⚠ unverified session"))
	}
	if state == queue.Pending {
		chips = append(chips, dim.Render("expires in "+d.expires))
	}
	p("%s\n", strings.Join(chips, "  "))
	p("%s\n", bold.Render(sanitize(it.Description)))

	if m.changed {
		p("%s", warn.Render("Changed on disk after you opened it. Press o to review it again."))
	}
	if !verified {
		p("%s", warn.Render("⚠ unverified session "+sanitize(shortID(it.SessionID))+": no hook record, provenance unknown"))
	}
	if runner.RunsInterpreter(it.Argv) {
		p("%s", warn.Render(fmt.Sprintf("⚠ runs an interpreter (%s): its arguments are code", sanitize(it.Argv[0]))))
	}
	for _, pr := range d.problems {
		p("%s", warn.Render("settings problem: "+sanitize(pr)))
	}

	if res := d.res; res != nil {
		p("\n%s", heading("Result"))
		p("%s exit %d at %s", chip(strings.ToUpper(res.State), resColor(res.State)), res.ExitCode, res.RanAt.Format(time.Kitchen))
		addBox(m.mkBox(resultBox, func(w int) (out []string) {
			for _, st := range [][2]string{{"stdout", res.Stdout}, {"stderr", res.Stderr}} {
				if st[1] != "" {
					out = append(out, bold.Render(st[0]))
					out = append(out, wrapText(sanitize(st[1]))(w)...)
				}
			}
			return out
		}, m.capPct(40)))
	} else if d.resErr != "" {
		p("\n%s", heading("Result"))
		p("%s", warn.Render(sanitize(d.resErr)))
	}

	p("\n%s", heading("Command"))
	for _, l := range commandLines(it.Argv, d.sizes) {
		p("%s", l)
	}

	if it.Edit != "" {
		p("\n%s", heading("Diff"))
		if m.reviewed.Diff == "" {
			p("%s", dim.Render("no changes: the proposed file matches "+sanitize(it.Edit)))
		} else {
			if m.reviewed.BaseSHA256 == "" {
				p("%s", warn.Render("new file: "+sanitize(it.Edit)+" doesn't exist yet"))
			}
			addBox(m.diffBox())
		}
	}

	p("\n%s", heading("Why"))
	if verified {
		if pr := d.prompt; pr != nil {
			p("%s you said:", dim.Render(pr.TS.Format("15:04")))
			addBox(m.mkBox(promptBox, wrapText(sanitize(pr.Prompt)), m.capPct(20)))
		} else {
			p("%s", warn.Render("no prompt recorded"))
		}
		p("%s %s", ok.Render("✓ verified"), sanitize(it.SessionID))
		p("%s", dim.Render("session cwd ")+sanitize(d.cwd))
	}
	why := []string{}
	if it.Reason != "" {
		why = append(why, sanitize(it.Reason))
	}
	cwd := sanitize(it.Cwd)
	if m.home != "" && (cwd == m.home || strings.HasPrefix(cwd, m.home+"/")) {
		cwd = "~" + strings.TrimPrefix(cwd, m.home)
	}
	why = append(why, d.badge.String(m.home), cwd)
	p("%s", dim.Render(strings.Join(why, " · ")))

	for _, a := range atts {
		p("\n%s", heading("📎 "+sanitize(strings.TrimPrefix(a.id, "file:"))))
		addBox(a)
	}

	p("\n%s", heading("Integrity"))
	for _, f := range m.reviewed.FileHashes {
		name, path := sanitize(f.Name), sanitize(f.Path)
		if f.Name != "item.json" {
			name = "📎 " + name
		}
		if m.home != "" && strings.HasPrefix(path, m.home+"/") {
			path = "~" + strings.TrimPrefix(path, m.home)
		}
		p("%s\n%s", name, dim.Render(f.SHA256+"  "+path))
	}
	p("%s\n%s", dim.Render("combined (internal; not comparable to shasum)"), dim.Render(m.reviewed.Hash))

	return append(segs, seg{text: b.String()})
}

// resColor: pending yellow, succeeded green, anything else (failed, rejected) red.
func resColor(state string) string {
	switch state {
	case queue.Pending:
		return "3"
	case queue.Succeeded:
		return "2"
	}
	return "1"
}

func shortID(id string) string { return id[:min(8, len(id))] }
