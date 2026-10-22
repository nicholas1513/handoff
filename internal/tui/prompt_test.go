package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/nicholas1513/handoff/internal/queue"
)

// withPrompt stages an item right after a prompt of pn lines "p001".., finished with rn lines "out001".. when rn > 0,
// and returns the model on it at the given height.
func withPrompt(t *testing.T, m model, pn, rn, h int) model {
	t.Helper()
	var p strings.Builder
	for i := 1; i <= pn; i++ {
		fmt.Fprintf(&p, "p%03d\n", i)
	}
	m.q.AddPrompt(queue.Prompt{SessionID: sid, TS: time.Now(), Prompt: strings.TrimSuffix(p.String(), "\n")})
	it, err := m.q.Add(queue.Item{Description: "pr", Argv: []string{"/usr/bin/true"}, Cwd: t.TempDir(), SessionID: sid}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rn > 0 {
		var o strings.Builder
		for i := 1; i <= rn; i++ {
			fmt.Fprintf(&o, "out%03d\n", i)
		}
		m.q.Claim(it)
		m.q.WriteResult(it, queue.Result{State: queue.Succeeded, RanAt: time.Now(), Stdout: o.String()})
	}
	nm := newModel(m.q, m.home)
	for i, x := range nm.session().items {
		if x.ID == it.ID {
			nm.ii = i
		}
	}
	nm.open()
	next, _ := nm.Update(tea.WindowSizeMsg{Width: 120, Height: h})
	nm = next.(model)
	nm.focus = 2
	return nm
}

func promptFooter(m model) string {
	for _, l := range strings.Split(ansi.Strip(m.detail()), "\n") {
		if i := strings.Index(l, "lines "); i >= 0 && strings.Contains(l, " · { }") {
			return l[i:strings.Index(l, " · { }")]
		}
	}
	return ""
}

func TestPromptBoxFramedWhenLong(t *testing.T) {
	_, m := setup(t)
	m = withPrompt(t, m, 100, 0, 40)
	d := ansi.Strip(m.detail())
	if !strings.Contains(d, "lines 1–8 of 100 · { }") {
		t.Fatalf("prompt box not capped at 20%%:\n%s", d)
	}
	if n := strings.Count(d, "p0"); n != 8 {
		t.Fatalf("%d prompt lines drawn, want 8", n)
	}
	// the label, the verified id and the cwd line sit outside the box
	for _, want := range []string{"you said:", "✓ verified " + sid, "session cwd "} {
		i := strings.Index(d, want)
		if i < 0 {
			t.Fatalf("%q missing", want)
		}
		if line := d[strings.LastIndex(d[:i], "\n")+1:]; strings.ContainsAny(line[:min(3, len(line))], "│╭╰") {
			t.Fatalf("%q drawn inside the box", want)
		}
	}
	if strings.Index(d, "you said:") > strings.Index(d, "╭") {
		t.Fatal("label must sit above the box")
	}
	if !strings.Contains(ansi.Strip(m.View()), "{ } scroll prompt") {
		t.Fatal("status hint missing")
	}
}

func TestPromptBoxScrollKeys(t *testing.T) {
	_, m := setup(t)
	m = withPrompt(t, m, 100, 200, 40)
	m, _ = press(m, "}")
	if f := promptFooter(m); f != "lines 9–16 of 100" {
		t.Fatalf("after }: %q", f)
	}
	m, _ = press(m, "]")
	if f := promptFooter(m); f != "lines 9–16 of 100" || m.promptTop != 8 {
		t.Fatalf("] moved the prompt: %q", f)
	}
	m, _ = press(m, "{")
	if f := promptFooter(m); f != "lines 1–8 of 100" {
		t.Fatalf("after {: %q", f)
	}
	out := m.outTop
	m, _ = press(m, "{")
	if m.outTop != out || m.promptTop != 0 {
		t.Fatalf("{ moved outTop %d -> %d", out, m.outTop)
	}
	for i := 0; i < 40; i++ {
		m, _ = press(m, "}")
	}
	if f := promptFooter(m); f != "lines 93–100 of 100" {
		t.Fatalf("end: %q", f)
	}
	m.focus = 1
	m, _ = press(m, "}")
	if m.promptTop != 92 {
		t.Fatal("} acted outside focus 2")
	}
}

func TestShortPromptNotFramed(t *testing.T) {
	_, m := setup(t)
	m = withPrompt(t, m, 1, 0, 40)
	d := ansi.Strip(m.detail())
	if strings.Contains(d, "╭") || strings.Contains(d, "{ }") || !strings.Contains(d, "p001") {
		t.Fatal("short prompt must be plain")
	}
	if strings.Contains(ansi.Strip(m.View()), "scroll prompt") {
		t.Fatal("hint shown for a short prompt")
	}
}

func TestWheelScrollsTheBoxUnderIt(t *testing.T) {
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
	if g := wheel(oy); g.outTop != 3 || g.promptTop != 0 {
		t.Fatalf("result row: outTop %d promptTop %d", g.outTop, g.promptTop)
	}
	if g := wheel(py); g.promptTop != 3 || g.outTop != 0 {
		t.Fatalf("prompt row: outTop %d promptTop %d", g.outTop, g.promptTop)
	}
	for _, y := range []int{oy - 1, oy + m.outCap(), py - 1, py + m.promptCap()} {
		if g := wheel(y); g.outTop != 0 || g.promptTop != 0 {
			t.Fatalf("border row %d moved a box: %d %d", y, g.outTop, g.promptTop)
		}
	}
}

func TestPromptTopResetsOnNewItem(t *testing.T) {
	_, m := setup(t)
	m = withPrompt(t, m, 100, 0, 40)
	m, _ = press(m, "}")
	if m.promptTop == 0 {
		t.Fatal("did not scroll")
	}
	m.focus = 1
	m, _ = press(m, "k")
	if m.promptTop != 0 {
		t.Fatalf("promptTop %d after opening another item", m.promptTop)
	}
}

func TestBracesWhileArmedOnlyCancel(t *testing.T) {
	_, m := setup(t)
	m.focus, m.promptTop = 2, 5
	m, _ = press(m, " ")
	if !m.armed {
		t.Fatal("not armed")
	}
	m, _ = press(m, "}")
	if m.armed || m.promptTop != 5 {
		t.Fatalf("armed=%v promptTop=%d", m.armed, m.promptTop)
	}
}
