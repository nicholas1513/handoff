package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/nicholas1513/handoff/internal/queue"
)

// An am item (git [-C <repo>] am {file:<name>}, see queue.Am) hands over commits as a patch. Rather than parse the
// mail, which git am decodes in ways a parser lags behind, the viewer lets git apply the reviewed snapshot in a review
// copy of the repository and draws git's own log of what it made.

// reviewKey names an am item's review copy: the item and the snapshot it applies.
type reviewKey struct{ id, hash string }

// amView is an am item's review copy as drawn: git's log of the commits git am made, or why it failed. Never
// modified once built.
type amView struct {
	key        reviewKey
	repo, base string  // the repository's top level and the commit git am applied to
	sum        string  // "N commits · F files changed, +A −B"
	bin        int     // binary files, whose contents the log doesn't show
	rows       []dline // the log, coloured
	err        string  // why the build failed: git's own error, which the run would hit too
}

// reviews serializes builds, which remove and rebuild an item's directory, and keeps each item's last good build.
// shortcut: entries stay until the viewer quits; drop finished items' if long sessions show up.
var reviews = struct {
	sync.Mutex
	built map[string]*amView
}{built: map[string]*amView{}}

// reviewDir is an item's review copy, beside queue/ so the sandbox can't write it.
func reviewDir(root, id string) string { return filepath.Join(root, "review", id) }

// amKey is the open item's review copy key, if it is an am item.
func (m model) amKey() (reviewKey, bool) {
	if r := m.reviewed; r != nil {
		if _, _, ok := queue.Am(r.Item); ok {
			return reviewKey{r.Item.ID, r.Hash}, true
		}
	}
	return reviewKey{}, false
}

// shownAm is the open am item's review copy, once built. Layout lookups ask on every event, so it compares keys only:
// a review copy is built for an am item, and its key holds the hash of the item's argv.
func (m model) shownAm() *amView {
	if r := m.reviewed; m.am != nil && r != nil && m.am.key == (reviewKey{r.Item.ID, r.Hash}) {
		return m.am
	}
	return nil
}

// notReady is the status while a run waits for the review copy.
const notReady = "review copy not ready yet"

// amReady is false, and says why in the status line, while the open am item's review copy is not drawn: nothing of its
// patch is on screen yet, so nothing may run it.
func (m *model) amReady() bool {
	if _, ok := m.amKey(); ok && m.shownAm() == nil {
		m.status = notReady
		return false
	}
	return true
}

// flight is a review copy being built, and how to stop it.
type flight struct {
	key  reviewKey
	stop context.CancelFunc
}

// reviewCmd starts building the open am item's review copy off the event loop while it is pending and neither built
// nor building, and stops a build the selection has left, whose result then never arrives.
func (m *model) reviewCmd() tea.Cmd {
	k, ok := m.amKey()
	ok = ok && m.drawnState() == queue.Pending && m.shownAm() == nil
	if f := m.amFlight; f != nil && (!ok || f.key != k) {
		f.stop()
		m.amFlight = nil
	}
	if !ok || m.amFlight != nil {
		return nil
	}
	ctx, stop := context.WithTimeout(context.Background(), time.Minute)
	m.amFlight = &flight{k, stop}
	return m.build(ctx)
}

// build is the command that builds the open item's review copy. Its result arrives as an *amView.
func (m model) build(ctx context.Context) tea.Cmd {
	root, r := m.q.Root, *m.reviewed
	return func() tea.Msg {
		v := buildReview(ctx, root, r)
		if errors.Is(ctx.Err(), context.Canceled) {
			return nil
		}
		return v
	}
}

// buildReview applies the snapshot of r, an am item, with git am in a fresh clone of its repository at HEAD and reads
// back what git made of it. A fresh clone has no hooks and none of the repository's config but the settings it copies
// because they change what git am does (am, mailinfo, i18n, apply and core.whitespace), so nothing an agent planted runs.
func buildReview(ctx context.Context, root string, r queue.Reviewed) *amView {
	repo, file, _ := queue.Am(r.Item)
	v := &amView{key: reviewKey{r.Item.ID, r.Hash}}
	reviews.Lock()
	defer reviews.Unlock()
	out, err := git(ctx, repo, "rev-parse", "--show-toplevel", "--verify", "HEAD^{commit}")
	if err == nil {
		v.repo, v.base, _ = strings.Cut(strings.TrimSpace(out), "\n")
		if old := reviews.built[r.Item.ID]; old != nil && old.key == v.key && old.base == v.base {
			return old
		}
		delete(reviews.built, r.Item.ID) // its directory is about to go
		var patch string
		if patch, err = filepath.Abs(filepath.Join(r.SnapDir, file)); err == nil {
			err = v.build(ctx, reviewDir(root, r.Item.ID), patch)
		}
	}
	if err != nil {
		v.err = err.Error()
	} else if ctx.Err() == nil { // a stopped build may have read a snapshot that o was rewriting
		reviews.built[r.Item.ID] = v
	}
	return v
}

// build makes the review copy in dir, git am of patch on v.base, keeps its commits on the branch "reviewed", and
// reads them back.
func (v *amView) build(ctx context.Context, dir, patch string) error {
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	var err error
	run := func(dir string, args ...string) (out string) {
		if err == nil {
			out, err = git(ctx, dir, args...)
		}
		return out
	}
	cfg := run(v.repo, "config", "--list", "-z")
	run("", "clone", "--quiet", "--shared", "--no-checkout", "--", v.repo, dir)
	// Before any file is written: no filter (the user's own, such as git-lfs) runs on the agent's files, and the diff
	// reads files by their bytes alone, where an attribute such as -diff or binary would hide a text change.
	if err == nil {
		info := filepath.Join(dir, ".git", "info")
		if err = os.MkdirAll(info, 0o700); err == nil {
			err = os.WriteFile(filepath.Join(info, "attributes"), []byte("* -filter !diff\n"), 0o600)
		}
	}
	for _, kv := range strings.Split(cfg, "\x00") {
		k, val, found := strings.Cut(kv, "\n")
		if !found {
			val = "true" // a key with no value
		}
		if s, _, _ := strings.Cut(k, "."); s == "am" || s == "mailinfo" || s == "i18n" || s == "apply" ||
			k == "core.whitespace" {
			run(dir, "config", "--add", k, val)
		}
	}
	run(dir, "checkout", "--quiet", "--detach", v.base)
	if err != nil {
		return err
	}
	_, amErr := git(ctx, dir, "am", "--quiet", patch) // if it stops partway, the run makes the commits before too
	run(dir, "branch", "reviewed", "HEAD")
	// a submodule's ignore setting or the user's diff.ignoreSubmodules would hide a gitlink change, and their
	// diff.submodule would draw it as a log instead of a diff
	num := run(dir, "diff", "--numstat", "--no-ext-diff", "--no-textconv", "--ignore-submodules=none", v.base, "reviewed")
	log := run(dir, "log", "--reverse", "-p", "--no-color", "--no-ext-diff", "--no-textconv", "--ignore-submodules=none",
		"--submodule=short", "--format=commit %H%nAuthor: %an <%ae>%nDate:   %ad%n%n%w(0,4,4)%B", v.base+"..reviewed")
	if err != nil {
		return err
	}
	commits, files, add, del := 0, 0, 0, 0
	v.rows, commits = amRows(log)
	for _, l := range splitLines(num) {
		a, rest, _ := strings.Cut(l, "\t")
		d, _, _ := strings.Cut(rest, "\t")
		x, _ := strconv.Atoi(a)
		y, _ := strconv.Atoi(d)
		if a == "-" {
			v.bin++
		}
		files, add, del = files+1, add+x, del+y
	}
	if amErr != nil && commits == 0 {
		return amErr
	}
	what := count(commits, "commit")
	if amErr != nil {
		what += " applied before git am stopped"
	}
	v.sum = fmt.Sprintf("%s · %s changed, +%d −%d", what, count(files, "file"), add, del)
	if v.bin > 0 {
		v.sum += " · " + count(v.bin, "binary file")
	}
	if v.bin > 0 || amErr != nil {
		v.sum = "⚠ " + v.sum
	}
	return amErr
}

// git runs git in dir ("" for the viewer's own) with hooks, the file-system monitor, commit signing and lazy fetches
// (which run a promisor remote's upload-pack) off, and returns its output or an error holding git's message. The
// committer is fixed: it is never drawn, and the user's may come from an includeIf or repository config that doesn't
// reach the review copy.
func git(ctx context.Context, dir string, args ...string) (string, error) {
	a := []string{"--no-lazy-fetch", "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false",
		"-c", "commit.gpgSign=false", "-c", "user.name=handoff", "-c", "user.email=handoff@review"}
	if dir != "" {
		a = append(a, "-C", dir)
	}
	cmd := exec.CommandContext(ctx, "git", append(a, args...)...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err == nil {
		return string(out), nil
	}
	var msg []string
	for _, l := range strings.Split(stderr.String(), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "hint:") {
			msg = append(msg, l)
		}
	}
	if ctx.Err() != nil || len(msg) == 0 {
		msg = []string{fmt.Sprint(err)}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			msg = []string{"timed out"}
		}
	}
	return "", fmt.Errorf("git %s: %s", args[0], strings.Join(msg, "\n"))
}

var amHead = lipgloss.Color("3") // a commit's header lines

// amRows colours git log -p output and counts its commits. Each commit's header lines are highlighted and its message
// is plain: git indents it, so no message line can pass for a diff line. A file's header lines are dim, and its hunk
// lines are green, red or cyan until the hunk header's counts are in.
func amRows(log string) (rows []dline, commits int) {
	const (
		inMsg = iota
		inHead
		inFile
		inHunk
	)
	st, old, neu := inMsg, 0, 0
	for _, l := range splitLines(log) {
		if st == inHunk && old <= 0 && neu <= 0 {
			st = inFile
		}
		var c lipgloss.TerminalColor = lipgloss.NoColor{}
		switch o, n, hunk := hunkLen(l); {
		case st == inHunk:
			c = diffColor(l)
			switch {
			case strings.HasPrefix(l, "+"):
				neu--
			case strings.HasPrefix(l, "-"):
				old--
			case !strings.HasPrefix(l, `\`): // context, "" too under diff.suppressBlankEmpty
				old, neu = old-1, neu-1
			}
		case strings.HasPrefix(l, "commit "):
			st, c, commits = inHead, amHead, commits+1
		case st == inHead && l == "":
			st = inMsg
		case st == inHead:
			c = amHead
		case strings.HasPrefix(l, "diff --git "):
			st = inFile
		case st == inFile && hunk:
			st, old, neu, c = inHunk, o, n, diffColor(l)
		case st == inFile && strings.HasPrefix(l, "Binary files "):
			c = warn.GetForeground()
		case st == inFile:
			c = lipgloss.Color("8")
		}
		rows = append(rows, dline{text: l, color: c})
	}
	return rows, commits
}

// sweep removes the review copies of items no longer pending, best effort.
func sweep(q *queue.Queue) tea.Cmd {
	return func() tea.Msg {
		reviews.Lock()
		defer reviews.Unlock()
		ents, _ := os.ReadDir(filepath.Join(q.Root, "review"))
		for _, e := range ents {
			if it, err := q.Load(e.Name()); err != nil || q.State(it, time.Now()) != queue.Pending {
				os.RemoveAll(reviewDir(q.Root, e.Name()))
			}
		}
		return nil
	}
}
