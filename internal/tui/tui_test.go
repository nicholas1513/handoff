package tui

import (
	"errors"
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

// TestAgentTextIsDrawnSanitized: text an agent can choose reaches the screen sanitized, and a newline in it can't add
// a screen row: an invalid queue entry's name, a run's error (it can name an edit's target) and the rules badge (it
// names the project's settings file).
func TestAgentTextIsDrawnSanitized(t *testing.T) {
	q, m := setup(t)
	cwd := filepath.Join(t.TempDir(), "p\x1b]0;pwned\a")
	if err := os.MkdirAll(filepath.Join(cwd, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(cwd, ".claude", "settings.json"), []byte(`{"permissions":{"allow":["Bash"]}}`), 0o600)
	q.WriteSession(queue.Session{SessionID: sid, Cwd: cwd})
	if err := os.Mkdir(filepath.Join(q.Root, "queue", "bad\nentry\x1b]0;pwned\a"), 0o700); err != nil {
		t.Fatal(err)
	}
	next, _ := m.Update(runDone{id: "gone", err: errors.New("/tmp/t\x1b]0;pwned\a changed since the diff was taken")})
	m = next.(model) // runDone refreshes, so the new session cwd's badge is drawn too
	if v := m.View(); strings.Contains(v, "\x1b]") || !strings.Contains(ansi.Strip(v), "allowed for Claude") {
		t.Errorf("the badge or the run's error is drawn raw, or no badge is drawn:\n%q", v)
	}
	m, _ = press(m, "j") // onto the invalid entries
	if v := m.View(); strings.Contains(v, "\x1b]") || len(strings.Split(v, "\n")) != m.h {
		t.Errorf("an invalid entry's name is drawn raw:\n%q", v)
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

// TestPromptsOnlyForWhatIsDrawn: until the next refresh the disk can say what the screen doesn't (a revoke undone,
// another viewer starting the item). Space, c and the button row then arm nothing, so a second press can't answer a
// prompt that was never drawn.
func TestPromptsOnlyForWhatIsDrawn(t *testing.T) {
	q, m := setup(t)
	sw, iw, _ := columns(m.w)
	it := *m.item()
	if err := q.Revoke(it, sid); err != nil {
		t.Fatal(err)
	}
	m = tickM(m) // drawn as revoked
	if err := os.Remove(filepath.Join(q.Root, "queue", it.ID, "revoked")); err != nil {
		t.Fatal(err)
	}
	m2, _ := press(m, " ")
	if _, cmd := press(m2, " "); cmd != nil {
		t.Error("space space ran an item drawn as revoked")
	}
	m2, _ = click(m, sw+iw+runX+1, buttonY)
	if _, cmd := click(m2, sw+iw+yesX+1, buttonY); cmd != nil {
		t.Error("two clicks on the button row ran an item drawn as revoked")
	}

	for what, do := range map[string]func(model){
		"c c": func(m model) { m, _ = press(m, "c"); press(m, "c") },
		"two clicks on the button row": func(m model) {
			m, _ = click(m, sw+iw+runX+1, buttonY)
			click(m, sw+iw+yesCancelX+1, buttonY)
		},
	} {
		q, m := setup(t)
		it := *m.item()
		if err := q.Claim(it); err != nil { // drawn as pending until the next tick
			t.Fatal(err)
		}
		if do(m); q.State(it, time.Now()) != queue.Running {
			t.Errorf("%s on an item drawn as pending canceled another viewer's run", what)
		}
	}
}

// TestRejectOnlyWhatIsDrawn: like the prompts, x and the [ Reject ] click act only on an item the screen shows as
// pending, not on one drawn as revoked whose revoke was undone on disk since the last refresh.
func TestRejectOnlyWhatIsDrawn(t *testing.T) {
	q, m := setup(t)
	sw, iw, _ := columns(m.w)
	it := *m.item()
	if err := q.Revoke(it, sid); err != nil {
		t.Fatal(err)
	}
	m = tickM(m) // drawn as revoked
	if err := os.Remove(filepath.Join(q.Root, "queue", it.ID, "revoked")); err != nil {
		t.Fatal(err)
	}
	if press(m, "x"); q.State(it, time.Now()) != queue.Pending {
		t.Error("x rejected an item drawn as revoked")
	}
	if click(m, sw+iw+rejectX+1, buttonY); q.State(it, time.Now()) != queue.Pending {
		t.Error("a click on the button row rejected an item drawn as revoked")
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
	m = tickM(m)
	if m.item().Description != "first" {
		t.Fatal("refresh tick moved the selection back")
	}
}

// TestLoadBeforeARejectIsReadAgain: a tick's load that read the queue before this viewer rejected an item would draw
// the item pending again, so it is dropped and the queue read again.
func TestLoadBeforeARejectIsReadAgain(t *testing.T) {
	q, m := setup(t)
	addItem(t, q, queue.Item{Description: "third", SessionID: sid}, nil) // so the load has new rows to apply
	next, cmd := m.Update(tickMsg(time.Now()))
	l := cmd()
	m, _ = press(next.(model), "x") // rejects "second", which the load read as pending
	next, cmd = m.Update(l)
	if rows := drawn(next.(model), 1); rowWith(rows, "second", "⊘") == "" {
		t.Fatalf("a load read before the reject drew the item pending again:\n%s", strings.Join(rows, "\n"))
	}
	next, _ = next.Update(cmd())
	if rows := drawn(next.(model), 1); rowWith(rows, "third") == "" || rowWith(rows, "second", "⊘") == "" {
		t.Fatalf("the queue was not read again:\n%s", strings.Join(rows, "\n"))
	}
}

// TestLoadBeforeARecheckIsReadAgain: a click in the items pane re-checks the open item. A tick's load that read the
// item before it changed on disk would take the drawn warning back off the screen and let space space use the item up,
// so it is dropped and the queue read again.
func TestLoadBeforeARecheckIsReadAgain(t *testing.T) {
	for what, y := range map[string]int{"the open item's row": listTop + 2, "blank space": listTop + 20} {
		q, m := setup(t)
		sw, _, _ := columns(m.w)
		it := *m.item()
		next, cmd := m.Update(tickMsg(time.Now()))
		l := cmd() // reads the item before it changes
		p := filepath.Join(q.Root, "queue", it.ID, "item.json")
		b, err := os.ReadFile(p)
		if err == nil {
			err = os.WriteFile(p, append(b, '\n'), 0o600)
		}
		if err != nil {
			t.Fatal(err)
		}
		m, _ = click(next.(model), sw+3, y)
		if v := ansi.Strip(m.View()); m.item().ID != it.ID || !strings.Contains(v, "Changed on disk") {
			t.Fatalf("a click on %s did not re-check the open item:\n%s", what, v)
		}
		next, _ = m.Update(l)
		if v := ansi.Strip(next.View()); !strings.Contains(v, "Changed on disk") {
			t.Errorf("click on %s: a load read before the change took the warning off the screen:\n%s", what, v)
		}
		m, _ = press(next.(model), " ")
		if _, cmd := press(m, " "); cmd != nil {
			t.Errorf("click on %s: space space ran an item drawn as changed on disk", what)
		}
	}
}

// TestApplyingALoadReadsNoFiles: the tick reads the disk in its load, off the event loop; applying the load reads
// nothing, so it shows what was read even with the queue gone.
func TestApplyingALoadReadsNoFiles(t *testing.T) {
	q, m := setup(t)
	finished(t, q, *m.item(), queue.Succeeded, "hello-result\n")
	next, cmd := m.Update(tickMsg(time.Now()))
	l := cmd()
	if err := os.RemoveAll(q.Root); err != nil {
		t.Fatal(err)
	}
	if next, _ = next.Update(l); rowWith(drawn(next.(model), 2), "hello-result") == "" {
		t.Fatalf("applying the load read the removed queue:\n%s", strings.Join(drawn(next.(model), 2), "\n"))
	}
}

// TestLoadRunsBesideTheEventLoop: the tick's load runs while the loop handles input and draws (go test -race), and it
// writes nothing the loop reads, such as the title cache when a session's title changed.
func TestLoadRunsBesideTheEventLoop(t *testing.T) {
	q, m := setup(t)
	tr := tempFile(t, "t.jsonl", `{"type":"ai-title","aiTitle":"one"}`)
	q.WriteSession(queue.Session{SessionID: sid, Cwd: t.TempDir(), TranscriptPath: tr})
	m = tickM(m)
	os.WriteFile(tr, []byte(`{"type":"ai-title","aiTitle":"retitled"}`), 0o600)
	next, cmd := m.Update(tickMsg(time.Now()))
	done := make(chan tea.Msg)
	go func() { done <- cmd() }()
	m = next.(model)
	m.focus = 1
	for _, k := range []string{"k", "j", "x", "o"} {
		m, _ = press(m, k)
		m.View()
	}
	l := <-done
	if m.titles[tr].title != "one" {
		t.Fatal("the load wrote the event loop's title cache")
	}
	next, cmd = m.Update(l) // read before x and o: read again
	next, _ = next.Update(cmd())
	if v := ansi.Strip(next.View()); !strings.Contains(v, "retitled") {
		t.Fatalf("the new title is not drawn:\n%s", v)
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
	m, _ = press(tickM(m), " ")
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

// tickM is a refresh tick as the program runs it: the tick issues a load, whose result is then applied.
func tickM(m model) model {
	next, cmd := m.Update(tickMsg(time.Now()))
	next, _ = next.Update(cmd())
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

// ghosttyClick is one wheel click as Ghostty sends it: 3 wheel events in one write (measured under 0.3 ms apart).
func ghosttyClick(m model, x, y int, b tea.MouseButton) model {
	for range 3 {
		m = mouseAt(m, x, y, b)
	}
	return m
}

func TestWheelClickMovesTheListOneItem(t *testing.T) {
	m := manyItems(t, 20)
	clock := time.Now()
	m.now = func() time.Time { return clock }
	sw, iw, _ := columns(m.w)
	start := m.ii
	m = ghosttyClick(m, sw+3, listTop+1, tea.MouseButtonWheelDown)
	if m.ii != start+1 {
		t.Fatalf("one click moved the items selection %d, want 1", m.ii-start)
	}
	clock = clock.Add(10 * time.Millisecond) // the next click
	m = ghosttyClick(m, sw+3, listTop+1, tea.MouseButtonWheelDown)
	m = ghosttyClick(m, sw+iw-3, listTop+1, tea.MouseButtonWheelUp) // same instant, other direction: a new click
	if m.ii != start+1 {
		t.Fatalf("down then up moved the selection %d, want 1", m.ii-start)
	}
}

func TestWheelClickScrollsABoxThreeLines(t *testing.T) {
	_, m := setup(t)
	m = withResult(t, m, 200)
	sw, iw, _ := columns(m.w)
	y := -1
	for i, l := range strings.Split(ansi.Strip(m.View()), "\n") {
		if strings.Contains(l, "out0") && y < 0 {
			y = i
		}
	}
	if y < 0 {
		t.Fatal("box not drawn")
	}
	if got := ghosttyClick(m, sw+iw+5, y, tea.MouseButtonWheelDown).boxTop[resultBox]; got != 3 {
		t.Fatalf("one click scrolled the box %d lines, want 3 (one per event)", got)
	}
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
	m = tickM(m) // the new pending item pulls the selection to the top
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
		if i := strings.Index(l, "lines "); i >= 0 && strings.HasPrefix(l, "╰") {
			footer = strings.TrimRight(l[i:], " ─╯") // in the box's bottom border
		}
		l = strings.Trim(l, "│┃ ")
		if strings.HasPrefix(l, "out") {
			n++
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
	if m.boxTop[resultBox] != 0 {
		t.Fatalf("result top %d after opening another item", m.boxTop[resultBox])
	}
}

func TestResultBoxWheelAndPgKeys(t *testing.T) {
	_, m := setup(t)
	m = withResult(t, m, 200)
	m.focus = 2
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyPgDown})
	m = next.(model)
	if m.boxTop[resultBox] != 16 {
		t.Fatalf("pgdown result top %d", m.boxTop[resultBox])
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
	if got := wheel(m, y).boxTop[resultBox]; got != 17 {
		t.Fatalf("wheel over box result top %d, want 17", got)
	}
	if got := wheel(m, y-1).boxTop[resultBox]; got != 16 { // the box's top border
		t.Fatalf("wheel on the top border moved the result top to %d", got)
	}
	if got := wheel(m, y-2).boxTop[resultBox]; got != 16 { // the exit line above the border
		t.Fatalf("wheel above the box moved the result top to %d", got)
	}
	if got := wheel(m, y+m.capPct(40)).boxTop[resultBox]; got != 16 { // the bottom border
		t.Fatalf("wheel on the bottom border moved the result top to %d", got)
	}
	if got := wheel(m, y+m.capPct(40)-1).boxTop[resultBox]; got != 17 { // last content row
		t.Fatalf("wheel on the last box row result top %d, want 19", got)
	}
	if got := wheel(m, 2).boxTop[resultBox]; got != 16 {
		t.Fatalf("wheel off box moved the result top to %d", got)
	}
}

func TestOnlyBoxesAreCapped(t *testing.T) {
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
	m = tickM(m) // drawing reads snapshots: a result written after open shows at the next tick
	d := ansi.Strip(m.detail())
	for _, want := range []string{fmt.Sprintf("lines 1–%d of 201", m.capPct(40)), fmt.Sprintf("lines 1–%d of 200", m.capPct(30))} {
		if !strings.Contains(d, want) {
			t.Fatalf("result or attachment box not capped, want %q", want)
		}
	}
	if f := flatDetail(m); !strings.Contains(f, desc) || !strings.Contains(f, val) {
		t.Fatal("description or argv value cut")
	}
}

func TestBracketWhileArmedOnlyCancels(t *testing.T) {
	_, m := setup(t)
	m.focus, m.boxTop = 2, map[string]int{resultBox: 5}
	m, _ = press(m, " ")
	if !m.armed {
		t.Fatal("not armed")
	}
	m, _ = press(m, "]")
	if m.armed || m.boxTop[resultBox] != 5 {
		t.Fatalf("armed=%v top=%v", m.armed, m.boxTop)
	}
}

func TestResultComesFirstForFinishedItem(t *testing.T) {
	_, m := setup(t)
	d := ansi.Strip(m.detail())
	if strings.Contains(d, "── Result") || strings.Index(d, "── Command") < 0 {
		t.Fatal("pending item: want Command and no Result")
	}
	if strings.Index(d, "── Command") > strings.Index(d, "── Why") {
		t.Fatal("pending order changed")
	}
	m = withResult(t, m, 200)
	d = ansi.Strip(m.detail())
	r, f, c := strings.Index(d, "── Result"), strings.Index(d, "lines 1–"), strings.Index(d, "── Command")
	if r < 0 || f < 0 || c < 0 || r > f || f > c {
		t.Fatalf("order: result %d footer %d command %d", r, f, c)
	}
}

func TestResultBoxVisibleOnScreen(t *testing.T) {
	_, m := setup(t)
	url := "https://management.azure.com/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg-example/providers/Microsoft.Web/sites/app-example/config/appsettings?api-version=2023-01-01"
	m = withItem(t, m, queue.Item{Description: "list app settings", Reason: "check config", Argv: []string{"az", "rest", "--method", "get", "--url", url}}, nil, 160)
	it := *m.item()
	var o strings.Builder
	for i := 1; i <= 301; i++ {
		fmt.Fprintf(&o, "out%03d\n", i)
	}
	m.q.Claim(it)
	m.q.WriteResult(it, queue.Result{State: queue.Succeeded, RanAt: time.Now(), Stdout: o.String()})
	m = tickM(m) // drawing reads snapshots: a result written after open shows at the next tick
	next, _ := m.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	m = next.(model)
	if !strings.Contains(ansi.Strip(m.View()), "lines 1–") {
		t.Fatal("result footer not on screen without scrolling")
	}
}

func TestDetailScrollBoundedWhenContentFits(t *testing.T) {
	_, m := setup(t)
	m.focus = 2
	v := m.View()
	for i := 0; i < 5; i++ {
		m, _ = press(m, "j")
	}
	if m.scroll != 0 || m.View() != v {
		t.Fatalf("scroll %d on content that fits", m.scroll)
	}
}

func TestDetailScrollStopsAtEnd(t *testing.T) {
	_, m := setup(t)
	m = withItem(t, m, queue.Item{Description: "long", Argv: []string{"/usr/bin/true", "--body", "@{file:a.txt}"}}, map[string]string{"a.txt": longFile(t, 200)}, 120)
	m.focus = 2
	scrollToEnd(t, m, m.detailLines())
}

func TestDetailScrollStopsAtEndOfLongError(t *testing.T) {
	_, m := setup(t)
	m.openErr = errors.New(strings.Repeat("word ", 1000) + "THE-END")
	m.focus = 2
	scrollToEnd(t, m, m.detailLines())
}

// scrollToEnd presses down far past the end: scroll must stop at lines-(visible body), and the last real line must be drawn.
func scrollToEnd(t *testing.T, m model, lines []string) {
	t.Helper()
	for i := 0; i < 400; i++ {
		m, _ = press(m, "j")
	}
	want := len(lines) - (max(5, m.h-3) - 2)
	if m.scroll != want || want <= 0 {
		t.Fatalf("scroll %d, want %d", m.scroll, want)
	}
	last := strings.TrimSpace(ansi.Strip(lines[len(lines)-1]))
	if last == "" || !strings.Contains(ansi.Strip(m.View()), last) {
		t.Fatalf("last detail line %q not on screen", last)
	}
}

func columnsDW(m model) int { _, _, dw := columns(m.w); return dw }

func longFile(t *testing.T, n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "att%03d\n", i)
	}
	p := filepath.Join(t.TempDir(), "a.txt")
	os.WriteFile(p, []byte(b.String()), 0o600)
	return p
}

func TestScrollbarGeometry(t *testing.T) {
	for _, c := range []struct{ cap, total, top, pos, n int }{
		{10, 100, 0, 0, 1},    // top
		{10, 100, 90, 9, 1},   // bottom
		{10, 100000, 5, 0, 1}, // huge total: still one row
		{10, 10, 0, 0, 10},    // documented: total<=cap is all thumb
	} {
		if pos, n := thumb(c.cap, c.total, c.top); pos != c.pos || n != c.n {
			t.Errorf("thumb(%d, %d, %d) = %d, %d; want %d, %d", c.cap, c.total, c.top, pos, n, c.pos, c.n)
		}
	}
}

func resultView(t *testing.T, m model, n, w, h int) model {
	m = withResult(t, m, n)
	next, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	return next.(model)
}

func TestResultBoxFramed(t *testing.T) {
	_, m := setup(t)
	old := lipgloss.ColorProfile()
	t.Cleanup(func() { lipgloss.SetColorProfile(old) })
	lipgloss.SetColorProfile(termenv.TrueColor)
	m = resultView(t, m, 200, 160, 40)
	raw := m.View()
	v := ansi.Strip(raw)
	d := ansi.Strip(m.detail())
	if !strings.Contains(d, "╭") || !strings.Contains(d, "╰") {
		t.Fatal("detail has no box border")
	}
	for _, want := range []string{"╭", "╰", "lines 1–", "┃"} {
		if !strings.Contains(v, want) {
			t.Fatalf("view lacks %q", want)
		}
	}
	if !regexp.MustCompile(`lines 1–16 of \d+ ─`).MatchString(v) {
		t.Fatal("footer is not just the line range")
	}
	if !strings.Contains(raw, "48;2;") {
		t.Fatal("no background colour on the box")
	}
	if strings.Index(v, "╭") > strings.Index(v, "out001") || strings.Index(v, "out001") > strings.Index(v, "╰") {
		t.Fatal("output not between the borders")
	}
}

func TestShortResultNotFramed(t *testing.T) {
	_, m := setup(t)
	m = resultView(t, m, 3, 160, 40)
	v := ansi.Strip(m.detail()) // the panes draw their own rounded borders, so look at the detail alone
	if strings.Contains(v, "╭") || strings.Contains(v, "lines 1–") || !strings.Contains(v, "out003") {
		t.Fatal("short result must be plain")
	}
}

func TestScrollOutputHintOnStatusLine(t *testing.T) {
	_, m := setup(t)
	long := resultView(t, m, 200, 160, 40)
	long.focus = 2
	if !strings.Contains(ansi.Strip(long.View()), "[ ] scroll · { } next box") {
		t.Fatal("hint missing for a framed box at focus 2")
	}
	long.focus = 1
	if strings.Contains(ansi.Strip(long.View()), "[ ] scroll · { } next box") {
		t.Fatal("hint shown at focus 1")
	}
	short := resultView(t, m, 3, 160, 40)
	short.focus = 2
	if strings.Contains(ansi.Strip(short.View()), "[ ] scroll · { } next box") {
		t.Fatal("hint shown for a short result")
	}
}

func TestFramedBoxWrapsToInnerWidth(t *testing.T) {
	_, m := setup(t)
	m = resultView(t, m, 200, 160, 40)
	_, _, dw := columns(m.w)
	for _, l := range strings.Split(ansi.Strip(m.detail()), "\n") {
		if strings.ContainsAny(l[:min(3, len(l))], "│╭╰") && lipgloss.Width(l) != dw-2 {
			t.Fatalf("box line not %d wide: %q", dw-2, l)
		}
	}
	// the long line is out999 + 120 x's; scroll to it and check nothing is lost
	m.scrollBox(resultBox, 1<<20)
	var rows []string
	for _, l := range strings.Split(ansi.Strip(m.detail()), "\n") {
		if i := strings.Index(l, "out999"); i >= 0 || strings.Contains(l, "xxx") {
			rows = append(rows, strings.TrimRight(strings.Trim(l, "│┃ "), " "))
		}
	}
	if got := strings.Join(rows, ""); got != "out999"+strings.Repeat("x", 120) {
		t.Fatalf("wrapped text lost characters: %q", got)
	}
}

// stale makes the selected item "running" with no live run in this viewer (its viewer went away).
func stale(t *testing.T, q *queue.Queue, m model) (model, queue.Item) {
	t.Helper()
	it := *m.item()
	if err := q.Claim(it); err != nil {
		t.Fatal(err)
	}
	m.refresh()
	if m.item().ID != it.ID {
		t.Fatal("selection moved off the claimed item")
	}
	return m, it
}

func TestCCCancelsStaleRun(t *testing.T) {
	q, m := setup(t)
	m, it := stale(t, q, m)
	m.focus = 2
	if v := m.View(); !strings.Contains(v, "[ Cancel run ]") || !strings.Contains(v, "c c cancel run") {
		t.Fatal("a running item should offer Cancel run")
	}
	m, _ = press(m, "c")
	if !m.cancelArmed || q.State(it, time.Now()) != queue.Running {
		t.Fatal("one c must only arm")
	}
	if !strings.Contains(m.View(), "c again to cancel") {
		t.Fatal("armed cancel prompt not shown")
	}
	m, _ = press(m, "c")
	if s := q.State(it, time.Now()); s != queue.Canceled {
		t.Fatalf("state after c c = %s, want canceled", s)
	}
}

func TestOtherKeyDisarmsCancelAndIsNotActedOn(t *testing.T) {
	q, m := setup(t)
	m, it := stale(t, q, m)
	m, _ = press(m, "c")
	m, _ = press(m, "q")
	if m.cancelArmed || q.State(it, time.Now()) != queue.Running {
		t.Fatal("another key must disarm the cancel and leave the run alone")
	}
}

func TestCCCancelsLiveRun(t *testing.T) {
	_, m := setup(t)
	called := false
	m.running, m.runningID, m.cancelRun = true, m.item().ID, func() { called = true }
	m, _ = press(m, "c")
	m, _ = press(m, "c")
	if !called {
		t.Fatal("c c did not cancel the run in progress")
	}
	m.focus, m.ii = 1, 0
	m, _ = press(m, "j")
	if m.ii != 0 {
		t.Fatal("other keys are still dropped while running")
	}
}

func TestCancelButtonNeedsConfirm(t *testing.T) {
	q, m := setup(t)
	m, it := stale(t, q, m)
	dx := func() int { sw, iw, _ := columns(m.w); return sw + iw }()
	m, _ = click(m, dx+cancelRunX+1, buttonY)
	if !m.cancelConfirm || q.State(it, time.Now()) != queue.Running {
		t.Fatal("clicking Cancel run should only ask for confirmation")
	}
	if !strings.Contains(m.View(), "Yes, cancel") {
		t.Fatal("cancel confirmation not shown")
	}
	m, _ = click(m, dx+noX+1, buttonY)
	if m.cancelConfirm || q.State(it, time.Now()) != queue.Running {
		t.Fatal("No must keep the run")
	}
	m, _ = click(m, dx+cancelRunX+1, buttonY)
	m, _ = click(m, dx+yesCancelX+1, buttonY)
	if s := q.State(it, time.Now()); s != queue.Canceled {
		t.Fatalf("state after Yes, cancel = %s", s)
	}
}

func TestRevokedItemIsFinishedAndCannotRun(t *testing.T) {
	q, m := setup(t)
	it := *m.item()
	if err := q.Revoke(it, sid); err != nil {
		t.Fatal(err)
	}
	m.refresh()
	m.ii = indexOf(m, it.ID)
	m.open()
	m, _ = press(m, " ")
	m, cmd := press(m, " ")
	if cmd != nil || q.State(it, time.Now()) != queue.Revoked {
		t.Fatal("a revoked item must not run")
	}
	if !strings.Contains(m.View(), "REVOKED") {
		t.Fatal("detail should show the revoked state")
	}
}

func indexOf(m model, id string) int {
	for i, it := range m.session().items {
		if it.ID == id {
			return i
		}
	}
	return -1
}
