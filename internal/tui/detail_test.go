package tui

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/nicholas1513/handoff/internal/queue"
)

// withItem stages it in m's queue and returns a fresh model (newest item selected) at width w.
func withItem(t *testing.T, m model, it queue.Item, files map[string]string, w int) model {
	t.Helper()
	it.Cwd, it.SessionID = t.TempDir(), sid
	if _, err := m.q.Add(it, files); err != nil {
		t.Fatal(err)
	}
	next, _ := newModel(m.q, m.home).Update(tea.WindowSizeMsg{Width: w, Height: 30})
	return next.(model)
}

func TestCommandLinesShowEveryElementVerbatim(t *testing.T) {
	cases := [][]string{
		{"az", "rest", "--method", "put", "--resource", "499b84ac-1321-427f-aa17-267ca6975798",
			"--url", "https://dev.azure.com/Org/Example%20Proj/Example%20Proj%20Team/_apis/dashboard/dashboards/3c9a1f2e-7b4d-4e8a-9f61-2d5c8b0e7a43?api-version=7.1-preview.3",
			"--body", "@{file:body.json}"},
		{"/usr/bin/true"},
		{"curl", "--data", "-", "-X", "POST", "https://x.test/a"},
		{"az", "rest", "--method=get", "--url=https://x.test/b"},
		{"bash", "-c", "echo hi; rm -rf ~/x"},
		{"--foo", "bar"},
		{"-X", "POST", "https://x.test"},
		{"terraform", "apply", "-var", "x=1\n  -target=module.safe", "https://h/a/b\n  --dry-run"},
	}
	for _, argv := range cases {
		got := flatten(strings.Join(commandLines(argv, map[string]int{"body.json": 6100}), "\n"))
		for _, a := range argv {
			want := sanitize(a)
			if strings.ContainsFunc(want, unicode.IsSpace) { // shown Go-quoted so it reads as one word
				want = strconv.Quote(want)
			}
			if !strings.Contains(got, want) {
				t.Errorf("argv %q: element %q missing from %q", argv, want, got)
			}
		}
	}
}

func TestWhitespaceCannotForgeArgs(t *testing.T) {
	argv := []string{"terraform", "apply", "-var", "x=1\n  -target=module.safe", "-auto-approve", "https://h/a/b\n  --dry-run"}
	for _, l := range strings.Split(ansi.Strip(strings.Join(commandLines(argv, nil), "\n")), "\n") {
		if l = strings.TrimLeft(l, " "); strings.HasPrefix(l, "-target") || strings.HasPrefix(l, "--dry-run") {
			t.Errorf("forged argument line %q", l)
		}
	}
	for want, argv := range map[string][]string{
		`curl ""`:                 {"curl", "", "--x"},
		`git "push origin"`:       {"git", "push origin"},
		`git "\"push" "origin\""`: {"git", `"push`, `origin"`},
	} {
		if got := ansi.Strip(commandLines(argv, nil)[0]); got != want {
			t.Errorf("head of %q = %q want %q", argv, got, want)
		}
	}
}

func TestCommandLinesKeepArgvOrder(t *testing.T) {
	got := ansi.Strip(strings.Join(commandLines([]string{"cmd", "-a", "x", "pos1", "-b", "y"}, nil), "\n"))
	a, p, b := strings.Index(got, "-a"), strings.Index(got, "pos1"), strings.Index(got, "-b")
	if a < 0 || !(a < p && p < b) {
		t.Errorf("argv order lost: %q", got)
	}
}

func TestMethodOf(t *testing.T) {
	for want, argv := range map[string][]string{
		"PUT": {"az", "rest", "--method", "put"}, "GET": {"az", "rest", "--method=get"},
		"POST": {"curl", "-X", "POST"}, "": {"az", "account", "show"},
		"DELETE": {"az", "rest", "--method", "GET", "--url", "u", "--method", "delete"},
	} {
		if got := methodOf(argv); got != want {
			t.Errorf("methodOf(%q) = %q want %q", argv, got, want)
		}
	}
}

func TestAttachmentDecoration(t *testing.T) {
	got := flatten(strings.Join(commandLines([]string{"az", "rest", "--body", "@{file:body.json}"}, map[string]int{"body.json": 6100}), "\n"))
	for _, w := range []string{"@{file:body.json}", "📎", "6.1 KB"} {
		if !strings.Contains(got, w) {
			t.Errorf("%q missing from %q", w, got)
		}
	}
}

func TestEmptyArgIsVisible(t *testing.T) {
	got := flatten(strings.Join(commandLines([]string{"curl", "--data", "", "https://x.test"}, nil), "\n"))
	if !strings.Contains(got, `--data  ""`) && !strings.Contains(got, `--data ""`) {
		t.Errorf("empty value not shown as \"\": %q", got)
	}
}

func TestDetailOrderAndTrustedSession(t *testing.T) {
	_, m := setup(t)
	out := ansi.Strip(m.detail())
	order := []string{"PENDING", "second", "── Command", "/usr/bin/true", "── Why", "do the thing", sid, "── Integrity"}
	last := -1
	for _, s := range order {
		i := strings.Index(out, s)
		if i <= last {
			t.Fatalf("%q missing or out of order in:\n%s", s, out)
		}
		last = i
	}
	if !strings.Contains(out, "verified") {
		t.Fatal("verified marker missing")
	}
}

func TestDetailWarningsAboveCommand(t *testing.T) {
	_, m := setup(t)
	m = withItem(t, m, queue.Item{Description: "x", Argv: []string{"bash", "-c", "echo"}}, nil, 120)
	out := ansi.Strip(m.detail())
	i, j := strings.Index(out, "runs an interpreter"), strings.Index(out, "── Command")
	if i < 0 || j < 0 || i > j {
		t.Fatalf("interpreter warning should precede the Command section:\n%s", out)
	}
}

func TestDetailNarrowTerminal(t *testing.T) {
	_, m := setup(t)
	argv := []string{"az", "rest", "--method", "put", "--url", "https://dev.azure.com/Org/Example%20Proj/_apis/dashboard/dashboards/3c9a1f2e-7b4d-4e8a-9f61-2d5c8b0e7a43?api-version=7.1-preview.3"}
	m = withItem(t, m, queue.Item{Description: "x", Argv: argv}, nil, 60)
	_ = m.View()
	_, _, dw := columns(m.w)
	lines := strings.Split(lipgloss.NewStyle().Width(dw-2).Render(m.detail()), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " ") // drop lipgloss's right padding so wrapped words rejoin
	}
	got := flatten(strings.Join(lines, "\n"))
	for _, a := range argv {
		if !strings.Contains(got, a) {
			t.Errorf("argv element %q missing from %q", a, got)
		}
	}
}

func TestDetailPrettyPrintsJSONAttachment(t *testing.T) {
	_, m := setup(t)
	src := filepath.Join(t.TempDir(), "body.json")
	os.WriteFile(src, []byte(`{"a":1}`), 0o600)
	m = withItem(t, m, queue.Item{Description: "x", Argv: []string{"az", "rest", "--body", "@{file:body.json}"}}, map[string]string{"body.json": src}, 120)
	out := ansi.Strip(m.detail())
	if !strings.Contains(out, "── 📎 body.json") || !strings.Contains(out, `"a": 1`) {
		t.Fatalf("attachment not pretty-printed under its heading:\n%s", out)
	}
}

func TestCwdHomeShortening(t *testing.T) {
	_, m := setup(t)
	cwd := m.reviewed.Item.Cwd
	m.home = filepath.Dir(cwd)
	if out := ansi.Strip(m.detail()); !strings.Contains(out, "~/"+filepath.Base(cwd)) {
		t.Errorf("cwd under home not shortened:\n%s", out)
	}
	m.home = cwd[:len(cwd)-1] // a sibling: /x/00 vs /x/001
	if out := ansi.Strip(m.detail()); !strings.Contains(out, cwd) {
		t.Errorf("sibling of home was shortened:\n%s", out)
	}
}

func TestIntegrityShowsAlgorithmAndPerFileHashes(t *testing.T) {
	_, m := setup(t)
	src := filepath.Join(t.TempDir(), "body.json")
	os.WriteFile(src, []byte(`{"a":1}`), 0o600)
	m = withItem(t, m, queue.Item{Description: "x", Argv: []string{"az", "rest", "--body", "@{file:body.json}"}}, map[string]string{"body.json": src}, 400)
	out := ansi.Strip(m.detail())
	fh := m.reviewed.FileHashes
	if len(fh) != 2 {
		t.Fatalf("FileHashes = %+v", fh)
	}
	for _, w := range []string{"SHA-256 (combined", "item.json", fh[0].SHA256, "📎 body.json", fh[1].SHA256, "/body.json"} {
		if !strings.Contains(out, w) {
			t.Errorf("%q missing from:\n%s", w, out)
		}
	}
}
