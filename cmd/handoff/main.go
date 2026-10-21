package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nicholas1513/handoff/internal/hook"
	"github.com/nicholas1513/handoff/internal/queue"
	"github.com/nicholas1513/handoff/internal/tui"
)

const usage = `usage:
  handoff                                   open the viewer
  handoff add --desc "…" [--reason r] [--file name=path …] [--wait] -- <argv…>
  handoff hook session-start|prompt         (called by the Claude Code plugin)
`

func main() {
	home, err := os.UserHomeDir()
	if err != nil {
		fail(err)
	}
	root := os.Getenv("HANDOFF_ROOT")
	if root == "" {
		root = filepath.Join(home, ".handoff")
	}
	q := &queue.Queue{Root: root}

	args := os.Args[1:]
	switch {
	case len(args) == 0:
		err = tui.Run(q, home)
	case args[0] == "add":
		os.Exit(add(q, args[1:]))
	case args[0] == "hook" && len(args) == 2 && args[1] == "session-start":
		err = hook.SessionStart(q, os.Stdin, os.Getenv("CLAUDE_ENV_FILE"))
	case args[0] == "hook" && len(args) == 2 && args[1] == "prompt":
		err = hook.Prompt(q, os.Stdin)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fail(err)
	}
}

// fail exits 1, never 2: exit 2 from a UserPromptSubmit hook would block the user's prompt.
func fail(err error) {
	fmt.Fprintln(os.Stderr, "handoff:", err)
	os.Exit(1)
}

func add(q *queue.Queue, args []string) int {
	fs := flag.NewFlagSet("add", flag.ContinueOnError)
	desc := fs.String("desc", "", "what the command does, for the reviewer")
	reason := fs.String("reason", "", "why it was staged, e.g. sandbox-blocked")
	wait := fs.Bool("wait", false, "block until the item is run, rejected or expires")
	files := map[string]string{}
	fs.Func("file", "attach name=path; refer to it in argv as {file:name}", func(v string) error {
		name, path, ok := strings.Cut(v, "=")
		if !ok {
			return errors.New("want name=path")
		}
		files[name] = path
		return nil
	})
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *desc == "" || fs.NArg() == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	sid := os.Getenv("HANDOFF_SESSION_ID")
	if sid == "" {
		sid = os.Getenv("CLAUDE_CODE_SESSION_ID") // set by Claude Code in its Bash tool
	}
	if sid == "" {
		fmt.Fprintln(os.Stderr, "handoff: no session id (CLAUDE_CODE_SESSION_ID or HANDOFF_SESSION_ID); run this from Claude Code")
		return 1
	}
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "handoff:", err)
		return 1
	}
	it, err := q.Add(queue.Item{Description: *desc, Reason: *reason, Argv: fs.Args(), Cwd: cwd, SessionID: sid}, files)
	if err != nil {
		fmt.Fprintln(os.Stderr, "handoff:", err)
		return 1
	}
	if !*wait {
		fmt.Println(it.ID)
		return 0
	}
	fmt.Fprintf(os.Stderr, "staged %s; waiting for review\n", it.ID)
	res, err := q.Wait(context.Background(), it, time.Second)
	if err != nil {
		fmt.Fprintln(os.Stderr, "handoff:", err)
		return 1
	}
	b, _ := json.MarshalIndent(res, "", "  ")
	fmt.Println(string(b))
	if res.State != queue.Succeeded {
		return 1
	}
	return 0
}
