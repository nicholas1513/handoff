package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
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
	m.move(1) // user moves to "first" while the run is in flight
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
		row := itemRow(it, m.q.State(it, time.Now()))
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
}

func TestStayOnFinishedWhileOthersPending(t *testing.T) {
	_, m := setup(t)
	m, _ = press(m, "x") // rejects "second", advances to "first"
	m.focus = 1
	m.move(-1)
	if m.item().Description != "second" {
		t.Fatalf("setup: selected %q, want second", m.item().Description)
	}
	m = tickM(m)
	if m.item().Description != "second" {
		t.Fatalf("tick dragged selection to %q", m.item().Description)
	}
}
