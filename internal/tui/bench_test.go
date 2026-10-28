package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/nicholas1513/handoff/internal/queue"
)

// benchModel is the measured worst case: 176 queued items in one session (every other one finished), the selected
// one with a 5,000-line result, a 5,000-key JSON attachment and a 1,000-line prompt, at 120x60, detail focused.
func benchModel(b *testing.B) model {
	b.Helper()
	q := &queue.Queue{Root: b.TempDir()}
	cwd := b.TempDir()
	if err := q.WriteSession(queue.Session{SessionID: sid, Cwd: cwd}); err != nil {
		b.Fatal(err)
	}
	add := func(it queue.Item, files map[string]string, stdout string) queue.Item {
		it.Cwd, it.SessionID = cwd, sid
		if it.Argv == nil {
			it.Argv = []string{"/usr/bin/true"}
		}
		it, err := q.Add(it, files)
		if err == nil && stdout != "" {
			if err = q.Claim(it); err == nil {
				err = q.WriteResult(it, queue.Result{State: queue.Succeeded, RanAt: time.Now(), Stdout: stdout})
			}
		}
		if err != nil {
			b.Fatal(err)
		}
		return it
	}
	for i := 0; i < 175; i++ {
		add(queue.Item{Description: fmt.Sprintf("item%03d", i)}, nil, map[bool]string{true: "ok\n"}[i%2 == 1])
	}
	var p, js, out strings.Builder
	for i := 1; i <= 1000; i++ {
		fmt.Fprintf(&p, "prompt line %04d\n", i)
	}
	for i := 1; i <= 5000; i++ {
		fmt.Fprintf(&js, `"key%04d":%d,`, i, i)
		fmt.Fprintf(&out, "out%04d some result text\n", i)
	}
	q.AddPrompt(queue.Prompt{SessionID: sid, TS: time.Now(), Prompt: p.String()})
	body := filepath.Join(b.TempDir(), "body.json")
	if err := os.WriteFile(body, []byte("{"+strings.TrimSuffix(js.String(), ",")+"}"), 0o600); err != nil {
		b.Fatal(err)
	}
	rich := add(queue.Item{Description: "rich", Argv: []string{"az", "rest", "--method", "put", "--body", "@{file:body.json}"}},
		map[string]string{"body.json": body}, out.String())
	m := newModel(q, b.TempDir())
	for i, it := range m.session().items {
		if it.ID == rich.ID {
			m.ii = i
		}
	}
	m.open()
	m = resize(m, 120, 60)
	m.focus = 2
	return m
}

// rowOf is the first screen row holding s.
func rowOf(b *testing.B, m model, s string) int {
	for i, l := range strings.Split(ansi.Strip(m.View()), "\n") {
		if strings.Contains(l, s) {
			return i
		}
	}
	b.Fatalf("%q not drawn", s)
	return 0
}

func BenchmarkView(b *testing.B) {
	m := benchModel(b)
	m.View()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.View()
	}
}

// BenchmarkWheelOverBox is one wheel event over the result box: Update only (View is BenchmarkView).
func BenchmarkWheelOverBox(b *testing.B) {
	m := benchModel(b)
	sw, iw, _ := columns(m.w)
	y := rowOf(b, m, "│ stdout")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		btn := tea.MouseButtonWheelDown
		if i/1000%2 == 1 {
			btn = tea.MouseButtonWheelUp
		}
		m = mouseAt(m, sw+iw+5, y, btn)
	}
}

// BenchmarkWheelOverItems is one wheel event over the items pane, each moving the selection and opening an item.
func BenchmarkWheelOverItems(b *testing.B) {
	m := benchModel(b)
	sw, _, _ := columns(m.w)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		btn := tea.MouseButtonWheelUp
		if i/100%2 == 1 {
			btn = tea.MouseButtonWheelDown
		}
		m = mouseAt(m, sw+3, listTop+3, btn)
	}
}

// BenchmarkRefresh is the 2 s tick: reload the queue and re-open the selection.
func BenchmarkRefresh(b *testing.B) {
	m := benchModel(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		next, _ := m.Update(tickMsg(time.Now()))
		m = next.(model)
	}
}
