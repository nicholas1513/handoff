// Package runner executes a reviewed item as the human, with no shell.
package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
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
	return res, q.WriteResult(r.Item, res)
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
