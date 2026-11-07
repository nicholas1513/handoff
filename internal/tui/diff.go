package tui

import (
	"encoding/json"
	"errors"
	"fmt"
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

var hunkRe = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+\d+(?:,(\d+))? @@`)

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
			add("[+] ↕ show "+count(hidden, "line"), expandColor, &expand{gap: i, top: hidden})
		default:
			if !first {
				add("[+] ↓ show "+count(expandStep, "line")+" · "+strconv.Itoa(hidden)+" hidden", expandColor, &expand{gap: i, top: expandStep})
			}
			if !last {
				add("[+] ↑ show "+count(expandStep, "line")+" · "+strconv.Itoa(hidden)+" hidden", expandColor, &expand{gap: i, bottom: expandStep})
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

func count(n int, what string) string {
	if n == 1 {
		return "1 " + what
	}
	return strconv.Itoa(n) + " " + what + "s"
}

// hunkLen is a hunk header's old and new line counts.
func hunkLen(l string) (old, neu int, ok bool) {
	m := hunkRe.FindStringSubmatch(l)
	if m == nil {
		return 0, 0, false
	}
	n := func(s string) int {
		if s == "" {
			return 1
		}
		v, _ := strconv.Atoi(s)
		return v
	}
	return n(m[2]), n(m[3]), true
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

// diffBox is the edit's diff, revealed context included.
func (m model) diffBox() box {
	return m.diffLike(diffBox, diffLines(m.diff.rows(m.diffExp), m.diffWrap(), false))
}

// diffWrap is the width diff-like boxes wrap to, 0 with diff word wrap off.
func (m model) diffWrap() int {
	if !m.settings.DiffWrap {
		return 0
	}
	_, _, dw := columns(m.w)
	return max(1, dw-6)
}

// diffLines is rows as a box's lines, wrapped to wrap cells (0: not wrapped). Each line keeps its row in meta, so
// wrapped continuations keep their colour and clicks land on the right row. hard wraps by cell and keeps every space,
// so a wrapped line's parts rejoin to the line git wrote: word wrap drops the space at each break.
func diffLines(rows []dline, wrap int, hard bool) (b box) {
	for _, r := range rows {
		t := shownDiff(r.text)
		parts := []string{t}
		switch {
		case wrap > 0 && hard:
			parts = strings.Split(ansi.Hardwrap(t, wrap, true), "\n")
		case wrap > 0:
			parts = wrapLines(t, wrap)
		}
		for _, p := range parts {
			b.lines, b.meta = append(b.lines, p), append(b.meta, r)
		}
	}
	return b
}

// diffLike is b, from diffLines, in an always-framed box capped at half the window, so it scrolls instead of growing
// the pane. With wrap off, lines are cut to the window at the box's pan offset (see cutLines).
func (m model) diffLike(id string, b box) box {
	b.id, b.framed = id, true
	if !m.settings.DiffWrap {
		_, _, dw := columns(m.w)
		b.lines, b.x = cutLines(b.lines, m.boxX[id], max(1, dw-6))
		b.pan = true
	}
	b.cap = max(1, min(len(b.lines), m.capPct(50)))
	return m.atTop(b)
}

// shownDiff is a diff row as drawn: sanitized (the proposal is agent-written), tabs as four spaces.
func shownDiff(s string) string { return strings.ReplaceAll(sanitize(s), "\t", "    ") }

// cutLines shows unwrapped lines through a window inner cells wide starting at column x, which it clamps to the
// widest line and returns. ‹ and › mark lines that go on past either edge, so a cut never hides text silently.
func cutLines(lines []string, x, inner int) ([]string, int) {
	widest := 0
	for _, l := range lines {
		widest = max(widest, ansi.StringWidth(l))
	}
	x = max(0, min(x, widest-inner))
	out := make([]string, len(lines))
	for i, l := range lines {
		w := ansi.StringWidth(l)
		l = ansi.Cut(l, x, x+inner)
		if x > 0 && l != "" {
			l = "‹" + ansi.TruncateLeft(l, 1, "")
		}
		if w > x+inner {
			l = ansi.Truncate(l, inner-1, "") + "›"
		}
		out[i] = l
	}
	return out, x
}

// viewerSettings are the reviewer's choices, kept in <root>/viewer.json. The sandbox can write only
// <root>/queue, so an agent can't change them.
type viewerSettings struct {
	DiffWrap       bool `json:"diff_wrap"`
	AttachmentWrap bool `json:"attachment_wrap"`
	// how an am item's commits are reviewed: "vscode", "patch" (drawn here), or "" for auto, which is resolved at
	// each start and never saved, so installing or removing VS Code changes it
	CommitReview string `json:"commit_review,omitempty"`
}

// commitReview is the setting's choice, else "vscode" when VS Code is installed, else "patch".
func commitReview(s viewerSettings, vscode bool) string {
	switch {
	case s.CommitReview == "vscode" || s.CommitReview == "patch":
		return s.CommitReview
	case vscode:
		return "vscode"
	}
	return "patch"
}

// settingRows are the overlay's rows, in display order: a setting's value as drawn, and a toggle of it. vscode is
// whether VS Code is installed, which auto resolves by.
var settingRows = []struct {
	label  string
	value  func(s viewerSettings, vscode bool) string
	toggle func(s *viewerSettings, vscode bool)
}{
	{"Diff word wrap", func(s viewerSettings, _ bool) string { return onOff(s.DiffWrap) },
		func(s *viewerSettings, _ bool) { s.DiffWrap = !s.DiffWrap }},
	{"Attachment word wrap", func(s viewerSettings, _ bool) string { return onOff(s.AttachmentWrap) },
		func(s *viewerSettings, _ bool) { s.AttachmentWrap = !s.AttachmentWrap }},
	{"Commit review", func(s viewerSettings, vscode bool) string {
		v := map[string]string{"vscode": "[ VS Code ]", "patch": "[ viewer ]"}[commitReview(s, vscode)]
		if commitReview(s, vscode) != s.CommitReview {
			v += " (auto)"
		}
		return v
	}, func(s *viewerSettings, vscode bool) {
		s.CommitReview = map[string]string{"vscode": "patch", "patch": "vscode"}[commitReview(*s, vscode)]
		if s.CommitReview == commitReview(viewerSettings{}, vscode) { // back to what auto picks: auto again
			s.CommitReview = ""
		}
	}},
}

func onOff(on bool) string { return map[bool]string{true: "[ on ] ", false: "[ off ]"}[on] }

func settingsPath(root string) string { return filepath.Join(root, "viewer.json") }

// loadSettings returns the defaults when there is no file yet, and the defaults plus the error for a bad one.
func loadSettings(root string) (viewerSettings, error) {
	s := viewerSettings{DiffWrap: true, AttachmentWrap: true} // a field missing from an older file keeps its default
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

// settingsRow is the overlay's first toggle row, counted from the overlay's top border; toggle i is on settingsRow+i.
const settingsRow = 3

// overlay is the settings menu as drawn, and where it sits (top-left cell) when centred in the window.
func (m model) overlay() (lines []string, x, y int) {
	rows := []string{section.Render("Settings"), ""}
	for i, r := range settingRows {
		mark := "  "
		if i == m.menuSel {
			mark = "› "
		}
		rows = append(rows, mark+fmt.Sprintf("%-22s", r.label)+bold.Render(r.value(m.settings, m.vscode != nil)))
	}
	body := strings.Join(append(rows, "", dim.Render("↑↓ select · space / click toggle · esc close")), "\n")
	lines = strings.Split(focused.Padding(0, 1).Render(body), "\n")
	w := lipgloss.Width(lines[0])
	return lines, max(0, (m.w-w)/2), max(0, (m.h-len(lines))/2)
}

// withOverlay draws an overlay over view at x, y, keeping the view visible around it.
func withOverlay(view string, ol []string, x, y int) string {
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
