package tui

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// expandStep is how many hidden context lines one click on a [+] row reveals.
const expandStep = 10

// hunk is one @@ block of a unified diff: the old-file lines it covers, [begin, end) 1-based, and its body.
type hunk struct {
	header     string
	begin, end int
	lines      []string
}

// fileDiff is an edit item's diff, parsed so the unchanged lines between hunks can be revealed from base.
// raw is set instead when the diff can't be parsed against base (binary files, a missing base): shown as is.
type fileDiff struct {
	head  []string // the ---/+++ lines
	hunks []hunk
	base  []string
	raw   []string
}

var hunkRe = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+\d+(?:,\d+)? @@`)

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

func parseDiff(diff, base string) fileDiff {
	lines := splitLines(diff)
	d := fileDiff{base: splitLines(base)}
	for i, l := range lines {
		switch m := hunkRe.FindStringSubmatch(l); {
		case m != nil:
			start, _ := strconv.Atoi(m[1])
			n := 1
			if m[2] != "" {
				n, _ = strconv.Atoi(m[2])
			}
			begin := start
			if n == 0 { // an empty range names the line before it
				begin++
			}
			prev := 1
			if len(d.hunks) > 0 {
				prev = d.hunks[len(d.hunks)-1].end
			}
			if begin < prev || begin+n-1 > len(d.base) {
				return fileDiff{raw: lines}
			}
			d.hunks = append(d.hunks, hunk{header: l, begin: begin, end: begin + n})
		case len(d.hunks) > 0:
			h := &d.hunks[len(d.hunks)-1]
			h.lines = append(h.lines, l)
		case i < 2 && (strings.HasPrefix(l, "--- ") || strings.HasPrefix(l, "+++ ")):
			d.head = append(d.head, l)
		default:
			return fileDiff{raw: lines}
		}
	}
	if len(d.hunks) == 0 {
		return fileDiff{raw: lines}
	}
	return d
}

// expand is what clicking a [+] row does: reveal top more lines under the hunk above gap, bottom more over the hunk below.
type expand struct{ gap, top, bottom int }

// dline is one logical row of the drawn diff.
type dline struct {
	text  string
	color lipgloss.TerminalColor
	act   *expand // nil: clicking does nothing
}

var expandColor = lipgloss.Color("#7aa2f7")

// rows lays the diff out with exp[gap] = {revealed under the hunk above, revealed over the hunk below}.
// Gap i is the unchanged run before hunk i; the last gap is the run after the last hunk.
func (d fileDiff) rows(exp map[int][2]int) []dline {
	var out []dline
	add := func(text string, c lipgloss.TerminalColor, act *expand) { out = append(out, dline{text, c, act}) }
	if d.raw != nil {
		for _, l := range d.raw {
			add(l, diffColor(l), nil)
		}
		return out
	}
	for _, l := range d.head {
		add(l, lipgloss.NoColor{}, nil)
	}
	ctx := func(lines []string) {
		for _, l := range lines {
			add(" "+l, lipgloss.NoColor{}, nil)
		}
	}
	for i := 0; i <= len(d.hunks); i++ {
		lo, hi := 1, len(d.base)+1
		if i > 0 {
			lo = d.hunks[i-1].end
		}
		if i < len(d.hunks) {
			hi = d.hunks[i].begin
		}
		gap := d.base[lo-1 : hi-1]
		top, bot := min(exp[i][0], len(gap)), exp[i][1]
		bot = min(bot, len(gap)-top)
		hidden := len(gap) - top - bot
		ctx(gap[:top])
		switch first, last := i == 0, i == len(d.hunks); {
		case hidden == 0:
		case hidden <= expandStep || (first && last):
			add("[+] ↕ show "+lines(hidden), expandColor, &expand{gap: i, top: hidden})
		default:
			if !first {
				add("[+] ↓ show "+lines(expandStep)+" · "+strconv.Itoa(hidden)+" hidden", expandColor, &expand{gap: i, top: expandStep})
			}
			if !last {
				add("[+] ↑ show "+lines(expandStep)+" · "+strconv.Itoa(hidden)+" hidden", expandColor, &expand{gap: i, bottom: expandStep})
			}
		}
		ctx(gap[len(gap)-bot:])
		if i < len(d.hunks) {
			if hidden > 0 { // a fully revealed gap joins the hunks, so the header would only interrupt
				add(d.hunks[i].header, diffColor("@@"), nil)
			}
			for _, l := range d.hunks[i].lines {
				add(l, diffColor(l), nil)
			}
		}
	}
	return out
}

func lines(n int) string {
	if n == 1 {
		return "1 line"
	}
	return strconv.Itoa(n) + " lines"
}

// expandAll applies every [+] row at once: the + key.
func (d fileDiff) expandAll(exp map[int][2]int) map[int][2]int {
	for _, r := range d.rows(exp) {
		if r.act != nil {
			exp = applyExpand(exp, *r.act)
		}
	}
	return exp
}

func applyExpand(exp map[int][2]int, a expand) map[int][2]int {
	v := exp[a.gap]
	return with(exp, a.gap, [2]int{v[0] + a.top, v[1] + a.bottom})
}

// diffBox is the diff in an always-framed box capped at half the window, so it scrolls instead of growing the
// pane. Each drawn line keeps its logical row in meta, so wrapped continuations keep their colour and clicks
// land on the right row. With wrap off, lines are cut to the window at m.diffX, marked ‹ › where they go on.
func (m model) diffBox() box {
	_, _, dw := columns(m.w)
	inner := max(1, dw-6)
	b := box{id: diffBox, framed: true}
	for _, r := range m.diff.rows(m.diffExp) {
		t := shownDiff(r.text)
		parts := []string{t}
		if m.settings.DiffWrap {
			parts = strings.Split(ansi.Wrap(t, inner, ""), "\n")
		}
		for _, p := range parts {
			b.lines, b.meta = append(b.lines, p), append(b.meta, r)
		}
	}
	if !m.settings.DiffWrap {
		x := max(0, min(m.diffX, m.diffWidest()-inner))
		for i, l := range b.lines {
			w := ansi.StringWidth(l)
			l = ansi.Cut(l, x, x+inner)
			if x > 0 && l != "" {
				l = "‹" + ansi.TruncateLeft(l, 1, "")
			}
			if w > x+inner {
				l = ansi.Truncate(l, inner-1, "") + "›"
			}
			b.lines[i] = l
		}
	}
	b.cap = max(1, min(len(b.lines), m.capPct(50)))
	b.top = max(0, min(m.boxTop[diffBox], len(b.lines)-b.cap))
	return b
}

// shownDiff is a diff row as drawn: sanitized (the proposal is agent-written), tabs as four spaces.
func shownDiff(s string) string { return strings.ReplaceAll(sanitize(s), "\t", "    ") }

// diffWidest is the widest diff row as drawn unwrapped.
func (m model) diffWidest() int {
	w := 0
	for _, r := range m.diff.rows(m.diffExp) {
		w = max(w, ansi.StringWidth(shownDiff(r.text)))
	}
	return w
}

// viewerSettings are the reviewer's choices, kept in <root>/viewer.json. The sandbox can write only
// <root>/queue, so an agent can't change them.
type viewerSettings struct {
	DiffWrap bool `json:"diff_wrap"`
}

func settingsPath(root string) string { return filepath.Join(root, "viewer.json") }

// loadSettings returns the defaults when there is no file yet, and the defaults plus the error for a bad one.
func loadSettings(root string) (viewerSettings, error) {
	s := viewerSettings{DiffWrap: true}
	b, err := os.ReadFile(settingsPath(root))
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err == nil {
		err = json.Unmarshal(b, &s)
	}
	return s, err
}

func saveSettings(root string, s viewerSettings) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err == nil {
		err = os.MkdirAll(root, 0o700)
	}
	if err == nil {
		err = os.WriteFile(settingsPath(root), b, 0o600)
	}
	return err
}

// settingsRow is the overlay's toggle row, counted from the overlay's top border.
const settingsRow = 3

// overlay is the settings menu as drawn, and where it sits (top-left cell) when centred in the window.
func (m model) overlay() (lines []string, x, y int) {
	state := "off"
	if m.settings.DiffWrap {
		state = "on"
	}
	body := strings.Join([]string{
		section.Render("Settings"),
		"",
		"Diff word wrap   " + bold.Render("[ "+state+" ]"),
		"",
		dim.Render("space / click toggle · esc close"),
	}, "\n")
	lines = strings.Split(focused.Padding(0, 1).Render(body), "\n")
	w := lipgloss.Width(lines[0])
	return lines, max(0, (m.w-w)/2), max(0, (m.h-len(lines))/2)
}

// withOverlay draws the overlay over view, keeping the view visible around it.
func (m model) withOverlay(view string) string {
	ol, x, y := m.overlay()
	vl := strings.Split(view, "\n")
	for i, l := range ol {
		if y+i >= len(vl) {
			break
		}
		bg := vl[y+i]
		vl[y+i] = ansi.Truncate(bg, x, "") + strings.Repeat(" ", max(0, x-ansi.StringWidth(bg))) + l +
			ansi.TruncateLeft(bg, x+lipgloss.Width(l), "")
	}
	return strings.Join(vl, "\n")
}
