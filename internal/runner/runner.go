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
	err = cmd.Run()

	// shortcut: output is kept whole in memory and in result.json; cap it if huge outputs show up.
	res := queue.Result{State: queue.Succeeded, Stdout: stdout.String(), Stderr: stderr.String(), RanAt: time.Now(), SHA256Reviewed: r.Hash}
	var exitErr *exec.ExitError
	switch {
	case errors.As(err, &exitErr):
		res.State, res.ExitCode = queue.Failed, exitErr.ExitCode()
	case err != nil: // didn't start: not found, bad cwd
		res.State, res.ExitCode = queue.Failed, -1
		res.Stderr += err.Error()
	}
	return res, q.WriteResult(r.Item, res)
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
