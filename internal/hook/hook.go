// Package hook handles the Claude Code SessionStart and UserPromptSubmit hooks.
//
// Both must print nothing to stdout: UserPromptSubmit stdout is injected into Claude's context.
package hook

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

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
	// WriteSession validates the id, which also keeps it safe to put in the shell env file.
	if err := q.WriteSession(queue.Session{SessionID: in.SessionID, Cwd: in.Cwd, TranscriptPath: in.TranscriptPath, Source: in.Source, Started: time.Now()}); err != nil {
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

func Prompt(q *queue.Queue, r io.Reader) error {
	in, err := read(r)
	if err != nil {
		return err
	}
	return q.AddPrompt(queue.Prompt{TS: time.Now(), SessionID: in.SessionID, Prompt: in.Prompt})
}
