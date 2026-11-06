// Package hook handles the Claude Code SessionStart and UserPromptSubmit hooks.
//
// Both must print nothing to stdout: UserPromptSubmit stdout is injected into Claude's context. The one exception is
// /handoff:claude-sessions, which Prompt answers with a JSON block decision: neither the command nor the answer
// reaches Claude, and it runs here, outside the sandbox.
package hook

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nicholas1513/handoff/internal/claudesessions"
	"github.com/nicholas1513/handoff/internal/queue"
)

type input struct {
	SessionID      string `json:"session_id"`
	Cwd            string `json:"cwd"`
	TranscriptPath string `json:"transcript_path"`
	Source         string `json:"source"`
	Prompt         string `json:"prompt"`
}

func read(r io.Reader) (input, error) {
	var in input
	err := json.NewDecoder(r).Decode(&in)
	return in, err
}

// SessionStart records the session and exports HANDOFF_SESSION_ID to later Bash commands.
func SessionStart(q *queue.Queue, r io.Reader, envFile string) error {
	in, err := read(r)
	if err != nil {
		return err
	}
	// The sandbox may write only inside queue/, so it can't create the directory itself.
	if err := os.MkdirAll(filepath.Join(q.Root, "queue"), 0o700); err != nil {
		return err
	}
	s := queue.Session{SessionID: in.SessionID, Cwd: in.Cwd, TranscriptPath: in.TranscriptPath, Source: in.Source, Started: time.Now()}
	s.ClaudePID, s.ClaudeStart, _ = claudesessions.Owner(os.Getppid()) // none when Claude has no terminal: nothing to restore
	// WriteSession validates the id, which also keeps it safe to put in the shell env file.
	if err := q.WriteSession(s); err != nil {
		return err
	}
	if envFile == "" { // optional: add also reads CLAUDE_CODE_SESSION_ID
		return nil
	}
	f, err := os.OpenFile(envFile, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(f, "export HANDOFF_SESSION_ID=%s\n", in.SessionID)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// runSessions answers /handoff:claude-sessions. Tests replace it: the real one talks to Ghostty.
var runSessions = claudesessions.Command

// sessionsCommand is the argument of /handoff:claude-sessions, also typed without the plugin's name, if p is that.
func sessionsCommand(p string) (string, bool) {
	p = strings.TrimSpace(p)
	for _, name := range []string{"/handoff:claude-sessions", "/claude-sessions"} {
		if p == name {
			return "", true
		}
		if arg, ok := strings.CutPrefix(p, name+" "); ok {
			return strings.TrimSpace(arg), true
		}
	}
	return "", false
}

func Prompt(q *queue.Queue, r io.Reader, w io.Writer) error {
	in, err := read(r)
	if err != nil {
		return err
	}
	if arg, ok := sessionsCommand(in.Prompt); ok {
		return json.NewEncoder(w).Encode(map[string]string{"decision": "block", "reason": runSessions(q, arg, time.Now())})
	}
	// Background-task notifications come through this hook too; they aren't the human's words.
	// shortcut: matched by prefix; revisit if Claude Code adds other injected message kinds.
	if strings.HasPrefix(strings.TrimSpace(in.Prompt), "<task-notification>") {
		return nil
	}
	return q.AddPrompt(queue.Prompt{TS: time.Now(), SessionID: in.SessionID, Prompt: in.Prompt})
}
