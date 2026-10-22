package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

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

	m, cmd = click(m, dx+yesX+1, buttonY)
	if cmd == nil {
		t.Fatal("confirm click did not start the run")
	}
	next, _ := m.Update(cmd())
	m = next.(model)
	if s := q.State(*m.item(), time.Now()); s != queue.Succeeded {
		t.Fatalf("state after confirmed run = %s", s)
	}
}

func TestRejectAndInvalid(t *testing.T) {
	q, m := setup(t)
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	m = next.(model)
	if s := q.State(*m.item(), time.Now()); s != queue.Rejected {
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
	m.running = true
	m, _ = press(m, " ")
	m, cmd := press(m, " ")
	if m.armed || cmd != nil {
		t.Fatal("keys must be dropped while running")
	}
}
