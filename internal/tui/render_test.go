package tui

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/nicholas1513/handoff/internal/queue"
)

const sidC = "2e8f4c0a-3b5f-4e77-9c90-4f6b8d0e2a34"

// fresh is m with empty render caches: what View draws when nothing cached can be stale.
func fresh(m model) model {
	m.rc, m.pc = newRenderCache(), newPaneCache()
	return m
}

// firstDiff is the first screen row where two views differ, unstyled.
func firstDiff(got, want string) string {
	g, w := strings.Split(ansi.Strip(got), "\n"), strings.Split(ansi.Strip(want), "\n")
	for i := 0; i < max(len(g), len(w)); i++ {
		var a, b string
		if i < len(g) {
			a = g[i]
		}
		if i < len(w) {
			b = w[i]
		}
		if a != b {
			return fmt.Sprintf("row %d\n  got  %q\n  want %q", i, a, b)
		}
	}
	return "only the styling differs"
}

// sameAsFresh fails when m draws differently from m with empty caches.
func sameAsFresh(t *testing.T, m model, after string) {
	t.Helper()
	if got, want := m.View(), fresh(m).View(); got != want {
		t.Fatalf("after %s the view differs from one drawn with empty caches: %s", after, firstDiff(got, want))
	}
}

// drawn is pane p (0 sessions, 1 items, 2 detail) of m's view, one unstyled string per screen row, borders included.
func drawn(m model, p int) []string {
	sw, iw, dw := columns(m.w)
	a, b := [3]int{0, sw, sw + iw}[p], [3]int{sw, sw + iw, sw + iw + dw}[p]
	var out []string
	for _, l := range strings.Split(ansi.Strip(m.View()), "\n") {
		out = append(out, ansi.Cut(l, a, b))
	}
	return out
}

// rowWith is the first row holding every one of subs, or "".
func rowWith(rows []string, subs ...string) string {
	for _, r := range rows {
		all := true
		for _, s := range subs {
			all = all && strings.Contains(r, s)
		}
		if all {
			return r
		}
	}
	return ""
}

func mouseAt(m model, x, y int, b tea.MouseButton) model {
	next, _ := m.Update(tea.MouseMsg{X: x, Y: y, Action: tea.MouseActionPress, Button: b})
	return next.(model)
}

func resize(m model, w, h int) model {
	next, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	return next.(model)
}

// addItem stages it (argv /usr/bin/true unless set) with attachments and returns it as stamped.
func addItem(t testing.TB, q *queue.Queue, it queue.Item, files map[string]string) queue.Item {
	t.Helper()
	if it.Argv == nil {
		it.Argv = []string{"/usr/bin/true"}
	}
	if it.Cwd == "" {
		it.Cwd = t.TempDir()
	}
	it, err := q.Add(it, files)
	if err != nil {
		t.Fatal(err)
	}
	return it
}

// finished records a result for it, as a run or a reject would.
func finished(t testing.TB, q *queue.Queue, it queue.Item, state, stdout string) {
	t.Helper()
	if err := q.Claim(it); err != nil {
		t.Fatal(err)
	}
	if err := q.WriteResult(it, queue.Result{State: state, RanAt: time.Now(), Stdout: stdout}); err != nil {
		t.Fatal(err)
	}
}

// pick opens the item with that description, as clicking it would.
func pick(t *testing.T, m model, desc string) model {
	t.Helper()
	for si, r := range m.rows {
		for ii, it := range r.items {
			if it.Description == desc {
				m.si, m.ii = si, ii
				m.open()
				return m
			}
		}
	}
	t.Fatalf("no item %q", desc)
	return m
}

func numbered(format string, n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, format+"\n", i)
	}
	return b.String()
}

func tempFile(t testing.TB, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// richWorld stages three sessions and returns the model on the rich item at 120x40 with the detail focused and the
// clock fixed. A (verified, titled) holds pending and finished items, a rejected one, an edit with a diff, a git am of
// two commits and the rich item (60-line prompt, 120-line result, a JSON and a text attachment); B is unverified with
// one pending item; C is verified with a finished one.
func richWorld(t *testing.T) model {
	t.Helper()
	q, m := setup(t) // A: "first" and "second" pending after "do the thing"
	tr := tempFile(t, "a.jsonl", `{"type":"ai-title","aiTitle":"Session A"}`)
	q.WriteSession(queue.Session{SessionID: sid, Cwd: t.TempDir(), TranscriptPath: tr})
	q.WriteSession(queue.Session{SessionID: sidC, Cwd: t.TempDir()})
	finished(t, q, addItem(t, q, queue.Item{Description: "rejected", SessionID: sid}, nil), queue.Rejected, "")
	addItem(t, q, queue.Item{Description: "b1", SessionID: sidB}, nil)
	for i := 0; i < 8; i++ { // enough rows that short windows scroll the items pane
		it := addItem(t, q, queue.Item{Description: fmt.Sprintf("a%d", i), SessionID: sid}, nil)
		if i%2 == 0 {
			finished(t, q, it, queue.Succeeded, "ok\n")
		}
	}
	finished(t, q, addItem(t, q, queue.Item{Description: "c1", SessionID: sidC}, nil), queue.Succeeded, "done\n")

	var old, neu []string
	for i := 1; i <= 40; i++ {
		l := fmt.Sprintf("line %d", i)
		old, neu = append(old, l), append(neu, twoChanges(i, l))
	}
	neu[4] = "changed 5 " + strings.Repeat("wide ", 30) // wraps, or pans with wrap off
	target := tempFile(t, "f.txt", strings.Join(old, "\n")+"\n")
	addItem(t, q, queue.Item{Description: "edit", Edit: target, Argv: queue.EditArgv(target), SessionID: sid},
		map[string]string{queue.EditFile: tempFile(t, "new.txt", strings.Join(neu, "\n")+"\n")})

	repo, patch := commitsPatch(t)
	addItem(t, q, queue.Item{Description: "am", SessionID: sid, Argv: []string{"git", "-C", repo, "am", "{file:commits.patch}"}},
		map[string]string{"commits.patch": patch})

	q.AddPrompt(queue.Prompt{SessionID: sid, TS: time.Now(), Prompt: strings.TrimSuffix(numbered("p%03d", 60), "\n")})
	var js strings.Builder
	for i := 1; i <= 40; i++ {
		fmt.Fprintf(&js, `"k%03d":%d,`, i, i)
	}
	files := map[string]string{
		"body.json": tempFile(t, "body.json", "{"+strings.TrimSuffix(js.String(), ",")+"}"),
		"notes.txt": tempFile(t, "notes.txt", "short\nSTART"+strings.Repeat("y", 200)+"END\n"),
	}
	rich := addItem(t, q, queue.Item{Description: "rich", SessionID: sid,
		Argv: []string{"az", "rest", "--method", "put", "--body", "@{file:body.json}", "{file:notes.txt}"}}, files)
	finished(t, q, rich, queue.Succeeded, numbered("out%03d", 120)+"out999"+strings.Repeat("x", 130)+"\n")

	m = newModel(q, m.home)
	clock := time.Now()
	m.now = func() time.Time { return clock }
	m = resize(pick(t, m, "rich"), 120, 40)
	m.focus = 2
	return m
}

var (
	viewWidths  = []int{120, 100, 80, 60, 200, 40}
	viewHeights = []int{40, 24, 60, 14, 10}
	opKeys      = []string{"j", "k", "tab", "shift+tab", "[", "]", "{", "}", "+", "<", ">", "s", "esc", "o", "x", "v"}
	wheels      = []tea.MouseButton{tea.MouseButtonWheelDown, tea.MouseButtonWheelDown, tea.MouseButtonWheelUp,
		tea.MouseButtonWheelUp, tea.MouseButtonWheelLeft, tea.MouseButtonWheelRight}
)

// randomOp picks one user or disk event from what view (m's current screen) shows, or the review copy it is building
// arriving. once marks an event to apply to one model only: a reject writes the queue. A disk change is made here and
// the returned op is the tick after it.
func randomOp(t *testing.T, r *rand.Rand, m model, view string, added *int) (name string, op func(model) model, once bool) {
	if m.amFlight != nil && r.Intn(3) == 0 {
		return "review copy arrives", built, false
	}
	rows := strings.Split(ansi.Strip(view), "\n")
	menu := strings.Contains(view, "Diff word wrap")
	sw, iw, _ := columns(m.w)
	dx := sw + iw
	mouse := func(what string, x, y int, b tea.MouseButton) (string, func(model) model, bool) {
		return fmt.Sprintf("%s %v at %d,%d", what, b, x, y), func(m model) model { return mouseAt(m, x, y, b) }, false
	}
	tick := func(what string) (string, func(model) model, bool) { return what + ", tick", tickM, false }
	find := func(sub string) (x, y int, ok bool) {
		for i, l := range rows {
			if j := strings.Index(l, sub); j >= 0 {
				return ansi.StringWidth(l[:j]), i, true
			}
		}
		return 0, 0, false
	}
	switch c := r.Intn(100); {
	case c < 45:
		k := opKeys[r.Intn(len(opKeys))]
		if menu && r.Intn(3) == 0 {
			k = " " // only in the menu: outside it space arms a run
		}
		if !menu && strings.ContainsAny(view, "‹›") && r.Intn(3) == 0 {
			k = []string{">", ">", "<"}[r.Intn(3)] // a cut line is drawn: pan more often than chance would
		}
		return "key " + k, func(m model) model { m, _ = press(m, k); return m }, k == "x"
	case c < 65:
		b := wheels[r.Intn(len(wheels))]
		var boxRows []int
		for y, l := range rows {
			if ansi.Cut(l, dx+1, dx+2) == "│" {
				boxRows = append(boxRows, y)
			}
		}
		if len(boxRows) > 0 && r.Intn(2) == 0 {
			return mouse("wheel over box", dx+4, boxRows[r.Intn(len(boxRows))], b)
		}
		return mouse("wheel", []int{1, sw + 2, dx + 4}[r.Intn(3)], r.Intn(m.h), b)
	case c < 82:
		left := tea.MouseButtonLeft
		switch r.Intn(5) {
		case 0:
			return mouse("click items", sw+3, listTop+r.Intn(max(1, m.h-4)), left)
		case 1:
			return mouse("click sessions", 1, listTop+r.Intn(4), left)
		case 2:
			if _, y, ok := find("[+]"); ok {
				return mouse("click [+]", dx+5, y, left)
			}
		case 3:
			if x, y, ok := find(settingsKey); ok && y == len(rows)-1 {
				return mouse("click settings hint", x+1, y, left)
			}
		case 4:
			if x, y, ok := find("word wrap"); ok && menu {
				return mouse("click menu row", x, y, left)
			}
		}
		return mouse("click detail", dx+5, buttonY+1+r.Intn(max(1, m.h-5)), left) // never the Run/Reject row
	case c < 88:
		w, h := m.w, m.h // often only one of them changes, so a cache keyed on the other shows stale
		switch r.Intn(3) {
		case 0:
			w = viewWidths[r.Intn(len(viewWidths))]
		case 1:
			h = viewHeights[r.Intn(len(viewHeights))]
		default:
			w, h = viewWidths[r.Intn(len(viewWidths))], viewHeights[r.Intn(len(viewHeights))]
		}
		return fmt.Sprintf("resize %dx%d", w, h), func(m model) model { return resize(m, w, h) }, false
	case c < 97:
		items, _, _ := m.q.List()
		var pend []queue.Item
		for _, it := range items {
			if cur := m.item(); m.q.State(it, time.Now()) == queue.Pending && (cur == nil || cur.ID != it.ID) {
				pend = append(pend, it)
			}
		}
		switch k := r.Intn(3); {
		case k == 0 && len(pend) > 0:
			it := pend[r.Intn(len(pend))]
			finished(t, m.q, it, []string{queue.Succeeded, queue.Failed}[r.Intn(2)], numbered("res%02d", r.Intn(40)))
			return tick("result for " + it.Description)
		case k == 1 && len(pend) > 0:
			it := pend[r.Intn(len(pend))]
			if err := m.q.Revoke(it, it.SessionID); err != nil {
				t.Fatal(err)
			}
			return tick("revoke " + it.Description)
		}
		*added++
		d := fmt.Sprintf("new%d", *added)
		addItem(t, m.q, queue.Item{Description: d, SessionID: []string{sid, sidB, sidC}[r.Intn(3)]}, nil)
		return tick("add " + d)
	}
	return tick("nothing changed")
}

// TestCachedViewMatchesFresh drives seeded random keys, wheels, clicks, resizes, setting toggles and disk changes,
// and after each one checks that the view drawn with warm caches is the view drawn with empty ones, and that the
// event did the same thing as on a copy whose caches were empty when it arrived (hit-testing included).
func TestCachedViewMatchesFresh(t *testing.T) {
	trueColor(t) // so a selection or colour drawn from a stale cache shows as a difference
	for _, c := range []struct {
		seed  int64
		item  string
		start []string
	}{
		{1, "rich", nil},
		{2, "edit", nil},
		{3, "rich", []string{"s", "j", " ", "esc"}}, // attachment wrap off: the attachments pan
		{4, "edit", []string{"s", " ", "esc"}},      // diff wrap off: the diff pans
		{5, "am", nil},
		{6, "am", []string{"s", " ", "esc"}},            // diff wrap off: the commits pan
		{11, "am", []string{"s", "j", "j", " ", "esc"}}, // commit review in VS Code
	} {
		seed := c.seed
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			m := pick(t, richWorld(t), c.item)
			for _, k := range c.start {
				m, _ = press(m, k)
			}
			r := rand.New(rand.NewSource(seed))
			view, added := m.View(), 0
			var log []string
			for i := 0; i < 100; i++ {
				name, op, once := randomOp(t, r, m, view, &added)
				log = append(log, name)
				cold := fresh(m)
				m = op(m)
				if once {
					cold = fresh(m)
				} else {
					cold = op(cold)
				}
				view = m.View()
				if want := fresh(m).View(); view != want {
					t.Fatalf("seed %d op %d: the view with warm caches differs from empty caches: %s\nops:\n  %s",
						seed, i, firstDiff(view, want), strings.Join(log, "\n  "))
				}
				if want := cold.View(); view != want {
					t.Fatalf("seed %d op %d: the event acted differently with warm caches than with empty ones: %s\nops:\n  %s",
						seed, i, firstDiff(view, want), strings.Join(log, "\n  "))
				}
			}
		})
	}
}

// TestDrawingReadsNoFiles: once the model is built and drawn, drawing and wheel-scrolling a box must not touch the
// queue or the transcripts, so removing them changes nothing on screen.
func TestDrawingReadsNoFiles(t *testing.T) {
	q, m := setup(t)
	tr := tempFile(t, "t.jsonl", `{"type":"ai-title","aiTitle":"Titled session"}`)
	q.WriteSession(queue.Session{SessionID: sid, Cwd: t.TempDir(), TranscriptPath: tr})
	m = withAtt(t, m, 100, 200, 198, 40)
	before := m.View()
	y := strings.Index(ansi.Strip(before), "│ stdout")
	y = strings.Count(ansi.Strip(before)[:max(0, y)], "\n")
	if !strings.Contains(ansi.Strip(before), "Titled session") || y == 0 {
		t.Fatalf("setup: title or result box not drawn:\n%s", ansi.Strip(before))
	}
	if err := os.RemoveAll(q.Root); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(tr); err != nil {
		t.Fatal(err)
	}
	if after := m.View(); after != before {
		t.Errorf("View read from disk: with the queue root and the transcript removed, the screen changed (the row "+
			"shows something drawing re-read: session title, item state, result, prompt, attachment size, ...): %s",
			firstDiff(after, before))
	}
	sw, iw, _ := columns(m.w)
	m = mouseAt(m, sw+iw+5, y, tea.MouseButtonWheelDown)
	if v := ansi.Strip(m.View()); !strings.Contains(v, "lines 2–17 of 201") {
		t.Errorf("wheel over the result box with the files removed did not scroll it to lines 2–17 of 201 "+
			"(hit-testing or scrolling re-read the result from disk):\n%s", v)
	}
}

// Each event below must change the screen in the same Update, and match a view drawn with empty caches.

func TestResizeShowsAtOnce(t *testing.T) {
	trueColor(t)
	_, m := setup(t)
	m = withResult(t, m, 200)
	for _, s := range [][3]int{{80, 24, 9}, {200, 60, 24}, {60, 30, 12}, {120, 40, 16}, {120, 60, 24}, {100, 60, 24}} {
		m = resize(m, s[0], s[1])
		rows := strings.Split(ansi.Strip(m.View()), "\n")
		if len(rows) != s[1] {
			t.Fatalf("%dx%d: %d rows drawn", s[0], s[1], len(rows))
		}
		for i, l := range rows[:len(rows)-1] {
			if w := ansi.StringWidth(l); w != max(s[0], 70) {
				t.Fatalf("%dx%d: row %d is %d wide: %q", s[0], s[1], i, w, l)
			}
		}
		if !strings.Contains(strings.Join(rows, "\n"), fmt.Sprintf("lines 1–%d ", s[2])) {
			t.Fatalf("%dx%d: result box not capped at %d rows:\n%s", s[0], s[1], s[2], strings.Join(rows, "\n"))
		}
		sameAsFresh(t, m, fmt.Sprintf("resize to %dx%d", s[0], s[1]))
	}
}

func longLine5(i int, l string) string {
	if i == 5 {
		return "START" + strings.Repeat("0123456789", 20) + "END"
	}
	return l
}

func TestSettingTogglesShowAtOnce(t *testing.T) {
	trueColor(t)
	m, _ := editModel(t, 120, 40, longLine5)
	m.focus = 2
	if rowWith(drawn(m, 2), "›") != "" {
		t.Fatal("setup: a cut line with diff wrap on")
	}
	m, _ = press(m, "s")
	m, _ = press(m, " ")
	if rowWith(strings.Split(ansi.Strip(m.View()), "\n"), "Diff word wrap", "[ off ]") == "" {
		t.Fatal("the menu still shows diff wrap on")
	}
	sameAsFresh(t, m, "toggling diff wrap")
	m, _ = press(m, "esc")
	if rowWith(drawn(m, 2), "+START", "›") == "" {
		t.Fatalf("diff wrap off: the long line is not cut:\n%s", strings.Join(drawn(m, 2), "\n"))
	}
	sameAsFresh(t, m, "closing the menu")

	_, m = setup(t)
	m = withItem(t, m, queue.Item{Description: "x", Argv: []string{"/bin/cat", "{file:notes.txt}"}},
		map[string]string{"notes.txt": tempFile(t, "notes.txt", "short\nSTART"+strings.Repeat("y", 300)+"END\n")}, 120)
	m = resize(m, 120, 60)
	if rowWith(drawn(m, 2), "START") == "" || rowWith(drawn(m, 2), "›") != "" {
		t.Fatalf("setup: attachment not drawn wrapped:\n%s", strings.Join(drawn(m, 2), "\n"))
	}
	for _, k := range []string{"s", "j", " "} {
		m, _ = press(m, k)
	}
	if rowWith(strings.Split(ansi.Strip(m.View()), "\n"), "Attachment word wrap", "[ off ]") == "" {
		t.Fatal("the menu still shows attachment wrap on")
	}
	sameAsFresh(t, m, "toggling attachment wrap")
	m, _ = press(m, "esc")
	if rowWith(drawn(m, 2), "START", "›") == "" {
		t.Fatalf("attachment wrap off: the long line is not cut:\n%s", strings.Join(drawn(m, 2), "\n"))
	}
	sameAsFresh(t, m, "closing the menu")
}

func TestDiffExpandShowsAtOnce(t *testing.T) {
	trueColor(t)
	m, _ := editModel(t, 160, 60, twoChanges)
	if rowWith(drawn(m, 2), "line 12") != "" {
		t.Fatal("setup: hidden context drawn")
	}
	m2, _ := press(m, "+")
	if rowWith(drawn(m2, 2), "line 12") == "" {
		t.Fatal("+ did not show the hidden context at once")
	}
	sameAsFresh(t, m2, "+")

	sw, iw, _ := columns(m.w)
	y := -1
	for i, l := range drawn(m, 2) {
		if y < 0 && strings.Contains(l, "[+] ↓") {
			y = i
		}
	}
	if y < 0 {
		t.Fatal("no [+] ↓ row drawn")
	}
	m = mouseAt(m, sw+iw+5, y, tea.MouseButtonLeft)
	if rowWith(drawn(m, 2), "line 12") == "" {
		t.Fatal("clicking [+] did not show the hidden context at once")
	}
	sameAsFresh(t, m, "clicking [+]")
}

func TestPanShowsAtOnce(t *testing.T) {
	trueColor(t)
	m, _ := editModel(t, 120, 40, longLine5)
	for _, k := range []string{"s", " ", "esc"} { // diff wrap off
		m, _ = press(m, k)
	}
	if rowWith(drawn(m, 2), "‹") != "" {
		t.Fatal("setup: already panned")
	}
	m, _ = press(m, ">")
	if rowWith(drawn(m, 2), "‹") == "" {
		t.Fatal("> did not pan at once")
	}
	sameAsFresh(t, m, ">")
	for i := 0; i < 10; i++ {
		m, _ = press(m, ">")
	}
	if rowWith(drawn(m, 2), "9END") == "" {
		t.Fatal("panning to the end did not show the line's end")
	}
	sameAsFresh(t, m, "> to the end")
	m, _ = press(m, "<")
	if rowWith(drawn(m, 2), "9END") != "" {
		t.Fatal("< did not pan back at once")
	}
	sameAsFresh(t, m, "<")
	y := -1
	for i, l := range drawn(m, 2) {
		if y < 0 && strings.HasPrefix(l, "││") { // a box content row
			y = i
		}
	}
	before := m.View()
	sw, iw, _ := columns(m.w)
	m = mouseAt(m, sw+iw+5, y, tea.MouseButtonWheelLeft)
	if m.View() == before {
		t.Fatal("a horizontal wheel over the box did not pan at once")
	}
	sameAsFresh(t, m, "a horizontal wheel")
}

func TestOpeningAnotherItemShowsAtOnce(t *testing.T) {
	trueColor(t)
	_, m := setup(t)
	m.focus = 1
	shows := func(m model, want, not string) bool {
		d := strings.Join(drawn(m, 2), "\n")
		return strings.Contains(d, want) && !strings.Contains(d, not)
	}
	if !shows(m, "second", "first") {
		t.Fatal("setup: detail not on second")
	}
	m, _ = press(m, "k")
	if !shows(m, "first", "second") {
		t.Fatal("k did not open first at once")
	}
	sameAsFresh(t, m, "k")
	sw, _, _ := columns(m.w)
	m = mouseAt(m, sw+3, listTop+3, tea.MouseButtonWheelDown)
	if !shows(m, "second", "first") {
		t.Fatal("the wheel over the items did not open second at once")
	}
	sameAsFresh(t, m, "the wheel over the items")
	m = mouseAt(m, sw+3, listTop+1, tea.MouseButtonLeft)
	if !shows(m, "first", "second") {
		t.Fatal("clicking first did not open it at once")
	}
	sameAsFresh(t, m, "clicking an item")
}

func TestTickAfterResultShowsAtOnce(t *testing.T) {
	trueColor(t)
	q, m := setup(t)
	items, _, _ := q.List()
	for _, it := range items {
		if it.Description == "second" {
			finished(t, q, it, queue.Succeeded, "hello-result\n")
		}
	}
	m = tickM(m)
	d := drawn(m, 2)
	if rowWith(d, "SUCCEEDED") == "" || rowWith(d, "── Result") == "" || rowWith(d, "hello-result") == "" {
		t.Fatalf("the tick after the result did not show it:\n%s", strings.Join(d, "\n"))
	}
	if rowWith(drawn(m, 1), "second", "✓") == "" {
		t.Fatalf("the item row has no ✓:\n%s", strings.Join(drawn(m, 1), "\n"))
	}
	sameAsFresh(t, m, "the tick")
}

// A tick that shortens the open item's detail (here a broken settings file is fixed) must leave it scrolled no
// further than its new end, where j changes nothing.
func TestTickThatShortensTheDetailKeepsItInBounds(t *testing.T) {
	_, m := setup(t)
	p := filepath.Join(m.home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	m = resize(tickM(m), 120, 14)
	m.focus = 2
	if rowWith(drawn(m, 2), "settings problem") == "" {
		t.Fatalf("setup: no settings problem drawn:\n%s", strings.Join(drawn(m, 2), "\n"))
	}
	for i := 0; i < 40; i++ { // to the end
		m, _ = press(m, "j")
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	m = tickM(m)
	end := m.View()
	if m, _ = press(m, "j"); m.View() != end {
		t.Errorf("the tick left the detail scrolled past its end: %s", firstDiff(end, m.View()))
	}
}

func TestRejectShowsAtOnce(t *testing.T) {
	trueColor(t)
	_, m := setup(t) // "second" selected; x advances to "first"
	m, _ = press(m, "x")
	if rowWith(drawn(m, 1), "second", "⊘") == "" {
		t.Errorf("the rejected item's row lacks ⊘ until the next tick:\n%s", strings.Join(drawn(m, 1), "\n"))
	}
	if rowWith(drawn(m, 0), "● 1 ") == "" {
		t.Errorf("the session still counts the rejected item as pending:\n%s", strings.Join(drawn(m, 0), "\n"))
	}
	sameAsFresh(t, m, "x")
	m, _ = press(m, "x") // nothing left pending: the selection stays on "first"
	if rowWith(drawn(m, 2), "REJECTED") == "" || rowWith(drawn(m, 1), "first", "⊘") == "" {
		t.Errorf("the rejected item still shows pending:\n%s", ansi.Strip(m.View()))
	}
	if rowWith(drawn(m, 0), "●") != "" {
		t.Errorf("the session still shows a pending count:\n%s", strings.Join(drawn(m, 0), "\n"))
	}
	sameAsFresh(t, m, "the second x")
}

func TestRunStartShowsAtOnce(t *testing.T) {
	trueColor(t)
	_, m := setup(t)
	m, _ = press(m, " ")
	m, cmd := press(m, " ")
	if cmd == nil {
		t.Fatal("space space did not start the run")
	}
	d := drawn(m, 2)
	if rowWith(d, "[ Cancel run ]") == "" || rowWith(d, "RUNNING") == "" {
		t.Errorf("the detail does not show the run at once:\n%s", strings.Join(d, "\n"))
	}
	if rowWith(drawn(m, 1), "second", "…") == "" {
		t.Errorf("the item row does not show the run at once:\n%s", strings.Join(drawn(m, 1), "\n"))
	}
	sameAsFresh(t, m, "the run starting")
	next, _ := m.Update(cmd())
	m = next.(model)
	if rowWith(drawn(m, 1), "second", "✓") == "" || !strings.Contains(statusLine(m), "finished") {
		t.Errorf("the finished run is not shown at once:\n%s", ansi.Strip(m.View()))
	}
	sameAsFresh(t, m, "the run finishing")
}

func TestBoxSelectionAndScrollShowAtOnce(t *testing.T) {
	trueColor(t)
	_, m := setup(t)
	m = withAtt(t, m, 100, 200, 198, 60) // result, prompt and attachment boxes all on screen
	step := func(what string, f func(model) model, want string) {
		t.Helper()
		before := m.View()
		m = f(m)
		if m.View() == before || (want != "" && !strings.Contains(ansi.Strip(m.View()), want)) {
			t.Fatalf("%s: the screen did not change at once (want %q):\n%s", what, want, ansi.Strip(m.View()))
		}
		sameAsFresh(t, m, what)
	}
	keys := func(k string) func(model) model { return func(m model) model { m, _ = press(m, k); return m } }
	step("}", keys("}"), "")                   // only the highlight moves to the prompt box
	step("]", keys("]"), "lines 13–24 of 100") // scrolls the selected prompt box
	step("{", keys("{"), "")
	step("]", keys("]"), "lines 25–48 of 201")
	step("[", keys("["), "lines 1–24 of 201")
	sw, iw, _ := columns(m.w)
	y := -1
	for i, l := range strings.Split(ansi.Strip(m.View()), "\n") {
		if y < 0 && strings.Contains(l, "│ p0") {
			y = i
		}
	}
	step("the wheel over the prompt box", func(m model) model { return mouseAt(m, sw+iw+5, y, tea.MouseButtonWheelDown) }, "lines 14–25 of 100")
	step("the wheel back up", func(m model) model { return mouseAt(m, sw+iw+5, y, tea.MouseButtonWheelUp) }, "lines 13–24 of 100")
}

func TestItemsScrollShowsAtOnce(t *testing.T) {
	trueColor(t)
	m := manyItems(t, 20) // 120x12: 8 item rows, item00 (the oldest pending) selected
	m.focus = 1
	i := 0
	for _, k := range strings.Split("jjjjjjjjjkkkkkkkkk", "") {
		m, _ = press(m, k)
		i = map[string]int{"j": min(19, i+1), "k": max(0, i-1)}[k]
		sel := fmt.Sprintf("item%02d", i)
		if rowWith(drawn(m, 2), sel) == "" || rowWith(drawn(m, 1), sel) == "" {
			t.Fatalf("after %s: %s not drawn in both panes:\n%s", k, sel, ansi.Strip(m.View()))
		}
		sameAsFresh(t, m, k)
	}
}
