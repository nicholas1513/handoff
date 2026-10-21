package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Sessions and prompts are written only by the hooks, which run outside the sandbox. They are
// the provenance the viewer trusts, so they live outside queue/.

// KeepPrompts bounds how many prompts per session sit on disk.
const KeepPrompts = 20

type Session struct {
	SessionID      string    `json:"session_id"`
	Cwd            string    `json:"cwd"`
	TranscriptPath string    `json:"transcript_path"`
	Source         string    `json:"source"`
	Started        time.Time `json:"started"`
}

type Prompt struct {
	TS        time.Time `json:"ts"`
	SessionID string    `json:"session_id"`
	Prompt    string    `json:"prompt"`
}

func (q *Queue) sessionDir(sid string) (string, error) {
	if !safeName.MatchString(sid) {
		return "", fmt.Errorf("bad session id %q", sid)
	}
	return filepath.Join(q.Root, "sessions", sid), nil
}

func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

func (q *Queue) WriteSession(s Session) error {
	dir, err := q.sessionDir(s.SessionID)
	if err != nil {
		return err
	}
	return writeJSON(filepath.Join(dir, "session.json"), s)
}

func (q *Queue) ReadSession(sid string) (Session, error) {
	var s Session
	dir, err := q.sessionDir(sid)
	if err != nil {
		return s, err
	}
	b, err := os.ReadFile(filepath.Join(dir, "session.json"))
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal(b, &s)
}

// AddPrompt saves p and deletes all but the newest KeepPrompts for its session.
func (q *Queue) AddPrompt(p Prompt) error {
	dir, err := q.sessionDir(p.SessionID)
	if err != nil {
		return err
	}
	dir = filepath.Join(dir, "prompts")
	if err := writeJSON(filepath.Join(dir, fmt.Sprintf("%020d.json", p.TS.UnixNano())), p); err != nil {
		return err
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for i := 0; i < len(ents)-KeepPrompts; i++ { // sorted oldest first
		os.Remove(filepath.Join(dir, ents[i].Name()))
	}
	return nil
}

// PromptBefore returns the newest prompt at or before t: the one the agent was working on.
func (q *Queue) PromptBefore(sid string, t time.Time) (Prompt, error) {
	var p Prompt
	dir, err := q.sessionDir(sid)
	if err != nil {
		return p, err
	}
	ents, err := os.ReadDir(filepath.Join(dir, "prompts"))
	if err != nil {
		return p, err
	}
	cutoff := fmt.Sprintf("%020d.json", t.UnixNano())
	for i := len(ents) - 1; i >= 0; i-- {
		if ents[i].Name() > cutoff {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, "prompts", ents[i].Name()))
		if err != nil {
			return p, err
		}
		return p, json.Unmarshal(b, &p)
	}
	return p, errors.New("no prompt recorded before this item")
}

// Wait polls until it has a result, or returns a synthetic expired result once it expires.
func (q *Queue) Wait(ctx context.Context, it Item, every time.Duration) (Result, error) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		if r, err := q.ReadResult(it); err == nil {
			return r, nil
		}
		if time.Now().After(it.Expires) {
			return Result{State: Expired, ExitCode: -1}, nil
		}
		select {
		case <-ctx.Done():
			return Result{}, ctx.Err()
		case <-tick.C:
		}
	}
}
