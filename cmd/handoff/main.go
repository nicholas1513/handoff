package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
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
	case args[0] == "hook" && len(args) == 2 && (args[1] == "session-start" || args[1] == "prompt"):
		err = runHook(q, args[1])
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fail(err)
	}
}

func runHook(q *queue.Queue, event string) error {
	in, err := io.ReadAll(os.Stdin)
	if err == nil && event == "session-start" {
		err = hook.SessionStart(q, bytes.NewReader(in), os.Getenv("CLAUDE_ENV_FILE"))
	} else if err == nil {
		err = hook.Prompt(q, bytes.NewReader(in))
	}
	logHook(q.Root, event, in, err)
	return err
}

// logHook appends one line per hook call to <root>/hook.log, so "the hook didn't run" can be told
// apart from "it ran and failed". On failure it lists the input's keys, never its values (prompts).
// shortcut: the log grows one line per prompt; rotate it when gc exists (v2).
func logHook(root, event string, in []byte, err error) {
	line := fmt.Sprintf("%s %s ok env_file=%t", time.Now().Format(time.RFC3339), event, os.Getenv("CLAUDE_ENV_FILE") != "")
	if err != nil {
		var m map[string]any
		json.Unmarshal(in, &m)
		line = fmt.Sprintf("%s %s error: %v keys=%v env_file=%t", time.Now().Format(time.RFC3339), event, err, slices.Sorted(maps.Keys(m)), os.Getenv("CLAUDE_ENV_FILE") != "")
	}
	if os.MkdirAll(root, 0o700) != nil {
		return
	}
	if f, ferr := os.OpenFile(filepath.Join(root, "hook.log"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600); ferr == nil {
		fmt.Fprintln(f, line)
		f.Close()
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
