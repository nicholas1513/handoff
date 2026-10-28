// Package runner executes a reviewed item as the human, with no shell.
package runner

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nicholas1513/handoff/internal/queue"
)

var fileRef = regexp.MustCompile(`\{file:([^}]*)\}`)

// Expand replaces each {file:name} in argv with the path of the reviewed snapshot copy.
func Expand(r queue.Reviewed) ([]string, error) {
	argv := make([]string, len(r.Item.Argv))
	var bad error
	for i, a := range r.Item.Argv {
		argv[i] = fileRef.ReplaceAllStringFunc(a, func(m string) string {
			name := fileRef.FindStringSubmatch(m)[1]
			if !slices.Contains(r.Item.Files, name) {
				bad = fmt.Errorf("argv refers to unknown attachment %q", name)
			}
			return filepath.Join(r.SnapDir, name)
		})
	}
	return argv, bad
}

// Run executes r once and records the result. It refuses expired, already-run and modified items.
func Run(ctx context.Context, q *queue.Queue, r queue.Reviewed, now time.Time) (queue.Result, error) {
	if s := q.State(r.Item, now); s != queue.Pending {
		return queue.Result{}, fmt.Errorf("item is %s", s)
	}
	argv, err := Expand(r)
	if err != nil {
		return queue.Result{}, err
	}
	if _, _, ok := queue.Am(r.Item); ok && r.AmBase == "" { // before the claim: the item stays pending, to reject
		return queue.Result{}, queue.ErrNoCommit
	}
	if err := q.Claim(r.Item); err != nil {
		return queue.Result{}, fmt.Errorf("claim: %w", err)
	}
	if err := q.Verify(r); err != nil {
		res := queue.Result{State: queue.Modified, ExitCode: -1, Stderr: err.Error(), RanAt: now, SHA256Reviewed: r.Hash}
		return res, errors.Join(err, q.WriteResult(r.Item, res))
	}

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = r.Item.Cwd
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	repo, _, am := queue.Am(r.Item)
	if am { // as the review copy runs it: with no replace refs (it reads the objects the review copy read), and with no
		// hooks, fsmonitor, signing program or lazy fetch (a promisor remote's upload-pack) the repository names
		cmd.Env = append(os.Environ(), "GIT_NO_REPLACE_OBJECTS=1", "GIT_NO_LAZY_FETCH=1", "GIT_CONFIG_COUNT=3",
			"GIT_CONFIG_KEY_0=core.hooksPath", "GIT_CONFIG_VALUE_0=/dev/null", "GIT_CONFIG_KEY_1=core.fsmonitor",
			"GIT_CONFIG_VALUE_1=false", "GIT_CONFIG_KEY_2=commit.gpgSign", "GIT_CONFIG_VALUE_2=false")
	}
	// Own process group, so canceling also stops what the command started (e.g. ssh under a script).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = 3 * time.Second // then the leader is killed and pipes held by stragglers are closed
	if err = cmd.Start(); err == nil {
		q.RecordPID(r.Item, cmd.Process.Pid) // best effort: lets a later viewer stop this run if this one goes away
		err = cmd.Wait()
	}

	// shortcut: output is kept whole in memory and in result.json; cap it if huge outputs show up.
	res := queue.Result{State: queue.Succeeded, Stdout: stdout.String(), Stderr: stderr.String(), RanAt: time.Now(), SHA256Reviewed: r.Hash}
	var exitErr *exec.ExitError
	switch {
	case ctx.Err() != nil && err != nil:
		res.State, res.ExitCode = queue.Canceled, -1
		res.Stderr += "\ncanceled by the reviewer"
	case errors.As(err, &exitErr):
		res.State, res.ExitCode = queue.Failed, exitErr.ExitCode()
	case err != nil: // didn't start: not found, bad cwd
		res.State, res.ExitCode = queue.Failed, -1
		res.Stderr += err.Error()
	}
	// Checked even when git am failed or stopped in the review copy, which then drew its error and the raw patch. A run
	// that stops may make only the first of the reviewed commits; any other commit was never drawn.
	if am && r.AmChecked {
		got, _ := gitObjects(repo, "", AmLog(r.AmBase, "HEAD")...)
		differ := got != r.AmCommits && (res.State == queue.Succeeded || !strings.HasPrefix(r.AmCommits, got))
		if differ || !amIntact(repo, r.AmBase) {
			if res.State == queue.Succeeded {
				res.State, res.ExitCode = queue.Failed, -1
			}
			res.Stderr += "\nhandoff: these commits differ from the review copy's (an object or setting in the repository" +
				" changed since review): check them before you use them"
		}
	}
	return res, q.WriteResult(r.Item, res)
}

// AmLog is git log's arguments for the commits after base up to head, as the run compares the repository's with the
// review copy's: each one's tree, author, date and message, in formats no setting changes, so a message or author a
// setting changed since review shows. A file rewritten in .git/objects is read without a hash check, so only the trees
// git am made show it.
func AmLog(base, head string) []string {
	return []string{"log", "--no-show-signature", "--reverse", "--format=%T %aI %an <%ae>%x00%B%x00", base + ".." + head}
}

// amIntact reports whether every object that repo's commits after base added holds what its id names. git am doesn't
// write an object the repository already has, so one planted after review at the id of a blob or tree the patch makes
// would be committed as it is, with the trees the review copy made.
func amIntact(repo, base string) bool {
	ids, err := gitObjects(repo, "", "rev-list", "--objects", "--no-object-names", base+"..HEAD")
	if err != nil {
		return false
	}
	out, err := gitObjects(repo, ids, "cat-file", "--batch") // "<id> <type> <size>\n<content>\n" each
	for err == nil && out != "" {
		hdr, rest, _ := strings.Cut(out, "\n")
		f := strings.Fields(hdr)
		n := -1
		if len(f) == 3 {
			n, _ = strconv.Atoi(f[2])
		}
		if n < 0 || len(rest) <= n {
			return false // "<id> missing", or cut short
		}
		h := sha1.New()
		if len(f[0]) == 2*sha256.Size {
			h = sha256.New()
		}
		fmt.Fprintf(h, "%s %d\x00%s", f[1], n, rest[:n])
		if hex.EncodeToString(h.Sum(nil)) != f[0] {
			return false
		}
		out = rest[n+1:]
	}
	return err == nil
}

// gitObjects runs git in repo with stdin on its input, reading objects as the review copy does: with no replace refs,
// and with no lazy fetch, which would run a promisor remote's upload-pack.
func gitObjects(repo, stdin string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"--no-lazy-fetch", "--no-replace-objects", "-C", repo}, args...)...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.Output()
	return string(out), err
}

// CancelStale ends an item left "running" by a viewer that went away (quit or crashed mid-run). It
// stops the recorded process group only if that pid still runs the item's program, so a pid the OS
// has since reused for something else is never signalled.
func CancelStale(q *queue.Queue, it queue.Item, now time.Time) error {
	if s := q.State(it, now); s != queue.Running {
		return fmt.Errorf("item is %s", s)
	}
	note := "canceled by the reviewer; no process was recorded for this run, so none was stopped"
	if pid, ok := q.ClaimPID(it); ok {
		note = fmt.Sprintf("canceled by the reviewer; process %d is no longer running %s, so nothing was stopped", pid, it.Argv[0])
		if runsProgram(pid, it.Argv[0]) {
			syscall.Kill(-pid, syscall.SIGTERM)
			note = fmt.Sprintf("canceled by the reviewer; stopped process group %d", pid)
		}
	}
	return q.WriteResult(it, queue.Result{State: queue.Canceled, ExitCode: -1, Stderr: note, RanAt: now})
}

// runsProgram reports whether pid is running argv0. Kernels truncate process names (15 or 16 bytes),
// so a name at that length only has to be a prefix of argv0's base name.
func runsProgram(pid int, argv0 string) bool {
	name, want := procName(pid), filepath.Base(argv0)
	return name != "" && (name == want || (len(name) >= 15 && strings.HasPrefix(want, name)))
}

// Programs that run other code from their arguments, so argv alone doesn't show what will happen.
var interpreters = []string{
	"sh", "bash", "zsh", "fish", "dash", "ksh", "csh", "tcsh", "pwsh", "powershell",
	"python", "python2", "python3", "node", "deno", "bun", "ruby", "perl", "php", "lua", "osascript",
	"env", "xargs", "sudo", "nohup", "exec", "eval", "nice", "time", "timeout", "find", "awk", "make",
}

var pythonVersion = regexp.MustCompile(`^python\d+(\.\d+)*$`)

func RunsInterpreter(argv []string) bool {
	if len(argv) == 0 {
		return false
	}
	base := filepath.Base(argv[0])
	return slices.Contains(interpreters, base) || pythonVersion.MatchString(strings.ToLower(base))
}
