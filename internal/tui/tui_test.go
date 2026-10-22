package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"github.com/nicholas1513/handoff/internal/queue"
)

const sid = "0b6c2a8e-1f3d-4c55-9a7e-2d4f6b8c0e12"

func TestSanitize(t *testing.T) {
	for in, want := range map[string]string{
		"plain\ttext\nline2":       "plain\ttext\nline2",
		"red\x1b[31mhidden\x1b[0m": `red\x1b[31mhidden\x1b[0m`,
		"back\rover":               `back\x0dover`,
		"rm -rf /" + string(rune(0x202e)) + "/ olleh": "rm -rf /\\u202e/ olleh",
		"zero" + string(rune(0x200b)) + "width":       "zero\\u200bwidth",
		"bell\a":                                      `bell\x07`,
	} {
		if got := sanitize(in); got != want {
			t.Errorf("sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}

func setup(t *testing.T) (*queue.Queue, model) {
	t.Helper()
	q := &queue.Queue{Root: t.TempDir()}
	q.WriteSession(queue.Session{SessionID: sid, Cwd: t.TempDir()})
	q.AddPrompt(queue.Prompt{SessionID: sid, TS: time.Now(), Prompt: "do the thing"})
	for _, d := range []string{"first", "second"} {
		if _, err := q.Add(queue.Item{Description: d, Argv: []string{"/usr/bin/true"}, Cwd: t.TempDir(), SessionID: sid}, nil); err != nil {
			t.Fatal(err)
		}
	}
	m := newModel(q, t.TempDir())
	m.ii, m.advancedAt = 1, time.Time{} // items list oldest first, so "second" (the newest) is index 1; startup picks the oldest, these tests start unguarded
	m.open()
	next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	return q, next.(model)
}

func click(m model, x, y int) (model, tea.Cmd) {
	next, cmd := m.Update(tea.MouseMsg{X: x, Y: y, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	return next.(model), cmd
}

func TestClickSelectsAndRunNeedsConfirm(t *testing.T) {
	q, m := setup(t)
	if m.item() == nil || m.item().Description != "second" {
		t.Fatalf("newest item should be selected first, got %+v", m.item())
	}
	sw, iw, _ := columns(m.w)

	m, _ = click(m, sw+3, listTop+1)
	if m.item().Description != "first" {
		t.Fatalf("click on row 2 selected %q", m.item().Description)
	}
	if !strings.Contains(m.View(), "do the thing") || !strings.Contains(m.View(), "verified") {
		t.Fatal("detail pane is missing the verified prompt")
	}

	dx := sw + iw
	m, cmd := click(m, dx+runX+1, buttonY)
	if !m.confirm || cmd != nil {
		t.Fatalf("clicking Run should only ask for confirmation (confirm=%v cmd=%v)", m.confirm, cmd != nil)
	}
	if s := q.State(*m.item(), time.Now()); s != queue.Pending {
		t.Fatalf("item ran without confirmation: %s", s)
	}

	ran := *m.item() // the selection auto-advances once the run finishes
	m, cmd = click(m, dx+yesX+1, buttonY)
	if cmd == nil {
		t.Fatal("confirm click did not start the run")
	}
	next, _ := m.Update(cmd())
	m = next.(model)
	if s := q.State(ran, time.Now()); s != queue.Succeeded {
		t.Fatalf("state after confirmed run = %s", s)
	}
}

func TestRejectAndInvalid(t *testing.T) {
	q, m := setup(t)
	rejected := *m.item()
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	m = next.(model)
	if s := q.State(rejected, time.Now()); s != queue.Rejected {
		t.Fatalf("state after x = %s", s)
	}
	if _, err := q.Snapshot("../nope"); err == nil {
		t.Fatal("unsafe id accepted")
	}
}

func press(m model, k string) (model, tea.Cmd) {
	msg := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
	if k == " " {
		msg = tea.KeyMsg{Type: tea.KeySpace, Runes: []rune(" ")}
	}
	next, cmd := m.Update(msg)
	return next.(model), cmd
}

func TestSpaceSpaceRuns(t *testing.T) {
	q, m := setup(t)
	m, cmd := press(m, " ")
	if !m.armed || cmd != nil || q.State(*m.item(), time.Now()) != queue.Pending {
		t.Fatal("one space must only arm")
	}
	v := m.View()
	if !strings.Contains(v, "space again to run") || strings.Contains(v, "Yes, run") {
		t.Fatal("armed prompt not shown, or a Yes button is")
	}
	m, cmd = press(m, " ")
	if cmd == nil {
		t.Fatal("second space did not start the run")
	}
}

func TestArmedOtherKeyCancelsAndIsNotActedOn(t *testing.T) {
	q, m := setup(t)
	m, _ = press(m, " ")
	m, _ = press(m, "x")
	if m.armed || q.State(*m.item(), time.Now()) != queue.Pending {
		t.Fatal("x while armed must cancel, not reject")
	}
}

func TestClickAtYesWhileKeyArmedDoesNotRun(t *testing.T) {
	_, m := setup(t)
	sw, iw, _ := columns(m.w)
	m, _ = press(m, " ")
	m, cmd := click(m, sw+iw+yesX+1, buttonY)
	if cmd != nil || m.armed {
		t.Fatal("a click must cancel a key arm, never run")
	}
}

func TestSpaceCancelsMouseConfirm(t *testing.T) {
	_, m := setup(t)
	sw, iw, _ := columns(m.w)
	m, _ = click(m, sw+iw+runX+1, buttonY)
	if !m.confirm {
		t.Fatal("run click should enter confirm")
	}
	m, cmd := press(m, " ")
	if cmd != nil || m.confirm || m.armed {
		t.Fatal("space must cancel the mouse confirm, never run")
	}
}

func TestKeysDroppedWhileRunning(t *testing.T) {
	_, m := setup(t)
	m, _ = press(m, " ")
	m.running = true
	m, cmd := press(m, " ")
	if !m.armed || cmd != nil {
		t.Fatal("space must be dropped while running")
	}
	m.focus, m.ii = 1, 0
	m, _ = press(m, "j")
	if m.ii != 0 {
		t.Fatal("navigation must be dropped while running")
	}
}

func TestArmThenNotPendingLeavesNoStuckArm(t *testing.T) {
	q, m := setup(t)
	m, _ = press(m, " ")
	if err := q.Claim(*m.item()); err != nil {
		t.Fatal(err)
	}
	m, cmd := press(m, " ")
	if m.armed || cmd != nil {
		t.Fatal("arm must clear and nothing run when the item is no longer pending")
	}
}

func TestRejectAdvancesToOldestPending(t *testing.T) {
	_, m := setup(t) // "second" (newest) is selected, "first" is older
	m, _ = press(m, "x")
	if m.item().Description != "first" {
		t.Fatalf("after reject selected %q, want oldest pending %q", m.item().Description, "first")
	}
}

func TestAdvanceSurvivesRefresh(t *testing.T) {
	_, m := setup(t)
	m, _ = press(m, "x")
	next, _ := m.Update(tickMsg(time.Now()))
	m = next.(model)
	if m.item().Description != "first" {
		t.Fatal("refresh tick moved the selection back")
	}
}

func TestNoAdvanceIfUserMovedAway(t *testing.T) {
	_, m := setup(t)
	ran := m.item().ID
	m.focus = 1
	m.move(-1) // user moves to "first" while the run is in flight
	m.advance(ran)
	if m.item().Description != "first" || !m.advancedAt.IsZero() {
		t.Fatal("advance must not fire when the selection moved off the item that ran")
	}
}

func TestSpaceIgnoredRightAfterAdvance(t *testing.T) {
	_, m := setup(t)
	clock := time.Now()
	m.now = func() time.Time { return clock }
	m, _ = press(m, "x") // advances
	m, cmd := press(m, " ")
	if m.armed || cmd != nil {
		t.Fatal("space within 1s of an auto-advance must be ignored")
	}
	m, _ = press(m, "r")
	if m.armed {
		t.Fatal("r within 1s of an auto-advance must be ignored")
	}
	clock = clock.Add(1100 * time.Millisecond)
	m, _ = press(m, " ")
	if !m.armed {
		t.Fatal("space after the guard should arm")
	}
}

func TestNothingPendingStays(t *testing.T) {
	_, m := setup(t)
	m, _ = press(m, "x") // advances to "first"
	at := m.advancedAt
	m, _ = press(m, "x") // nothing left
	if m.item().Description != "first" || !m.advancedAt.Equal(at) {
		t.Fatal("with nothing pending the selection and advancedAt must stay")
	}
}

func TestSpaceIgnoredAfterSelectedItemVanishes(t *testing.T) {
	q, m := setup(t)
	clock := time.Now()
	m.now = func() time.Time { return clock }
	if err := os.RemoveAll(filepath.Join(q.Root, "queue", m.item().ID)); err != nil {
		t.Fatal(err)
	}
	next, _ := m.Update(tickMsg(clock))
	m, _ = press(next.(model), " ")
	if m.item().Description != "first" || m.armed {
		t.Fatalf("space right after the selection fell back must be ignored (selected %q, armed %v)", m.item().Description, m.armed)
	}
}

func TestSpaceIgnoredAfterRunAdvance(t *testing.T) {
	q, m := setup(t)
	clock := time.Now()
	m.now = func() time.Time { return clock }
	it := *m.item()
	if err := q.Claim(it); err != nil {
		t.Fatal(err)
	}
	if err := q.WriteResult(it, queue.Result{State: queue.Succeeded, RanAt: clock}); err != nil {
		t.Fatal(err)
	}
	next, _ := m.Update(runDone{id: it.ID})
	m, _ = press(next.(model), " ")
	if m.armed || m.item().Description != "first" {
		t.Fatalf("space right after a run's advance must be ignored (selected %q, armed %v)", m.item().Description, m.armed)
	}
}

func rejectBoth(t *testing.T) (*queue.Queue, model) {
	q, m := setup(t)
	m, _ = press(m, "x")
	m, _ = press(m, "x")
	if m.pending() || m.item().Description != "first" {
		t.Fatal("setup: both items should be rejected with the selection on first")
	}
	return q, m
}

func tickM(m model) model {
	next, _ := m.Update(tickMsg(time.Now()))
	return next.(model)
}

func TestNewItemArrivingPullsSelectionFromFinished(t *testing.T) {
	q, m := rejectBoth(t)
	clock := time.Now()
	m.now = func() time.Time { return clock }
	if _, err := q.Add(queue.Item{Description: "third", Argv: []string{"/usr/bin/true"}, Cwd: t.TempDir(), SessionID: sid}, nil); err != nil {
		t.Fatal(err)
	}
	m = tickM(m)
	if m.item().Description != "third" {
		t.Fatalf("selected %q, want third", m.item().Description)
	}
	m, _ = press(m, " ")
	if m.armed {
		t.Fatal("space right after the pull must be ignored")
	}
	clock = clock.Add(1100 * time.Millisecond)
	m, _ = press(m, " ")
	if !m.armed {
		t.Fatal("space after the guard should arm")
	}
}

func TestNewItemDoesNotPullFromPending(t *testing.T) {
	q, m := setup(t) // "second" selected and pending
	if _, err := q.Add(queue.Item{Description: "third", Argv: []string{"/usr/bin/true"}, Cwd: t.TempDir(), SessionID: sid}, nil); err != nil {
		t.Fatal(err)
	}
	m = tickM(m)
	if m.item().Description != "second" {
		t.Fatalf("selected %q, want second", m.item().Description)
	}
}

func TestNewItemInOtherSessionDoesNotPull(t *testing.T) {
	q, m := rejectBoth(t)
	id, si := m.item().ID, m.si
	q.WriteSession(queue.Session{SessionID: "other", Cwd: t.TempDir()})
	if _, err := q.Add(queue.Item{Description: "elsewhere", Argv: []string{"/usr/bin/true"}, Cwd: t.TempDir(), SessionID: "other"}, nil); err != nil {
		t.Fatal(err)
	}
	m = tickM(m)
	if m.item() == nil || m.item().ID != id || m.session().id != sid {
		t.Fatalf("selection moved across sessions (si %d -> %d)", si, m.si)
	}
}

func TestFinishedItemsDimmed(t *testing.T) {
	old := lipgloss.ColorProfile()
	t.Cleanup(func() { lipgloss.SetColorProfile(old) })
	lipgloss.SetColorProfile(termenv.ANSI)
	_, m := setup(t)
	m, _ = press(m, "x") // rejects "second"; "first" stays pending
	s := m.session()
	var rej, pend string
	for _, it := range s.items {
		row := itemRow(it, m.q.State(it, time.Now()), "12:00")
		if it.Description == "second" {
			rej = row
		} else {
			pend = row
		}
	}
	const faint = "\x1b[2m"
	if !strings.Contains(rej, faint) || strings.Contains(pend, faint) {
		t.Fatalf("rejected %q, pending %q", rej, pend)
	}
	if !strings.Contains(rej, "31m⊘") || !strings.Contains(pend, "33m●") {
		t.Fatalf("glyphs not colored: rejected %q, pending %q", rej, pend)
	}
}

func TestStayOnFinishedWhileOthersPending(t *testing.T) {
	_, m := setup(t)
	m, _ = press(m, "x") // rejects "second", advances to "first"
	m.focus = 1
	m.move(1)
	if m.item().Description != "second" {
		t.Fatalf("setup: selected %q, want second", m.item().Description)
	}
	m = tickM(m)
	if m.item().Description != "second" {
		t.Fatalf("tick dragged selection to %q", m.item().Description)
	}
}

const sidB = "1c7d3b9f-2a4e-4d66-8b8f-3e5a7c9d1f23"

// twoSessions: A has a1 (oldest, pending), a2 (pending), a3 (newest, rejected); B (newer, so row 0) has one pending item.
func twoSessions(t *testing.T, finishAll bool) (*queue.Queue, model) {
	t.Helper()
	q := &queue.Queue{Root: t.TempDir()}
	add := func(s, d string, fin bool) {
		it, err := q.Add(queue.Item{Description: d, Argv: []string{"/usr/bin/true"}, Cwd: t.TempDir(), SessionID: s}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if fin {
			if err := q.Claim(it); err != nil {
				t.Fatal(err)
			}
			q.WriteResult(it, queue.Result{State: queue.Rejected, ExitCode: -1, RanAt: time.Now()})
		}
		time.Sleep(2 * time.Millisecond)
	}
	add(sid, "a1", finishAll)
	add(sid, "a2", finishAll)
	add(sid, "a3", true)
	add(sidB, "b1", finishAll)
	m := newModel(q, t.TempDir())
	next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	return q, next.(model)
}

func TestSwitchSessionSelectsOldestPending(t *testing.T) {
	_, m := twoSessions(t, false)
	m.si, m.ii, m.focus = 0, 0, 0
	m.open()
	if m.item().Description != "b1" || m.session().id != sidB {
		t.Fatalf("setup: want B/b1, got %+v", m.item())
	}
	m, _ = press(m, "j")
	if m.item().Description != "a1" {
		t.Fatalf("down into A: want a1, got %q", m.item().Description)
	}
	m, _ = press(m, "k")
	m, _ = press(m, "j")
	if m.item().Description != "a1" {
		t.Fatalf("re-enter A: want a1, got %q", m.item().Description)
	}
}

func TestClickSessionSelectsOldestPending(t *testing.T) {
	_, m := twoSessions(t, false)
	m.si, m.ii = 0, 0
	m.open()
	m, _ = click(m, 1, listTop+1)
	if m.session().id != sid || m.item().Description != "a1" {
		t.Fatalf("click A: want a1, got %+v", m.item())
	}
}

func TestSwitchSessionNoPendingSelectsNewest(t *testing.T) {
	_, m := twoSessions(t, true)
	m.si, m.ii, m.focus = 0, 0, 0
	m, _ = press(m, "j")
	if m.session().id != sid || m.ii != 0 || m.item().Description != "a3" || !m.advancedAt.IsZero() {
		t.Fatalf("no pending: want newest a3 at 0, got %d %q", m.ii, m.item().Description)
	}
}

func TestSwitchSessionGuard(t *testing.T) {
	_, m := twoSessions(t, false)
	clock := time.Now()
	m.now = func() time.Time { return clock }
	m.focus = 0
	m, _ = press(m, "j")
	m, _ = press(m, " ")
	if m.armed {
		t.Fatal("space right after switching must be ignored")
	}
	clock = clock.Add(1100 * time.Millisecond)
	m, _ = press(m, " ")
	if !m.armed {
		t.Fatal("space after the guard should arm")
	}
}

func TestStartupSelectsOldestPending(t *testing.T) {
	q := &queue.Queue{Root: t.TempDir()}
	for _, d := range []string{"a1", "a2", "a3"} {
		if _, err := q.Add(queue.Item{Description: d, Argv: []string{"/usr/bin/true"}, Cwd: t.TempDir(), SessionID: sid}, nil); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	m := newModel(q, t.TempDir())
	if m.item().Description != "a1" || m.advancedAt.IsZero() {
		t.Fatalf("startup: want oldest pending a1 with advancedAt set, got %q", m.item().Description)
	}
}

// sections: p1, p2 pending (p1 older); f1 finished at base+1m, f2 at base+2m.
func sections(t *testing.T, finished bool) (*queue.Queue, model, time.Time) {
	t.Helper()
	q := &queue.Queue{Root: t.TempDir()}
	base := time.Date(2020, 1, 2, 9, 0, 0, 0, time.Local)
	for i, d := range []string{"p1", "f1", "p2", "f2"} {
		it, err := q.Add(queue.Item{Description: d, Argv: []string{"/usr/bin/true"}, Cwd: t.TempDir(), SessionID: sid}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if d[0] == 'f' {
			if !finished {
				continue
			}
			q.Claim(it)
			q.WriteResult(it, queue.Result{State: queue.Succeeded, RanAt: base.Add(time.Duration(i) * time.Minute)})
		}
		time.Sleep(2 * time.Millisecond)
	}
	m := newModel(q, t.TempDir())
	next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	return q, next.(model), base
}

func paneRows(m model) []string {
	rows, _ := m.itemPane(m.session(), m.now())
	for i := range rows {
		rows[i] = ansi.Strip(rows[i])
	}
	return rows
}

func TestItemsSectionsOrder(t *testing.T) {
	_, m, _ := sections(t, true)
	want := []string{"Outstanding (2)", "p1", "p2", "Completed (2)", "f2", "f1"}
	rows := paneRows(m)
	if len(rows) != len(want) {
		t.Fatalf("rows %q", rows)
	}
	for i, w := range want {
		if !strings.HasSuffix(rows[i], w) {
			t.Fatalf("row %d = %q, want ...%q (all %q)", i, rows[i], w, rows)
		}
	}
}

func TestItemsHeaderHiddenWhenEmpty(t *testing.T) {
	_, m, _ := sections(t, false)
	for _, r := range paneRows(m) {
		if strings.Contains(r, "Completed") {
			t.Fatalf("Completed header with no completed items: %q", r)
		}
	}
}

func TestItemsTimestamps(t *testing.T) {
	_, m, _ := sections(t, true)
	m.now = func() time.Time { return time.Date(2020, 1, 3, 10, 0, 0, 0, time.Local) }
	rows := paneRows(m)
	if !strings.HasPrefix(rows[4], "Jan 2 09:03  ✓ f2") || !strings.HasPrefix(rows[5], "Jan 2 09:01  ✓ f1") {
		t.Fatalf("completed stamps: %q", rows)
	}
	c := m.session().items[0].Created
	m.now = func() time.Time { return c }
	if rows := paneRows(m); !strings.HasPrefix(rows[1], c.Local().Format("15:04")+"  ● p1") {
		t.Fatalf("today stamp: %q", rows[1])
	}
}

func TestClickMapsPastHeaders(t *testing.T) {
	_, m, _ := sections(t, true)
	sw, _, _ := columns(m.w)
	m, _ = click(m, sw+3, listTop+2)
	if m.item().Description != "p2" {
		t.Fatalf("click selected %q, want p2", m.item().Description)
	}
	m.focus = 0
	m, _ = click(m, sw+3, listTop+3) // the Completed header
	if m.item().Description != "p2" || m.focus != 0 {
		t.Fatalf("header click not inert: %q focus %d", m.item().Description, m.focus)
	}
	m, _ = click(m, sw+3, listTop+4)
	if m.item().Description != "f2" {
		t.Fatalf("click selected %q, want f2", m.item().Description)
	}
}

func TestClickStableWhenItemsExpireAfterPaint(t *testing.T) {
	_, m, _ := sections(t, false) // p1, p2 pending
	sw, _, _ := columns(m.w)
	want := paneRows(m) // header, p1, p2
	m.now = func() time.Time { return time.Now().Add(100 * 24 * time.Hour) }
	for y, w := range want {
		m, _ = click(m, sw+3, listTop+y)
		got := m.item().Description
		if y == 0 && got != "p1" && got != "p2" { // header leaves the previous selection
			t.Fatalf("header click selected %q", got)
		}
		if y > 0 && !strings.HasSuffix(w, got) {
			t.Fatalf("row %d drawn %q but selected %q", y, w, got)
		}
	}
}

func TestClickBelowDrawnListDoesNothing(t *testing.T) {
	_, m, _ := sections(t, true)                                  // 6 rows
	next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 8}) // h=5: 4 rows drawn
	m = next.(model)
	sw, _, _ := columns(m.w)
	before := m.item().ID
	m, _ = click(m, sw+3, listTop+4)
	if m.item().ID != before {
		t.Fatal("click below the last drawn row selected an undrawn item")
	}
	m, _ = click(m, 1, listTop+4)
	if m.si != 0 {
		t.Fatal("sessions click below the list moved the session")
	}
}

func TestSectionHeadersShareOneStyle(t *testing.T) {
	old := lipgloss.ColorProfile()
	t.Cleanup(func() { lipgloss.SetColorProfile(old) })
	lipgloss.SetColorProfile(termenv.TrueColor)
	_, m := setup(t)
	rows, _ := m.itemPane(m.session(), time.Now())
	if rows[0] != section.Render("Outstanding (2)") {
		t.Fatalf("items header %q", rows[0])
	}
	if !strings.Contains(heading("Command"), section.Render("Command")) {
		t.Fatalf("detail heading %q", heading("Command"))
	}
	if section.GetForeground() != lipgloss.Color("#7aa2f7") {
		t.Fatalf("section color %v", section.GetForeground())
	}
}

func statusLine(m model) string { v := m.View(); return v[strings.LastIndex(v, "\n")+1:] }

func TestKeyHelpOnlyForPendingItemInFocus(t *testing.T) {
	_, m := setup(t)
	m.focus = 0
	if l := statusLine(m); strings.Contains(l, "space space run") || !strings.Contains(l, "q quit") {
		t.Fatalf("sessions focus: %q", l)
	}
	for _, f := range []int{1, 2} {
		m.focus = f
		if l := statusLine(m); !strings.Contains(l, "space space run") || !strings.Contains(l, "x reject") {
			t.Fatalf("focus %d on pending item: %q", f, l)
		}
	}
	m.focus = 1
	m, _ = press(m, "x") // rejects "second", advances to pending "first"
	if l := statusLine(m); !strings.Contains(l, "rejected") || !strings.Contains(l, "space space run") {
		t.Fatalf("after reject, on pending: %q", l)
	}
	m, _ = press(m, "x") // rejects "first"; nothing pending, selection stays on it
	if l := statusLine(m); strings.Contains(l, "space space run") || strings.Contains(l, "x reject") || !strings.Contains(l, "q quit") {
		t.Fatalf("on finished item: %q", l)
	}
	if strings.Contains(statusLine(m), "o re-open") {
		t.Fatal("o re-open shown without a change on disk")
	}
}

// manyItems stages n pending items "item00".. in one session; Height 12 leaves 8 item rows.
func manyItems(t *testing.T, n int) model {
	t.Helper()
	q := &queue.Queue{Root: t.TempDir()}
	q.WriteSession(queue.Session{SessionID: sid, Cwd: t.TempDir()})
	for i := 0; i < n; i++ {
		if _, err := q.Add(queue.Item{Description: fmt.Sprintf("item%02d", i), Argv: []string{"/usr/bin/true"}, Cwd: t.TempDir(), SessionID: sid}, nil); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	next, _ := newModel(q, t.TempDir()).Update(tea.WindowSizeMsg{Width: 120, Height: 12})
	return next.(model)
}

// itemsCol is the drawn items-pane line at screen row y, stripped, without its border.
func itemsCol(m model, y int) string {
	sw, iw, _ := columns(m.w)
	line := strings.Split(m.View(), "\n")[y]
	return strings.TrimSpace(ansi.Strip(ansi.Cut(line, sw+1, sw+iw-1)))
}

func drawnItems(m model) (out []string) {
	for y := listTop; y < max(5, m.h-3)+1; y++ {
		out = append(out, itemsCol(m, y))
	}
	return
}

func TestItemsPaneScrollsToSelection(t *testing.T) {
	m := manyItems(t, 20)
	m.focus = 1
	for i := 0; i < 12; i++ {
		m, _ = press(m, "j")
	}
	rows := drawnItems(m)
	if !strings.Contains(strings.Join(rows, "\n"), "item12") || !strings.HasPrefix(rows[0], "↑ ") || !strings.HasSuffix(rows[0], " more") {
		t.Fatalf("want item12 drawn under an up indicator: %q", rows)
	}
	for i := 0; i < 30; i++ { // and back to the top: no up indicator, first rows drawn
		m, _ = press(m, "k")
	}
	if rows = drawnItems(m); strings.Contains(rows[0], "↑") || !strings.Contains(rows[1], "item00") {
		t.Fatalf("top not restored: %q", rows)
	}
}

func TestClickOnScrolledListSelectsDrawnItem(t *testing.T) {
	m := manyItems(t, 20)
	m.focus = 1
	for i := 0; i < 15; i++ {
		m, _ = press(m, "j")
	}
	sw, _, _ := columns(m.w)
	for y := listTop; y < max(5, m.h-3)+1; y++ {
		drawn := itemsCol(m, y)
		before := m.item().Description
		m, _ = click(m, sw+3, y)
		got := m.item().Description
		if strings.Contains(drawn, "more") || strings.Contains(drawn, "Outstanding") {
			if got != before {
				t.Fatalf("row %d (%q) is inert but selection %q -> %q", y, drawn, before, got)
			}
		} else if !strings.HasSuffix(drawn, got) {
			t.Fatalf("row %d drawn %q but selected %q", y, drawn, got)
		}
	}
}

func TestAutoJumpKeepsSelectionVisible(t *testing.T) {
	m := manyItems(t, 20)
	for _, it := range m.session().items { // all finished: completed list, 20 rows
		m.q.Claim(it)
		m.q.WriteResult(it, queue.Result{State: queue.Succeeded, RanAt: time.Now()})
	}
	m.refresh()
	m.focus = 1
	for i := 0; i < 25; i++ {
		m, _ = press(m, "j") // scrolled to the bottom of the list
	}
	if _, err := m.q.Add(queue.Item{Description: "fresh", Argv: []string{"/usr/bin/true"}, Cwd: t.TempDir(), SessionID: sid}, nil); err != nil {
		t.Fatal(err)
	}
	next, _ := m.Update(tickMsg(time.Now())) // the new pending item pulls the selection to the top
	m = next.(model)
	if m.item().Description != "fresh" {
		t.Fatalf("selected %q", m.item().Description)
	}
	if rows := drawnItems(m); !strings.Contains(strings.Join(rows, "\n"), "fresh") || strings.Contains(rows[0], "↑") {
		t.Fatalf("selected row not drawn: %q", rows)
	}
}

// withResult stages an item whose result holds stdout of n lines "out001".. and returns the model on it.
func withResult(t *testing.T, m model, n int) model {
	t.Helper()
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "out%03d\n", i)
	}
	b.WriteString("out999" + strings.Repeat("x", 120) + "\n") // wider than the pane: wraps to 3 lines
	it, err := m.q.Add(queue.Item{Description: "res", Argv: []string{"/usr/bin/true"}, Cwd: t.TempDir(), SessionID: sid}, nil)
	if err != nil {
		t.Fatal(err)
	}
	m.q.Claim(it)
	m.q.WriteResult(it, queue.Result{State: queue.Succeeded, RanAt: time.Now(), Stdout: b.String()})
	nm := newModel(m.q, m.home)
	for i, x := range nm.session().items {
		if x.ID == it.ID {
			nm.ii = i
		}
	}
	nm.open()
	next, _ := nm.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	return next.(model)
}

func outLines(m model) (n int, footer string) {
	for _, l := range strings.Split(ansi.Strip(m.detail()), "\n") {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "out") {
			n++
		}
		if strings.HasPrefix(l, "lines ") {
			footer = l
		}
	}
	return
}

func TestResultBoxCapped(t *testing.T) {
	_, m := setup(t)
	m = withResult(t, m, 200)
	m.focus = 2
	c := 40 * 40 / 100
	// the "stdout" label takes one box line and the long line wraps to 3: 200+1+3 = 204 lines
	if n, f := outLines(m); n > c || f != fmt.Sprintf("lines 1–%d of 204", c) {
		t.Fatalf("n=%d footer=%q", n, f)
	}
	m, _ = press(m, "]")
	if _, f := outLines(m); f != fmt.Sprintf("lines %d–%d of 204", c+1, 2*c) {
		t.Fatalf("after ]: %q", f)
	}
	m, _ = press(m, "[")
	if _, f := outLines(m); f != fmt.Sprintf("lines 1–%d of 204", c) {
		t.Fatalf("after [: %q", f)
	}
	for i := 0; i < 40; i++ { // clamps at the end
		m, _ = press(m, "]")
	}
	if _, f := outLines(m); f != "lines 189–204 of 204" {
		t.Fatalf("end: %q", f)
	}
	m, _ = press(m, "]")
	m.focus = 1
	m, _ = press(m, "k") // another item opens: back to line 1
	if m.outTop != 0 {
		t.Fatalf("outTop %d after opening another item", m.outTop)
	}
}

func TestResultBoxWheelAndPgKeys(t *testing.T) {
	_, m := setup(t)
	m = withResult(t, m, 200)
	m.focus = 2
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyPgDown})
	m = next.(model)
	if m.outTop != 16 {
		t.Fatalf("pgdown outTop %d", m.outTop)
	}
	_, iw, _ := columns(m.w)
	sw, _, _ := columns(m.w)
	wheel := func(m model, y int) model {
		n, _ := m.Update(tea.MouseMsg{X: sw + iw + 5, Y: y, Action: tea.MouseActionPress, Button: tea.MouseButtonWheelDown})
		return n.(model)
	}
	y := -1
	for i, l := range strings.Split(ansi.Strip(m.View()), "\n") {
		if strings.Contains(l, "out0") && y < 0 {
			y = i
		}
	}
	if y < 0 {
		t.Fatal("box not drawn")
	}
	if got := wheel(m, y).outTop; got != 19 {
		t.Fatalf("wheel over box outTop %d, want 19", got)
	}
	if got := wheel(m, y-1).outTop; got != 16 { // the exit line just above the box
		t.Fatalf("wheel above the box moved outTop to %d", got)
	}
	if got := wheel(m, 2).outTop; got != 16 {
		t.Fatalf("wheel off box moved outTop to %d", got)
	}
}

func TestOnlyResultIsCapped(t *testing.T) {
	_, m := setup(t)
	var b strings.Builder
	for i := 1; i <= 200; i++ {
		fmt.Fprintf(&b, "att%03d\n", i)
	}
	src := filepath.Join(t.TempDir(), "a.txt")
	os.WriteFile(src, []byte(b.String()), 0o600)
	desc, val := "desc-"+strings.Repeat("d", 30)+"-end", "https://example.com/"+strings.Repeat("v", 30)+"/vend"
	m = withItem(t, m, queue.Item{Description: desc, Argv: []string{"/usr/bin/true", "--url", val, "--body", "@{file:a.txt}"}}, map[string]string{"a.txt": src}, 120)
	it := *m.item()
	var o strings.Builder
	for i := 1; i <= 200; i++ {
		fmt.Fprintf(&o, "out%03d\n", i)
	}
	m.q.Claim(it)
	m.q.WriteResult(it, queue.Result{State: queue.Succeeded, RanAt: time.Now(), Stdout: o.String()})
	d := ansi.Strip(m.detail())
	for i := 1; i <= 200; i++ {
		if !strings.Contains(d, fmt.Sprintf("att%03d", i)) {
			t.Fatalf("attachment line %d missing from the detail", i)
		}
	}
	if want := fmt.Sprintf("lines 1–%d of 201", m.outCap()); !strings.Contains(d, want) {
		t.Fatalf("result box not capped, want %q", want)
	}
	if f := flatten(m.detail()); !strings.Contains(f, desc) || !strings.Contains(f, val) {
		t.Fatal("description or argv value cut")
	}
}

func TestBracketWhileArmedOnlyCancels(t *testing.T) {
	_, m := setup(t)
	m.focus, m.outTop = 2, 5
	m, _ = press(m, " ")
	if !m.armed {
		t.Fatal("not armed")
	}
	m, _ = press(m, "]")
	if m.armed || m.outTop != 5 {
		t.Fatalf("armed=%v outTop=%d", m.armed, m.outTop)
	}
}
