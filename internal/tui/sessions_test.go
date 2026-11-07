package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/nicholas1513/handoff/internal/claudesessions"
	"github.com/nicholas1513/handoff/internal/queue"
)

func key(m model, t tea.KeyType) (model, tea.Cmd) {
	next, cmd := m.Update(tea.KeyMsg{Type: t})
	return next.(model), cmd
}

// fakeSessions plans one window to restore and counts restores.
func fakeSessions(t *testing.T, open int) *int {
	restored := 0
	oldP, oldR, oldS, oldC := prepareRestore, restoreSessions, saveSessions, closableTabs
	closableTabs = func(*queue.Queue) (int, int) { return 0, 0 }
	prepareRestore = func(*queue.Queue) (claudesessions.Plan, error) {
		p := claudesessions.Plan{Running: 1}
		if open > 0 {
			p.Open = [][]claudesessions.Tab{make([]claudesessions.Tab, open)}
		}
		return p, nil
	}
	restoreSessions = func(claudesessions.Plan) (string, error) { restored++; return "restored", nil }
	saveSessions = func(*queue.Queue, time.Time) (string, error) { return "saved 1 window · 2 sessions", nil }
	t.Cleanup(func() { prepareRestore, restoreSessions, saveSessions, closableTabs = oldP, oldR, oldS, oldC })
	return &restored
}

func TestCtrlLAsksDefaultingToYes(t *testing.T) {
	for name, sel := range map[string]tea.KeyType{"space": tea.KeySpace, "enter": tea.KeyEnter} {
		_, m := setup(t)
		m.w, m.h = 120, 40
		restored := fakeSessions(t, 2)
		m, cmd := key(m, tea.KeyCtrlL)
		if cmd != nil || *restored != 0 {
			t.Fatal("ctrl+l must only open the prompt")
		}
		v := ansi.Strip(m.View())
		for _, want := range []string{"Restore sessions", "1 window · 2 sessions (1 running)", "[ No ]  [ Yes ]"} {
			if !strings.Contains(v, want) {
				t.Fatalf("prompt is missing %q:\n%s", want, v)
			}
		}
		if strings.Contains(v, "reject") || strings.Contains(v, "space") {
			t.Fatalf("the prompt shouldn't explain its keys:\n%s", v)
		}
		if !m.prompt.yes {
			t.Fatal("must default to Yes")
		}
		m, cmd = key(m, sel)
		if cmd == nil {
			t.Fatalf("%s didn't restore", name)
		}
		next, _ := m.Update(cmd())
		if m = next.(model); *restored != 1 || m.status != "restored" || m.prompt.open() {
			t.Fatalf("restored=%d status=%q open=%v", *restored, m.status, m.prompt.open())
		}
	}
}

func TestRestorePromptNoDoesNothing(t *testing.T) {
	_, m := setup(t)
	restored := fakeSessions(t, 1)
	m, _ = key(m, tea.KeyCtrlL)
	m, _ = key(m, tea.KeyLeft)
	m, cmd := press(m, " ")
	if cmd != nil || m.prompt.open() || *restored != 0 {
		t.Fatalf("cmd=%v open=%v restored=%d", cmd != nil, m.prompt.open(), *restored)
	}
}

// x was this prompt's cancel once. Now it's a key like any other here, and it must not reject the item behind.
func TestXDoesNothingInTheRestorePrompt(t *testing.T) {
	q, m := setup(t)
	fakeSessions(t, 1)
	m, _ = key(m, tea.KeyCtrlL)
	m, cmd := press(m, "x")
	if cmd != nil || !m.prompt.open() {
		t.Fatalf("cmd=%v open=%v", cmd != nil, m.prompt.open())
	}
	if st := q.State(*m.item(), time.Now()); st != queue.Pending {
		t.Fatalf("item state = %v", st)
	}
}

func TestRestoreOverlaySwallowsKeysAndMouse(t *testing.T) {
	q, m := setup(t)
	m.w, m.h = 120, 40
	restored := fakeSessions(t, 1)
	m, _ = key(m, tea.KeyCtrlL)
	sel, focus := m.ii, m.focus
	for _, k := range []string{"j", "k", "r", "q", "s", "o", "y", "c", "x", "\t"} {
		var cmd tea.Cmd
		if m, cmd = press(m, k); cmd != nil {
			t.Fatalf("%q returned a command", k)
		}
	}
	m, cmd := key(m, tea.KeyCtrlL)
	if cmd != nil {
		t.Fatal("ctrl+l again restored")
	}
	m, _ = click(m, 1, listTop) // behind the overlay, in the sessions pane
	sw, _, _ := columns(m.w)
	m, _ = click(m, sw+2, listTop) // the items pane
	for _, b := range []tea.MouseButton{tea.MouseButtonWheelUp, tea.MouseButtonWheelDown} {
		next, _ := m.Update(tea.MouseMsg{X: sw + 2, Y: listTop, Action: tea.MouseActionPress, Button: b})
		m = next.(model)
	}
	if m.ii != sel || m.focus != focus || !m.prompt.open() || m.menu || m.armed || m.confirm || *restored != 0 {
		t.Fatalf("leaked: ii=%d/%d focus=%d/%d open=%v menu=%v run=%v confirm=%v restored=%d",
			m.ii, sel, m.focus, focus, m.prompt.open(), m.menu, m.armed, m.confirm, *restored)
	}
	if st := q.State(*m.item(), time.Now()); st != queue.Pending {
		t.Fatalf("item state = %v", st)
	}
}

func TestRestorePromptTakesClicks(t *testing.T) {
	_, m := setup(t)
	m.w, m.h = 120, 40
	restored := fakeSessions(t, 1)
	m, _ = key(m, tea.KeyCtrlL)
	x, y := findBtn(t, m, promptNoBtn)
	m, cmd := click(m, x+1, y)
	if cmd != nil || m.prompt.open() {
		t.Fatalf("No: cmd=%v open=%v", cmd != nil, m.prompt.open())
	}
	m, _ = key(m, tea.KeyCtrlL)
	x, y = findBtn(t, m, promptYesBtn)
	m, cmd = click(m, x+1, y)
	if cmd == nil || m.prompt.open() {
		t.Fatal("Yes didn't restore")
	}
	cmd()
	if *restored != 1 {
		t.Fatalf("restored = %d", *restored)
	}
}

func TestCtrlLWithNothingToOpenDoesNotArm(t *testing.T) {
	_, m := setup(t)
	fakeSessions(t, 0)
	if m, _ = key(m, tea.KeyCtrlL); m.prompt.open() || !strings.HasPrefix(m.status, "nothing to restore") {
		t.Fatalf("open=%v status=%q", m.prompt.open(), m.status)
	}
}

func TestCtrlSSaves(t *testing.T) {
	_, m := setup(t)
	fakeSessions(t, 0)
	m, cmd := key(m, tea.KeyCtrlS)
	if cmd == nil {
		t.Fatal("ctrl+s didn't save")
	}
	next, _ := m.Update(cmd())
	if m = next.(model); m.status != "saved 1 window · 2 sessions" {
		t.Fatalf("status = %q", m.status)
	}
}

// The last save shows in the Sessions pane title, also one made by /handoff:claude-sessions in a Claude tab.
func TestSessionsTitleShowsLastSave(t *testing.T) {
	q, m := setup(t)
	at := time.Now().Add(-time.Minute)
	b := `{"saved_at":"` + at.Format(time.RFC3339Nano) + `","windows":[[{"session_id":"x"}],[{"session_id":"y"},{"session_id":"z"}]]}`
	if err := os.WriteFile(filepath.Join(q.Root, "claude-sessions.json"), []byte(b), 0o600); err != nil {
		t.Fatal(err)
	}
	m.refresh()
	want := "Sessions · saved " + at.Format("15:04") + " · 2 windows · 3 sessions"
	if got := ansi.Strip(m.sessionsPane(60, 20)[1]); !strings.Contains(got, want) {
		t.Fatalf("title row = %q, want %q", got, want)
	}
	// A narrow pane cuts the counts first; when the save ran stays readable.
	if got := ansi.Strip(m.sessionsPane(26, 20)[1]); !strings.Contains(got, "saved "+at.Format("15:04")) || ansi.StringWidth(got) != 26 {
		t.Fatalf("narrow title row = %q", got)
	}
}

// saveThenPrompt saves with tabs to offer, and counts closes and whether the viewer's tab was kept.
func saveThenPrompt(t *testing.T, m model, tabs, busy int) (model, *[]bool) {
	t.Helper()
	fakeSessions(t, 0)
	closableTabs = func(*queue.Queue) (int, int) { return tabs, busy }
	var closes []bool
	old := closeTabs
	closeTabs = func(_ *queue.Queue, keepOwn bool) (string, error) {
		closes = append(closes, keepOwn)
		return "closed", nil
	}
	t.Cleanup(func() { closeTabs = old })
	m, cmd := key(m, tea.KeyCtrlS)
	next, _ := m.Update(cmd())
	return next.(model), &closes
}

func TestSaveOffersToCloseDefaultingToNo(t *testing.T) {
	_, m := setup(t)
	m.w, m.h = 120, 40
	m, closes := saveThenPrompt(t, m, 3, 1)
	v := ansi.Strip(m.View())
	for _, want := range []string{"Close the 3 saved Claude tabs?", "[ No ]  [ Yes ]", "1 still working"} {
		if !strings.Contains(v, want) {
			t.Fatalf("prompt is missing %q:\n%s", want, v)
		}
	}
	if strings.Contains(v, "space") || strings.Contains(v, "←") {
		t.Fatalf("the prompt shouldn't explain its keys:\n%s", v)
	}
	if m.prompt.yes {
		t.Fatal("must default to No")
	}
	m, cmd := press(m, " ")
	if cmd != nil || m.prompt.open() || len(*closes) != 0 {
		t.Fatalf("space on No: cmd=%v open=%v closes=%v", cmd != nil, m.prompt.open(), *closes)
	}
}

func TestCloseArrowsAndBothSelectKeys(t *testing.T) {
	for name, sel := range map[string]tea.KeyType{"space": tea.KeySpace, "enter": tea.KeyEnter} {
		_, m := setup(t)
		m, closes := saveThenPrompt(t, m, 2, 0)
		m, _ = key(m, tea.KeyLeft)
		if m.prompt.yes {
			t.Fatalf("%s: left moved off No", name)
		}
		m, _ = key(m, tea.KeyRight)
		m, _ = key(m, tea.KeyRight)
		if !m.prompt.yes {
			t.Fatalf("%s: right didn't pick Yes", name)
		}
		m, cmd := key(m, sel)
		if cmd == nil || m.prompt.open() {
			t.Fatalf("%s: didn't close", name)
		}
		next, _ := m.Update(cmd())
		if m = next.(model); len(*closes) != 1 || (*closes)[0] || m.status != "closed" {
			t.Fatalf("%s: closes=%v status=%q", name, *closes, m.status)
		}
	}
}

// findBtn is where a button's text sits on screen.
func findBtn(t *testing.T, m model, btn string) (x, y int) {
	t.Helper()
	for y, l := range strings.Split(ansi.Strip(m.View()), "\n") {
		if i := strings.Index(l, btn); i >= 0 {
			return ansi.StringWidth(l[:i]), y
		}
	}
	t.Fatalf("no %q on screen", btn)
	return 0, 0
}

func TestCloseDecisionRowIsCentred(t *testing.T) {
	for _, busy := range []int{0, 2} {
		_, m := setup(t)
		m.w, m.h = 120, 40
		m, _ = saveThenPrompt(t, m, 3, busy)
		ol, ox, _ := m.promptOverlay()
		w := len([]rune(ansi.Strip(ol[0])))
		x, _ := findBtn(t, m, promptNoBtn)
		left, right := x-ox, ox+w-(x+len(promptNoBtn)+promptGap+len(promptYesBtn))
		if d := left - right; d < -1 || d > 1 {
			t.Fatalf("busy=%d: margins %d and %d", busy, left, right)
		}
	}
}

func TestCloseOptionsTakeClicks(t *testing.T) {
	for _, busy := range []int{0, 2} { // the warning moves the row
		_, m := setup(t)
		m.w, m.h = 120, 40
		m, closes := saveThenPrompt(t, m, 3, busy)
		x, y := findBtn(t, m, promptNoBtn)
		m, cmd := click(m, x+2, y)
		if cmd != nil || m.prompt.open() || len(*closes) != 0 {
			t.Fatalf("No: cmd=%v open=%v", cmd != nil, m.prompt.open())
		}
		m, closes = saveThenPrompt(t, m, 3, busy)
		x, y = findBtn(t, m, promptYesBtn)
		m, cmd = click(m, x+len(promptYesBtn)-1, y)
		if cmd == nil || m.prompt.open() {
			t.Fatalf("Yes: cmd=%v open=%v", cmd != nil, m.prompt.open())
		}
		cmd()
		if len(*closes) != 1 {
			t.Fatalf("closes = %v", *closes)
		}
	}
}

// Nothing reaches the item behind the prompt, and a run in progress keeps the viewer's own tab.
func TestClosePromptSwallowsEverything(t *testing.T) {
	q, m := setup(t)
	m.w, m.h = 120, 40
	m, closes := saveThenPrompt(t, m, 2, 0)
	sel, focus := m.ii, m.focus
	for _, k := range []string{"x", "r", "j", "q", "s", "?", "c", "o"} {
		var cmd tea.Cmd
		if m, cmd = press(m, k); cmd != nil {
			t.Fatalf("%q returned a command", k)
		}
	}
	m, _ = key(m, tea.KeyEsc)
	m, _ = click(m, m.w/2, m.h/2)
	m, _ = click(m, 1, listTop)
	if !m.prompt.open() || m.ii != sel || m.focus != focus || m.menu || m.manual || m.armed || len(*closes) != 0 {
		t.Fatalf("leaked: open=%v ii=%d focus=%d menu=%v manual=%v armed=%v", m.prompt.open(), m.ii, m.focus, m.menu, m.manual, m.armed)
	}
	if st := q.State(*m.item(), time.Now()); st != queue.Pending {
		t.Fatalf("item state = %v", st)
	}
}

// A save lands whenever it lands: a first space already pressed mustn't carry over into the prompt and on to a run.
func TestClosePromptDropsWhatWasArmed(t *testing.T) {
	_, m := setup(t)
	fakeSessions(t, 0)
	closableTabs = func(*queue.Queue) (int, int) { return 2, 0 }
	m.armed = true
	next, _ := m.Update(savedMsg{"saved", 2, 0})
	if m = next.(model); m.armed || !m.prompt.open() {
		t.Fatalf("armed=%v open=%v", m.armed, m.prompt.open())
	}
}

func TestSaveWithNothingToCloseDoesNotAsk(t *testing.T) {
	_, m := setup(t)
	m, _ = saveThenPrompt(t, m, 0, 0)
	if m.prompt.open() || m.status == "" {
		t.Fatalf("open=%v status=%q", m.prompt.open(), m.status)
	}
}
