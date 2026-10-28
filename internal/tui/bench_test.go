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
		if it = addItem(b, q, it, files); stdout != "" {
			finished(b, q, it, queue.Succeeded, stdout)
		}
		return it
	}
	for i := 0; i < 175; i++ {
		add(queue.Item{Description: fmt.Sprintf("item%03d", i)}, nil, map[bool]string{true: "ok\n"}[i%2 == 1])
	}
	var js strings.Builder
	for i := 1; i <= 5000; i++ {
		fmt.Fprintf(&js, `"key%04d":%d,`, i, i)
	}
	q.AddPrompt(queue.Prompt{SessionID: sid, TS: time.Now(), Prompt: numbered("prompt line %04d", 1000)})
	body := tempFile(b, "body.json", "{"+strings.TrimSuffix(js.String(), ",")+"}")
	rich := add(queue.Item{Description: "rich", Argv: []string{"az", "rest", "--method", "put", "--body", "@{file:body.json}"}},
		map[string]string{"body.json": body}, numbered("out%04d some result text", 5000))
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

// BenchmarkRefresh is the 2 s tick on the event loop: issuing the load and applying it. The load's reads run off the
// loop (BenchmarkLoad).
func BenchmarkRefresh(b *testing.B) {
	m := benchModel(b)
	_, cmd := m.Update(tickMsg(time.Now()))
	l := cmd()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		next, _ := m.Update(tickMsg(time.Now()))
		next.Update(l)
	}
}

// BenchmarkLoad is the tick's reads of the queue, off the event loop.
func BenchmarkLoad(b *testing.B) {
	m := benchModel(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.load()()
	}
}
