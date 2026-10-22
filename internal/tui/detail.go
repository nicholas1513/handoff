package tui

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/nicholas1513/handoff/internal/queue"
	"github.com/nicholas1513/handoff/internal/rules"
	"github.com/nicholas1513/handoff/internal/runner"
)

var (
	cmdProg = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	cmdFlag = lipgloss.NewStyle().Foreground(lipgloss.Color("5"))
	cmdAtt  = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	attRe   = regexp.MustCompile(`\{file:([^}]*)\}`)
)

// flatten strips styling and rejoins wrapped lines (newline plus following indent).
func flatten(s string) string {
	s = ansi.Strip(s)
	for {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			return s
		}
		j := i + 1
		for j < len(s) && (s[j] == ' ' || s[j] == '\t') {
			j++
		}
		s = s[:i] + s[j:]
	}
}

func isURL(w string) bool { return strings.HasPrefix(w, "http://") || strings.HasPrefix(w, "https://") }

// shown sanitizes one argv word and Go-quotes it when empty or holding whitespace or a quote,
// so a newline can't draw a fake argument line and one word can't pass for two.
func shown(w string) string {
	w = sanitize(w)
	if w == "" || strings.ContainsFunc(w, func(r rune) bool { return unicode.IsSpace(r) || r == '"' }) {
		return strconv.Quote(w)
	}
	return w
}

// word renders one argv word; URLs break at "/" boundaries, continuation lines indented by indent.
func word(w string, indent int, sizes map[string]int) string {
	q := shown(w)
	if m := attRe.FindStringSubmatch(w); m != nil {
		note := " (attached, see below)"
		if n, ok := sizes[m[1]]; ok {
			note = fmt.Sprintf(" (attached, %.1f KB, see below)", float64(n)/1000)
		}
		return cmdAtt.Render("📎 ") + q + cmdAtt.Render(note)
	}
	if q != w || !isURL(w) {
		return q
	}
	rest := w[strings.Index(w, "://")+3:]
	h := strings.IndexByte(rest, '/')
	if h < 0 {
		return cmdProg.Render(w)
	}
	cut := len(w) - len(rest) + h
	parts := []string{cmdProg.Render(w[:cut])}
	segs := strings.Split(w[cut:], "/")[1:] // each segment keeps its leading "/"
	for i, s := range segs {
		s = "/" + s
		if i == len(segs)-1 {
			parts = append(parts, cmdProg.Render(s))
		} else {
			parts = append(parts, dim.Render(s))
		}
	}
	return strings.Join(parts, "\n"+strings.Repeat(" ", indent))
}

// commandLines lays argv out in argv order: program line, then one line per flag (with its value) or positional.
func commandLines(argv []string, sizes map[string]int) []string {
	i, head := 0, []string{}
	for i < len(argv) && !strings.HasPrefix(argv[i], "-") {
		head = append(head, cmdProg.Render(shown(argv[i])))
		i++
	}
	type arg struct {
		flag, val string // flag "" marks a positional
		has       bool
	}
	var args []arg
	width := 0
	for i < len(argv) {
		a := argv[i]
		i++
		if !strings.HasPrefix(a, "-") || a == "-" {
			args = append(args, arg{val: a})
			continue
		}
		f := arg{flag: a}
		if i < len(argv) && !strings.HasPrefix(argv[i], "--") {
			f.val, f.has, i = argv[i], true, i+1
		}
		args = append(args, f)
		width = max(width, len(shown(a)))
	}
	var lines []string
	if len(head) > 0 {
		lines = append(lines, strings.Join(head, " "))
	}
	for _, a := range args {
		if a.flag == "" {
			lines = append(lines, "  "+word(a.val, 4, sizes))
			continue
		}
		s := shown(a.flag)
		l := "  " + cmdFlag.Render(s)
		if a.has {
			l += strings.Repeat(" ", width-len(s)+1) + word(a.val, 2+width+1, sizes)
		}
		lines = append(lines, l)
	}
	return lines
}

// methodOf returns the upper-cased HTTP method from the last --method/-X/--request, or "" (CLIs honour the last).
func methodOf(argv []string) string {
	mt := ""
	for i, a := range argv {
		switch {
		case a == "--method" || a == "-X" || a == "--request":
			if i+1 < len(argv) {
				mt = strings.ToUpper(argv[i+1])
			}
		case strings.HasPrefix(a, "--method="):
			mt = strings.ToUpper(strings.TrimPrefix(a, "--method="))
		case strings.HasPrefix(a, "--request="):
			mt = strings.ToUpper(strings.TrimPrefix(a, "--request="))
		}
	}
	return mt
}

func heading(title string) string {
	return dim.Render("── ") + lipgloss.NewStyle().Foreground(lipgloss.Color("4")).Render(title) + dim.Render(" ──")
}

func chip(text, color string) string {
	return lipgloss.NewStyle().Foreground(lipgloss.Color(color)).Bold(true).Render(text)
}

func (m model) detail() string {
	s := m.session()
	if s != nil && s.invalid != nil && m.ii < len(s.invalid) {
		_, err := m.q.Load(s.invalid[m.ii])
		return warn.Render("This item failed to load and can't be run.") + "\n\n" + sanitize(fmt.Sprint(err))
	}
	if m.openErr != nil {
		return warn.Render("Could not open item: ") + sanitize(m.openErr.Error())
	}
	if m.reviewed == nil {
		return dim.Render("No items.")
	}
	it := m.reviewed.Item
	var b strings.Builder
	p := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }

	state := m.q.State(it, time.Now())
	chips := []string{chip(strings.ToUpper(state), resColor(state))}
	if mt := methodOf(it.Argv); mt != "" {
		c := ""
		switch mt {
		case "GET":
			c = "2"
		case "PUT", "POST", "PATCH", "DELETE":
			c = "1"
		}
		chips = append(chips, chip(sanitize(mt), c))
	}
	verified := s != nil && s.sess != nil
	if verified {
		chips = append(chips, ok.Render("✓ verified"))
	} else {
		chips = append(chips, warn.Render("⚠ unverified session"))
	}
	if state == queue.Pending {
		chips = append(chips, dim.Render("expires in "+strings.TrimSuffix(time.Until(it.Expires).Round(time.Minute).String(), "0s")))
	}
	p("%s\n", strings.Join(chips, "  "))
	p("%s\n", bold.Render(sanitize(it.Description)))

	project := it.Cwd
	if verified {
		project = s.sess.Cwd
	}
	settings := rules.Load(rules.DefaultFiles(m.home, project))
	if m.changed {
		p("%s", warn.Render("Changed on disk after you opened it. Press o to review it again."))
	}
	if !verified {
		p("%s", warn.Render("⚠ unverified session "+sanitize(shortID(it.SessionID))+": no hook record, provenance unknown"))
	}
	if runner.RunsInterpreter(it.Argv) {
		p("%s", warn.Render(fmt.Sprintf("⚠ runs an interpreter (%s): its arguments are code", sanitize(it.Argv[0]))))
	}
	for _, pr := range settings.Problems {
		p("%s", warn.Render("settings problem: "+sanitize(pr)))
	}

	sizes := map[string]int{}
	for _, name := range it.Files {
		if fi, err := os.Stat(filepath.Join(m.reviewed.SnapDir, name)); err == nil {
			sizes[name] = int(fi.Size())
		}
	}
	p("\n%s", heading("Command"))
	for _, l := range commandLines(it.Argv, sizes) {
		p("%s", l)
	}

	p("\n%s", heading("Why"))
	if verified {
		if pr, err := m.q.PromptBefore(it.SessionID, it.Created); err == nil {
			p("%s you said: %s", dim.Render(pr.TS.Format("15:04")), sanitize(pr.Prompt))
		} else {
			p("%s", warn.Render("no prompt recorded"))
		}
		p("%s %s", ok.Render("✓ verified"), sanitize(it.SessionID))
		p("%s", dim.Render("session cwd ")+sanitize(s.sess.Cwd))
	}
	why := []string{}
	if it.Reason != "" {
		why = append(why, sanitize(it.Reason))
	}
	cwd := sanitize(it.Cwd)
	if m.home != "" && (cwd == m.home || strings.HasPrefix(cwd, m.home+"/")) {
		cwd = "~" + strings.TrimPrefix(cwd, m.home)
	}
	why = append(why, settings.Check(it.Argv).String(m.home), cwd)
	p("%s", dim.Render(strings.Join(why, " · ")))

	for _, name := range it.Files {
		data, err := os.ReadFile(filepath.Join(m.reviewed.SnapDir, name))
		if err != nil {
			data = []byte(err.Error())
		}
		var pretty bytes.Buffer
		if json.Indent(&pretty, data, "", "  ") == nil {
			data = pretty.Bytes()
		}
		p("\n%s", heading("📎 "+sanitize(name)))
		p("%s", sanitize(string(data)))
	}

	p("\n%s", heading("Integrity"))
	p("%s", dim.Render("SHA-256 (combined: item.json + attachments)"))
	p("%s", dim.Render(m.reviewed.Hash))
	for _, f := range m.reviewed.FileHashes {
		name, path := sanitize(f.Name), sanitize(f.Path)
		if f.Name != "item.json" {
			name = "📎 " + name
		}
		if m.home != "" && strings.HasPrefix(path, m.home+"/") {
			path = "~" + strings.TrimPrefix(path, m.home)
		}
		p("%s\n%s", name, dim.Render(f.SHA256+"  "+path))
	}

	if res, err := m.q.ReadResult(it); err == nil {
		p("\n%s", heading("Result"))
		p("%s exit %d at %s", chip(strings.ToUpper(res.State), resColor(res.State)), res.ExitCode, res.RanAt.Format(time.Kitchen))
		if res.Stdout != "" {
			p("%s\n%s", bold.Render("stdout"), sanitize(res.Stdout))
		}
		if res.Stderr != "" {
			p("%s\n%s", bold.Render("stderr"), sanitize(res.Stderr))
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		p("\n%s", heading("Result"))
		p("%s", warn.Render(sanitize(err.Error())))
	}
	return b.String()
}

// resColor: pending yellow, succeeded green, anything else (failed, rejected) red.
func resColor(state string) string {
	switch state {
	case queue.Pending:
		return "3"
	case queue.Succeeded:
		return "2"
	}
	return "1"
}

func shortID(id string) string { return id[:min(8, len(id))] }
