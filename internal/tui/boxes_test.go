package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"github.com/nicholas1513/handoff/internal/queue"
)

const blueSGR = "38;2;121;162;247" // #7aa2f7 as termenv emits it under TrueColor (it rounds 122 to 121)

func trueColor(t *testing.T) {
	old := lipgloss.ColorProfile()
	t.Cleanup(func() { lipgloss.SetColorProfile(old) })
	lipgloss.SetColorProfile(termenv.TrueColor)
}

// withAtt stages a pending item after a prompt of pn lines (when pn > 0) with body.json of an keys (an+2 lines once
// pretty-printed), finished with rn lines of stdout when rn > 0, and returns the model on it at height h, focus 2.
func withAtt(t *testing.T, m model, pn, rn, an, h int) model {
	t.Helper()
	if pn > 0 {
		var p strings.Builder
		for i := 1; i <= pn; i++ {
			fmt.Fprintf(&p, "p%03d\n", i)
		}
		m.q.AddPrompt(queue.Prompt{SessionID: sid, TS: time.Now(), Prompt: strings.TrimSuffix(p.String(), "\n")})
	}
	var b strings.Builder
	for i := 1; i <= an; i++ {
		fmt.Fprintf(&b, `"k%03d":%d,`, i, i)
	}
	src := filepath.Join(t.TempDir(), "body.json")
	os.WriteFile(src, []byte("{"+strings.TrimSuffix(b.String(), ",")+"}"), 0o600)
	m = withItem(t, m, queue.Item{Description: "att", Argv: []string{"az", "rest", "--body", "@{file:body.json}"}}, map[string]string{"body.json": src}, 120)
	if rn > 0 {
		var o strings.Builder
		for i := 1; i <= rn; i++ {
			fmt.Fprintf(&o, "out%03d\n", i)
		}
		it := *m.item()
		m.q.Claim(it)
		m.q.WriteResult(it, queue.Result{State: queue.Succeeded, RanAt: time.Now(), Stdout: o.String()})
		m = tickM(m) // drawing reads snapshots: a result written after open shows at the next tick
	}
	next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: h})
	m = next.(model)
	m.focus, m.advancedAt = 2, time.Time{}
	return m
}

type frm struct {
	top, total int
	sel        bool
}

// frames reads every framed box in display order from the drawn detail: its first line (from the footer), total
// lines, and whether its top border is drawn in the selection blue.
func frames(m model) (out []frm) {
	sel := false
	for _, l := range strings.Split(m.detail(), "\n") {
		s := ansi.Strip(l)
		if strings.HasPrefix(s, "╭") {
			sel = strings.Contains(l, blueSGR)
		}
		if i := strings.Index(s, "lines "); strings.HasPrefix(s, "╰") && i >= 0 {
			var a, b, n int
			fmt.Sscanf(s[i:], "lines %d–%d of %d", &a, &b, &n)
			out = append(out, frm{a - 1, n, sel})
		}
	}
	return
}

func TestAttachmentBoxed(t *testing.T) {
	_, m := setup(t)
	m = withAtt(t, m, 0, 0, 198, 40)
	d := ansi.Strip(m.detail())
	if f := frames(m); len(f) != 1 || f[0].total != 200 || !strings.Contains(d, "lines 1–12 of 200") {
		t.Fatalf("attachment not boxed at 30%%: %+v", f)
	}
	keyRe := regexp.MustCompile(`"k(\d{3})"`)
	seen := map[string]bool{}
	first, last := false, false
	for i := 0; i < 30; i++ {
		for _, l := range strings.Split(ansi.Strip(m.detail()), "\n") {
			if !strings.HasPrefix(l, "│") {
				continue
			}
			c := strings.TrimSpace(strings.Trim(l, "│┃ "))
			first, last = first || c == "{", last || c == "}"
			for _, k := range keyRe.FindAllStringSubmatch(l, -1) {
				seen[k[1]] = true
			}
		}
		m, _ = press(m, "]")
	}
	for i := 1; i <= 198; i++ {
		if !seen[fmt.Sprintf("%03d", i)] {
			t.Fatalf("line k%03d never reachable", i)
		}
	}
	if !first || !last {
		t.Fatalf("first/last line reachable: %v %v", first, last)
	}
}

func TestCommandNeverBoxed(t *testing.T) {
	_, m := setup(t)
	argv := []string{"cmd"}
	for i := 1; i < 300; i++ {
		argv = append(argv, fmt.Sprintf("a%03d", i))
	}
	m = withItem(t, m, queue.Item{Description: "big", Argv: argv}, nil, 120)
	next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = next.(model)
	d := ansi.Strip(m.detail())
	if strings.Contains(d, "╭") {
		t.Fatal("a box drawn for a command-only item")
	}
	for _, l := range strings.Split(d, "\n") {
		if strings.ContainsAny(l[:min(3, len(l))], "│╭╰") {
			t.Fatalf("framed line %q", l)
		}
	}
	f := flatten(m.detail())
	for _, a := range argv {
		if !strings.Contains(f, a) {
			t.Fatalf("argv element %q missing", a)
		}
	}
}

func TestSelectedBoxHighlighted(t *testing.T) {
	trueColor(t)
	_, m := setup(t)
	m = withAtt(t, m, 100, 200, 198, 40)
	f := frames(m)
	if len(f) != 3 || !f[0].sel || f[1].sel || f[2].sel {
		t.Fatalf("default selection: %+v", f)
	}
	m, _ = press(m, "}")
	if f = frames(m); f[0].sel || !f[1].sel || f[2].sel {
		t.Fatalf("after }: %+v", f)
	}
	m, _ = press(m, "}")
	m, _ = press(m, "}") // past the last: stays there
	if f = frames(m); f[1].sel || !f[2].sel {
		t.Fatalf("after } } }: %+v", f)
	}
	for i := 0; i < 4; i++ {
		m, _ = press(m, "{")
	}
	if f = frames(m); !f[0].sel || f[2].sel {
		t.Fatalf("after {: %+v", f)
	}
	if v := ansi.Strip(m.View()); !strings.Contains(v, "[ ] scroll · { } next box") {
		t.Fatal("status hint missing")
	}
}

func TestBracketsScrollSelectedBoxOnly(t *testing.T) {
	_, m := setup(t)
	m = withAtt(t, m, 100, 200, 198, 40)
	m, _ = press(m, "}")
	m, _ = press(m, "]")
	if f := frames(m); len(f) != 3 || f[0].top != 0 || f[1].top != 8 || f[2].top != 0 {
		t.Fatalf("] after }: %+v", f)
	}
	m, _ = press(m, "}")
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyPgDown})
	m = next.(model)
	if f := frames(m); f[0].top != 0 || f[1].top != 8 || f[2].top != 12 {
		t.Fatalf("pgdown on the attachment: %+v", f)
	}
	m, _ = press(m, "[")
	if f := frames(m); f[2].top != 0 {
		t.Fatalf("[ on the attachment: %+v", f)
	}
}

func TestWheelSelectsAndScrollsBoxUnderIt(t *testing.T) {
	trueColor(t)
	_, m := setup(t)
	m = withPrompt(t, m, 100, 200, 100) // tall enough that both boxes are on screen
	sw, iw, _ := columns(m.w)
	wheel := func(y int) model {
		n, _ := m.Update(tea.MouseMsg{X: sw + iw + 5, Y: y, Action: tea.MouseActionPress, Button: tea.MouseButtonWheelDown})
		return n.(model)
	}
	oy, py := -1, -1
	for i, l := range strings.Split(ansi.Strip(m.View()), "\n") {
		if oy < 0 && strings.Contains(l, "│ stdout") {
			oy = i
		}
		if py < 0 && strings.Contains(l, "p001") {
			py = i
		}
	}
	if oy < 0 || py < 0 {
		t.Fatalf("boxes not drawn: %d %d", oy, py)
	}
	if f := frames(wheel(py)); f[1].top != 1 || !f[1].sel || f[0].top != 0 || f[0].sel {
		t.Fatalf("prompt row: %+v", f)
	}
	if f := frames(wheel(oy)); f[0].top != 1 || !f[0].sel || f[1].top != 0 {
		t.Fatalf("result row: %+v", f)
	}
	for _, y := range []int{oy - 1, oy + 40, py - 1, py + 20} { // border rows (caps 40% and 20% of 100)
		if f := frames(wheel(y)); f[0].top != 0 || f[1].top != 0 || !f[0].sel {
			t.Fatalf("border row %d moved or selected a box: %+v", y, f)
		}
	}
}

func TestBoxStateResetsOnNewItem(t *testing.T) {
	trueColor(t)
	_, m := setup(t)
	m = withAtt(t, m, 100, 200, 198, 40)
	id := m.item().ID
	m, _ = press(m, "}")
	m, _ = press(m, "}")
	for i := 0; i < 30; i++ {
		m, _ = press(m, "]") // ] keeps the selected box in view, so its last row is really drawn
	}
	if !lastDrawn(m) {
		t.Fatal("last attachment row never drawn")
	}
	if f := frames(m); len(f) != 3 || !f[2].sel || f[2].top == 0 {
		t.Fatalf("setup: %+v", f)
	}
	m.focus = 1
	m, _ = press(m, "k")
	m, _ = press(m, "j")
	if m.item().ID != id {
		t.Fatal("did not return to the item")
	}
	if f := frames(m); len(f) != 3 || !f[0].sel || f[1].sel || f[2].sel || f[0].top+f[1].top+f[2].top != 0 {
		t.Fatalf("after reopening: %+v", f)
	}
}

// TestWideGlyphsStayInsideTheBox: wrapping leaves some emoji runs wider than a box line; they are cut at the box's
// edge instead of pushing the frame out.
func TestWideGlyphsStayInsideTheBox(t *testing.T) {
	q, m := setup(t)
	it := addItem(t, q, queue.Item{Description: "wide", SessionID: sid}, nil)
	finished(t, q, it, queue.Succeeded, strings.Repeat(strings.Repeat("😀👍🏽👨‍👩‍", 3)+"\n", 60))
	m = resize(pick(t, tickM(m), "wide"), 120, 40)
	rows := strings.Split(ansi.Strip(m.View()), "\n")
	for i, l := range rows[:len(rows)-1] {
		if w := ansi.StringWidth(l); w != 120 {
			t.Fatalf("row %d is %d wide: %q", i, w, l)
		}
	}
}

// lastDrawn reports whether body.json's last line, the closing brace, is drawn inside its frame on screen.
func lastDrawn(m model) bool { return strings.Contains(ansi.Strip(m.View()), "││ }") }
