package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// prompt is a small Yes or No question in an overlay. While open it takes every key and mouse event, so nothing
// reaches the item behind it. ← → pick, space or enter select, and either option takes a click. It doesn't print
// those keys: they're the obvious ones.
type prompt struct {
	rows  []string              // what's drawn above the options
	yes   bool                  // Yes is picked
	onYes func(m model) tea.Cmd // what Yes does, given the model as it is when answered
}

func (p prompt) open() bool { return p.onYes != nil }

// ask opens a prompt. Some open from a command that finishes whenever it does, so whatever was armed by then is
// dropped: its next key mustn't answer this prompt and then a run's.
func (m *model) ask(p prompt) {
	m.prompt = p
	m.armed, m.confirm, m.cancelArmed, m.cancelConfirm, m.quitArmed = false, false, false, false, false
}

// answer closes the prompt, doing what Yes does if that was the answer.
func (m model) answer(yes bool) (tea.Model, tea.Cmd) {
	p := m.prompt
	m.prompt = prompt{}
	if yes {
		return m, p.onYes(m)
	}
	return m, nil
}

func (m model) promptKey(k string) (tea.Model, tea.Cmd) {
	switch k {
	case "left":
		m.prompt.yes = false
	case "right":
		m.prompt.yes = true
	case " ", "enter":
		return m.answer(m.prompt.yes)
	}
	return m, nil
}

const (
	promptNoBtn  = "[ No ]"
	promptYesBtn = "[ Yes ]"
	promptGap    = 2
)

// promptWidth is the width inside the frame.
func (m model) promptWidth() int {
	return max(lipgloss.Width(strings.Join(m.prompt.rows, "\n")), len(promptNoBtn)+promptGap+len(promptYesBtn))
}

// promptOverlay is the prompt as drawn, and where it sits (top-left cell) when centred in the window.
func (m model) promptOverlay() (lines []string, x, y int) {
	no, yes := promptNoBtn, promptYesBtn
	if m.prompt.yes {
		yes = sel.Render(yes)
	} else {
		no = sel.Render(no)
	}
	options := lipgloss.PlaceHorizontal(m.promptWidth(), lipgloss.Center, no+strings.Repeat(" ", promptGap)+yes)
	rows := append(append([]string{}, m.prompt.rows...), "", options)
	lines = strings.Split(focused.Padding(0, 1).Render(strings.Join(rows, "\n")), "\n")
	return lines, max(0, (m.w-lipgloss.Width(lines[0]))/2), max(0, (m.h-len(lines))/2)
}

// promptClick acts on the two options. Any other click is dropped: it must not land on what's behind.
func (m model) promptClick(x, y int) (tea.Model, tea.Cmd) {
	_, ox, oy := m.promptOverlay()
	if y != oy+1+len(m.prompt.rows)+1 { // border, rows, the blank row
		return m, nil
	}
	noX := ox + 2 + (m.promptWidth()-len(promptNoBtn)-promptGap-len(promptYesBtn))/2 // border and padding, then centred
	yesX := noX + len(promptNoBtn) + promptGap
	switch {
	case x >= noX && x < noX+len(promptNoBtn):
		return m.answer(false)
	case x >= yesX && x < yesX+len(promptYesBtn):
		return m.answer(true)
	}
	return m, nil
}
