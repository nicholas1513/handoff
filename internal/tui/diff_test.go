package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/nicholas1513/handoff/internal/queue"
)

// editModel stages an edit of a 40-line file changing lines 5 and 35, and opens it at w x h.
func editModel(t *testing.T, w, h int, change func(i int, l string) string) (model, []string) {
	t.Helper()
	_, m := setup(t)
	dir := t.TempDir()
	var old, neu []string
	for i := 1; i <= 40; i++ {
		l := fmt.Sprintf("line %d", i)
		old, neu = append(old, l), append(neu, change(i, l))
	}
	target, proposed := filepath.Join(dir, "f.txt"), filepath.Join(dir, "new.txt")
	os.WriteFile(target, []byte(strings.Join(old, "\n")+"\n"), 0o600)
	os.WriteFile(proposed, []byte(strings.Join(neu, "\n")+"\n"), 0o600)
	m = withItem(t, m, queue.Item{Description: "x", Edit: target, Argv: queue.EditArgv(target)},
		map[string]string{queue.EditFile: proposed}, w)
	next, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	return next.(model), neu
}

func twoChanges(i int, l string) string {
	if i == 5 || i == 35 {
		return fmt.Sprintf("changed %d", i)
	}
	return l
}

func acts(rows []dline) (out []string) {
	for _, r := range rows {
		if r.act != nil {
			out = append(out, r.text)
		}
	}
	return
}

func TestDiffGapsExpandLikeGitHub(t *testing.T) {
	m, neu := editModel(t, 160, 60, twoChanges)
	// gap 0 is 1 line, gap 1 (lines 9-31) is 23, gap 2 is 2: small gaps get one row, the big one a ↓ and an ↑.
	want := []string{"[+] ↕ show 1 line", "[+] ↓ show 10 lines · 23 hidden", "[+] ↑ show 10 lines · 23 hidden", "[+] ↕ show 2 lines"}
	if got := acts(m.diff.rows(m.diffExp)); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("expander rows = %q, want %q", got, want)
	}

	for _, r := range m.diff.rows(m.diffExp) {
		if r.act != nil && strings.Contains(r.text, "↓") {
			m.expandDiff(r.act)
			break
		}
	}
	text := func() string {
		var b strings.Builder
		for _, r := range m.diff.rows(m.diffExp) {
			b.WriteString(r.text + "\n")
		}
		return b.String()
	}
	if !strings.Contains(text(), " line 9\n") || !strings.Contains(text(), " line 18\n") || strings.Contains(text(), " line 19\n") {
		t.Fatalf("↓ should reveal lines 9-18 under the first hunk:\n%s", text())
	}
	if got := acts(m.diff.rows(m.diffExp)); !strings.Contains(strings.Join(got, "|"), "13 hidden") {
		t.Fatalf("after one ↓, rows = %q", got)
	}

	// Expanding everything ends with no [+] rows and the whole proposed file in order.
	for i := 0; i < 10 && len(acts(m.diff.rows(m.diffExp))) > 0; i++ {
		m.expandDiff(nil)
	}
	var got []string
	for _, r := range m.diff.rows(m.diffExp)[2:] { // after the ---/+++ head
		if r.act != nil || strings.HasPrefix(r.text, "@@") {
			t.Fatalf("left over row %q", r.text)
		}
		if !strings.HasPrefix(r.text, "-") {
			got = append(got, r.text[1:])
		}
	}
	if strings.Join(got, "\n") != strings.Join(neu, "\n") {
		t.Fatalf("fully expanded diff isn't the proposed file:\n%s", strings.Join(got, "\n"))
	}
}

func TestParseDiffFallsBackToRaw(t *testing.T) {
	for name, c := range map[string][2]string{
		"binary":         {"Binary files a and b differ\n", ""},
		"base too short": {"--- a\n+++ b\n@@ -5,3 +5,3 @@\n-x\n+y\n", "only\none\n"},
	} {
		if d := parseDiff(c[0], c[1]); d.raw == nil || len(d.hunks) != 0 {
			t.Errorf("%s: want the raw diff, got %+v", name, d)
		}
	}
}

func TestDiffWrapKeepsLineColour(t *testing.T) {
	long := strings.Repeat("word ", 60)
	m, _ := editModel(t, 120, 60, func(i int, l string) string {
		if i == 5 {
			return long
		}
		return l
	})
	b := m.diffBox()
	green := diffColor("+")
	n := 0
	for i, l := range b.lines {
		if b.meta[i].text == "+"+long {
			n++
			if b.meta[i].color != green {
				t.Errorf("wrapped part %q of an added line isn't green", l)
			}
		}
	}
	if n < 2 {
		t.Fatalf("the long line should wrap into several drawn lines, got %d", n)
	}
	// and the colour reaches the screen on the continuation rows too
	trueColor(t)
	sgr, _, _ := strings.Cut(lipgloss.NewStyle().Foreground(green).Render("x"), "x")
	sgr = strings.TrimSuffix(sgr, "m") // the frame adds a background to the same sequence
	n = 0
	for _, l := range m.detailLines() {
		if s := ansi.Strip(l); strings.Contains(s, "word word") && !strings.Contains(s, "+word") {
			n++
			if !strings.Contains(l, sgr) {
				t.Errorf("continuation row drawn without colour: %q", l)
			}
		}
	}
	if n == 0 {
		t.Fatal("no continuation rows drawn")
	}
}

func TestDiffAlwaysFramedAndCapped(t *testing.T) {
	m, _ := editModel(t, 120, 40, func(i int, l string) string { return "new " + l }) // every line changes
	b := m.diffBox()
	if !b.framed || b.cap != m.capPct(50) || len(b.lines) <= b.cap {
		t.Fatalf("long diff: framed %v cap %d of %d lines, want framed and capped at %d", b.framed, b.cap, len(b.lines), m.capPct(50))
	}
	m2, _ := editModel(t, 120, 40, twoChanges)
	m2.expandDiff(nil)
	if b := m2.diffBox(); !b.framed {
		t.Fatal("short diff should still be framed")
	}
}

func TestDiffWrapOffCutsAndPans(t *testing.T) {
	long := "START" + strings.Repeat("x", 200) + "END"
	m, _ := editModel(t, 120, 60, func(i int, l string) string {
		if i == 5 {
			return long
		}
		return l
	})
	m.settings.DiffWrap = false
	_, _, dw := columns(m.w)
	find := func() string {
		b := m.diffBox()
		for i, l := range b.lines {
			if w := ansi.StringWidth(l); w > dw-6 {
				t.Fatalf("line %d is %d wide, over the box's %d", i, w, dw-6)
			}
			if b.meta[i].text == "+"+long {
				return l
			}
		}
		t.Fatal("long line missing")
		return ""
	}
	if l := find(); !strings.HasPrefix(l, "+START") || !strings.HasSuffix(l, "›") {
		t.Fatalf("unpanned: %q, want the start and a › marker", l)
	}
	next, _ := m.key(">")
	m = next.(model)
	if l := find(); !strings.HasPrefix(l, "‹") {
		t.Fatalf("panned: %q, want a ‹ marker", l)
	}
	for i := 0; i < 20; i++ {
		next, _ = m.key(">")
		m = next.(model)
	}
	if l := find(); !strings.HasSuffix(l, "END") {
		t.Fatalf("panned to the end: %q", l)
	}
}

func TestAttachmentWrapIsSeparateFromDiffWrap(t *testing.T) {
	_, m := setup(t)
	long := "START" + strings.Repeat("y", 300) + "END"
	att := filepath.Join(t.TempDir(), "notes.txt")
	os.WriteFile(att, []byte("short\n"+long+"\n"), 0o600)
	m = withItem(t, m, queue.Item{Description: "x", Argv: []string{"/bin/cat", "{file:notes.txt}"}},
		map[string]string{"notes.txt": att}, 120)
	_, _, dw := columns(m.w)
	id := "file:notes.txt"

	m.settings.DiffWrap = false // the diff setting must not touch attachments
	if b, _ := m.box(id); b.pan || !strings.Contains(strings.Join(b.lines, ""), "END") {
		t.Fatalf("attachment wrap on: pan %v, want the whole line wrapped into view", b.pan)
	}

	m.settings.DiffWrap, m.settings.AttachmentWrap = true, false
	b, _ := m.box(id)
	if !b.pan || !b.framed {
		t.Fatalf("attachment wrap off: pan %v framed %v, want a framed, pannable box", b.pan, b.framed)
	}
	for _, l := range b.lines {
		if ansi.StringWidth(l) > dw-6 {
			t.Fatalf("line %q is wider than the box", l)
		}
	}
	if l := b.lines[1]; !strings.HasPrefix(l, "START") || !strings.HasSuffix(l, "›") {
		t.Fatalf("unpanned long line = %q", l)
	}
	if m.panTarget() != id {
		t.Fatalf("pan target = %q, want the attachment", m.panTarget())
	}
	for i := 0; i < 20; i++ {
		next, _ := m.key(">")
		m = next.(model)
	}
	if b, _ := m.box(id); !strings.HasPrefix(b.lines[1], "‹") || !strings.HasSuffix(b.lines[1], "END") {
		t.Fatalf("panned to the end: %q", b.lines[1])
	}
	next, _ := m.key("<") // clamped on the way out, so one step back moves at once
	if b, _ := next.(model).box(id); strings.HasSuffix(b.lines[1], "END") {
		t.Fatalf("one < after over-panning should move: %q", b.lines[1])
	}
}

func TestOlderSettingsFileKeepsNewDefaults(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(settingsPath(root), []byte(`{"diff_wrap": false}`), 0o600)
	if s, err := loadSettings(root); err != nil || s.DiffWrap || !s.AttachmentWrap {
		t.Fatalf("settings = %+v, %v; want diff off as saved and attachment wrap at its default (on)", s, err)
	}
}

func TestClickPlusRowExpands(t *testing.T) {
	m, _ := editModel(t, 160, 60, twoChanges)
	before := len(m.diff.rows(m.diffExp))
	var y int
	for _, p := range m.boxes() {
		if p.id != diffBox {
			continue
		}
		for i := p.top; i < p.top+p.cap; i++ {
			if p.meta[i].act != nil && strings.Contains(p.meta[i].text, "↓") {
				y = buttonY + 1 + p.first - m.bodyTop(len(m.detailLines())) + i - p.top
			}
		}
	}
	if y == 0 {
		t.Fatal("no ↓ row on screen")
	}
	sw, iw, _ := columns(m.w)
	m, _ = click(m, sw+iw+5, y)
	if after := len(m.diff.rows(m.diffExp)); after != before+10 {
		t.Fatalf("rows %d -> %d, want 10 more", before, after)
	}
}

func TestSettingsOverlay(t *testing.T) {
	m, _ := editModel(t, 160, 60, twoChanges)
	if !m.settings.DiffWrap {
		t.Fatal("wrap should default on")
	}
	if status, _ := m.footer(); !strings.Contains(ansi.Strip(status), settingsKey) {
		t.Fatalf("footer lacks %q: %q", settingsKey, ansi.Strip(status))
	}

	// footer click opens it
	_, x := m.footer()
	m, _ = click(m, x+1, m.statusY())
	if !m.menu || !strings.Contains(ansi.Strip(m.View()), "Diff word wrap") {
		t.Fatal("clicking the footer hint should open the settings overlay")
	}
	// clicking the toggle row flips and saves
	_, ox, oy := m.overlay()
	m, _ = click(m, ox+3, oy+settingsRow)
	if m.settings.DiffWrap || !m.menu {
		t.Fatalf("toggle click: wrap %v menu %v", m.settings.DiffWrap, m.menu)
	}
	if s, err := loadSettings(m.q.Root); err != nil || s.DiffWrap {
		t.Fatalf("saved settings = %+v, %v", s, err)
	}
	if newModel(m.q, m.home).settings.DiffWrap {
		t.Fatal("a new viewer should load the saved setting")
	}
	// keys: space toggles, other keys don't leak through, esc closes
	next, _ := m.key(" ")
	m = next.(model)
	next, _ = m.key("x") // would reject the item if the overlay let it through
	m = next.(model)
	if !m.settings.DiffWrap || !m.pending() {
		t.Fatalf("space should toggle back and x must not reach the item: wrap %v pending %v", m.settings.DiffWrap, m.pending())
	}
	next, _ = m.key("esc")
	if m = next.(model); m.menu {
		t.Fatal("esc should close the overlay")
	}
	// click outside closes; s opens
	next, _ = m.key("s")
	m = next.(model)
	m, _ = click(m, 0, 0)
	if m.menu {
		t.Fatal("a click outside should close the overlay")
	}

	// the second row is attachment wrap: ↓ then space, or a click on its row, flips only it
	next, _ = m.key("s")
	m = next.(model)
	for _, k := range []string{"down", " "} {
		next, _ = m.key(k)
		m = next.(model)
	}
	if m.settings.AttachmentWrap || !m.settings.DiffWrap {
		t.Fatalf("↓ space: diff %v attachment %v, want only attachment wrap off", m.settings.DiffWrap, m.settings.AttachmentWrap)
	}
	if !strings.Contains(ansi.Strip(m.View()), "› Attachment word wrap") {
		t.Fatal("the selected row should be marked")
	}
	_, ox, oy = m.overlay()
	m, _ = click(m, ox+3, oy+settingsRow+1)
	if !m.settings.AttachmentWrap || !m.settings.DiffWrap {
		t.Fatalf("row click: diff %v attachment %v", m.settings.DiffWrap, m.settings.AttachmentWrap)
	}
	if s, _ := loadSettings(m.q.Root); !s.AttachmentWrap || !s.DiffWrap {
		t.Fatalf("saved settings = %+v", s)
	}
}
