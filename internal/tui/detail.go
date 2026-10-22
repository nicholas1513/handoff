package tui

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
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

// word renders one sanitized argv word; URLs break at "/" boundaries, continuation lines indented by indent.
func word(w string, indent int, sizes map[string]int) string {
	w = sanitize(w)
	if m := attRe.FindStringSubmatch(w); m != nil {
		note := " (attached, see below)"
		if n, ok := sizes[m[1]]; ok {
			note = fmt.Sprintf(" (attached, %.1f KB, see below)", float64(n)/1000)
		}
		return cmdAtt.Render("📎 ") + w + cmdAtt.Render(note)
	}
	if !isURL(w) {
		return w
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

// commandLines lays argv out as: program line, one line per flag, one line per positional.
func commandLines(argv []string, sizes map[string]int) []string {
	i, head := 0, []string{}
	for i < len(argv) && !strings.HasPrefix(argv[i], "-") {
		head = append(head, cmdProg.Render(sanitize(argv[i])))
		i++
	}
	if len(head) == 0 {
		return nil
	}
	type kv struct{ flag, val string }
	var flags []kv
	var pos []string
	width := 0
	for i < len(argv) {
		a := argv[i]
		i++
		if !strings.HasPrefix(a, "-") || a == "-" {
			pos = append(pos, a)
			continue
		}
		f := kv{flag: a}
		if i < len(argv) && !strings.HasPrefix(argv[i], "--") {
			f.val, i = argv[i], i+1
		}
		flags = append(flags, f)
		width = max(width, len(sanitize(a)))
	}
	lines := []string{strings.Join(head, " ")}
	for _, f := range flags {
		s := sanitize(f.flag)
		l := "  " + cmdFlag.Render(s)
		if f.val != "" {
			l += strings.Repeat(" ", width-len(s)+1) + word(f.val, 2+width+1, sizes)
		}
		lines = append(lines, l)
	}
	for _, p := range pos {
		lines = append(lines, "  "+word(p, 4, sizes))
	}
	return lines
}

// methodOf returns the upper-cased HTTP method from --method/-X/--request, or "".
func methodOf(argv []string) string {
	for i, a := range argv {
		switch {
		case a == "--method" || a == "-X" || a == "--request":
			if i+1 < len(argv) {
				return strings.ToUpper(argv[i+1])
			}
		case strings.HasPrefix(a, "--method="):
			return strings.ToUpper(strings.TrimPrefix(a, "--method="))
		case strings.HasPrefix(a, "--request="):
			return strings.ToUpper(strings.TrimPrefix(a, "--request="))
		}
	}
	return ""
}
